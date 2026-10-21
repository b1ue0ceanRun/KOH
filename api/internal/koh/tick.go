package koh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cyberkiller/api/internal/db"
)

// KingFile locations. They are fixed by the rules and never returned by the API.
const (
	linuxKingFile   = "/root/king.txt"
	windowsKingFile = `C:\king.txt`
)

const (
	probeTimeout   = 10 * time.Second
	maxKingFileLen = 64 << 10 // anything larger can't be a strip()-ed flag worth reading
)

// StartTicker runs the tick loop: every running arena is scored on its own
// schedule (started_at + n*tick_seconds), and arenas an operator finished are
// moved settling -> finished once no tick is in flight. One goroutine does
// both, so settlement can never interleave with a tick.
func StartTicker(ctx context.Context) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runDue(ctx)
			}
		}
	}()
}

type dueArena struct {
	id       int64
	code     string
	period   time.Duration
	tickTime time.Time
}

func runDue(ctx context.Context) {
	db.Pool.Exec(ctx, `
		UPDATE koh_arenas SET status='finished', ended_at=NOW(), next_tick_at=NULL WHERE status='settling'
	`)
	rows, err := db.Pool.Query(ctx, `
		SELECT id, arena_code, tick_seconds, next_tick_at FROM koh_arenas
		WHERE status='running' AND next_tick_at IS NOT NULL AND next_tick_at <= NOW()
	`)
	if err != nil {
		log.Printf("[koh] due arenas: %v", err)
		return
	}
	var due []dueArena
	for rows.Next() {
		var a dueArena
		var secs int
		if rows.Scan(&a.id, &a.code, &secs, &a.tickTime) == nil {
			a.period = time.Duration(secs) * time.Second
			due = append(due, a)
		}
	}
	rows.Close()
	for _, a := range due {
		if err := runTick(ctx, a); err != nil {
			log.Printf("[koh] arena %s tick %s: %v", a.code, isoTime(a.tickTime), err)
		}
		next := nextTickAfter(a.tickTime, a.period, time.Now())
		db.Pool.Exec(ctx, `UPDATE koh_arenas SET next_tick_at=$2 WHERE id=$1 AND status='running'`, a.id, next)
	}
}

// nextTickAfter keeps ticks on the arena's original grid: if the platform
// fell behind (restart, slow tick) missed slots are skipped, not replayed.
func nextTickAfter(last time.Time, period time.Duration, now time.Time) time.Time {
	next := last.Add(period)
	if !next.After(now) {
		missed := now.Sub(last) / period
		next = last.Add((missed + 1) * period)
	}
	return next
}

type tickHill struct {
	id         int64
	code       string
	os         string
	base       Units
	healthCmd  string
	king       *uuid.UUID
	kingName   string
	streak     int
	container  string
	machineOK  bool
	arenaIP    string
	probePorts []int
}

type holder struct {
	id   uuid.UUID
	name string
}

func runTick(ctx context.Context, a dueArena) error {
	var tickID int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO koh_ticks (arena_id, tick_time) VALUES ($1,$2)
		ON CONFLICT (arena_id, tick_time) DO NOTHING RETURNING id
	`, a.id, a.tickTime).Scan(&tickID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // this slot was already scored
	}
	if err != nil {
		return err
	}

	hills, err := loadTickHills(ctx, a.id)
	if err != nil {
		return err
	}
	flags, err := playerFlags(ctx)
	if err != nil {
		return err
	}

	// Observe every hill concurrently (each probe is bounded by probeTimeout),
	// then apply results one transaction per hill.
	holders := make([]*holder, len(hills))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range hills {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, err := observe(ctx, hills[i])
			if err != nil {
				log.Printf("[koh] arena %s hill %s unowned this tick: %v", a.code, hills[i].code, err)
				return
			}
			if h, found := flags[strings.TrimSpace(content)]; found {
				holders[i] = &h
			}
		}(i)
	}
	wg.Wait()

	for i, h := range hills {
		if err := applyHill(ctx, a, tickID, h, holders[i]); err != nil {
			log.Printf("[koh] arena %s hill %s apply: %v", a.code, h.code, err)
		}
	}
	_, err = db.Pool.Exec(ctx, `UPDATE koh_ticks SET finished_at=NOW() WHERE id=$1`, tickID)
	return err
}

func loadTickHills(ctx context.Context, arenaID int64) ([]tickHill, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT h.id, h.hill_code, h.os, (h.base_score*10000)::bigint, h.healthcheck_cmd,
			h.current_king_id, COALESCE(p.handle,''), h.streak,
			COALESCE(k.container_id,''), COALESCE(k.status = 'active', false),
			COALESCE(host(k.arena_ip),''), COALESCE(ti.ssh_port, 22), COALESCE(ti.web_port, 80)
		FROM koh_hills h
		LEFT JOIN players p ON p.id = h.current_king_id
		LEFT JOIN koth_machines k ON k.id = h.machine_id
		LEFT JOIN target_images ti ON ti.id = k.image_id
		WHERE h.arena_id = $1
		ORDER BY h.id
	`, arenaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tickHill
	for rows.Next() {
		var h tickHill
		var base int64
		var ssh, web int
		if err := rows.Scan(&h.id, &h.code, &h.os, &base, &h.healthCmd, &h.king, &h.kingName, &h.streak,
			&h.container, &h.machineOK, &h.arenaIP, &ssh, &web); err != nil {
			return nil, err
		}
		h.base = Units(base)
		h.probePorts = []int{ssh, web}
		out = append(out, h)
	}
	return out, rows.Err()
}

// playerFlags maps every player_flag to its owner. Disabled players are
// included: the rules let them hold hills and score, just not be ranked.
func playerFlags(ctx context.Context) (map[string]holder, error) {
	rows, err := db.Pool.Query(ctx, `SELECT player_flag, id, handle FROM players WHERE player_flag IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]holder{}
	for rows.Next() {
		var f string
		var h holder
		if err := rows.Scan(&f, &h.id, &h.name); err != nil {
			return nil, err
		}
		out[f] = h
	}
	return out, rows.Err()
}

// observe is the tick's view of a hill; tests swap it for a scripted one.
var observe = observeHill

// observeHill health-checks a hill and returns its raw KingFile content. Any
// error means "no owner this tick": the box is down or its service broken, the
// file is missing or unreadable, or the probe timed out.
func observeHill(ctx context.Context, h tickHill) (string, error) {
	if h.container == "" || !h.machineOK {
		return "", errors.New("no live machine bound to this hill")
	}
	running, err := capped(ctx, 64, "docker", "inspect", "-f", "{{.State.Running}}", h.container)
	if err != nil || strings.TrimSpace(running) != "true" {
		return "", errors.New("container is not running")
	}
	if err := healthCheck(ctx, h); err != nil {
		return "", fmt.Errorf("health check failed: %w", err)
	}
	var args []string
	if h.os == "windows" {
		args = []string{"exec", h.container, "cmd", "/c", "type", windowsKingFile}
	} else {
		// As root: a holder must not be able to blind the platform with chmod 600.
		args = []string{"exec", "-u", "0", h.container, "cat", linuxKingFile}
	}
	content, err := capped(ctx, maxKingFileLen, "docker", args...)
	if err != nil {
		return "", fmt.Errorf("read KingFile: %w", err)
	}
	return content, nil
}

// healthCheck runs the hill's own check command when one is configured (the
// way to assert "the application still works", not just "a port is open");
// otherwise at least one of the box's service ports must accept TCP.
func healthCheck(ctx context.Context, h tickHill) error {
	if h.healthCmd != "" {
		args := []string{"exec", h.container, "sh", "-c", h.healthCmd}
		if h.os == "windows" {
			args = []string{"exec", h.container, "cmd", "/c", h.healthCmd}
		}
		_, err := capped(ctx, 4096, "docker", args...)
		return err
	}
	if h.arenaIP == "" {
		return errors.New("machine has no arena IP")
	}
	for _, port := range h.probePorts {
		if port <= 0 {
			continue
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(h.arenaIP, strconv.Itoa(port)), 3*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
	}
	return fmt.Errorf("no service port answering on %s", h.arenaIP)
}

var errTooLarge = errors.New("output exceeds size limit")

// capped runs a command with probeTimeout and returns at most max bytes of
// stdout; larger output is an error, so a hostile KingFile can't balloon memory.
func capped(ctx context.Context, max int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	_, copyErr := io.Copy(&buf, io.LimitReader(stdout, int64(max)+1))
	if buf.Len() > max {
		cmd.Process.Kill()
		cmd.Wait()
		return "", errTooLarge
	}
	if err := cmd.Wait(); err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	return buf.String(), nil
}

// transition is what one tick does to one hill.
type transition struct {
	change   string // "", "claim", "takeover", "lost"
	streak   int    // holder's consecutive ticks, including this one
	takeover bool
}

// nextState applies the ownership rules: a takeover is only a takeover when
// the hill is taken straight from another player; re-claiming a hill that
// went unowned is a plain claim. Any change of owner resets the streak.
func nextState(prev *uuid.UUID, prevStreak int, now *uuid.UUID) transition {
	switch {
	case now == nil && prev == nil:
		return transition{}
	case now == nil:
		return transition{change: "lost"}
	case prev == nil:
		return transition{change: "claim", streak: 1}
	case *prev == *now:
		return transition{streak: prevStreak + 1}
	default:
		return transition{change: "takeover", streak: 1, takeover: true}
	}
}

func changeDescription(change, before, after string) string {
	switch change {
	case "claim":
		return after + "占领了山头"
	case "takeover":
		return after + "从" + before + "手中抢夺了山头"
	}
	return before + "失去了山头"
}

func applyHill(ctx context.Context, a dueArena, tickID int64, h tickHill, now *holder) error {
	var nowID *uuid.UUID
	nowName := ""
	if now != nil {
		nowID, nowName = &now.id, now.name
	}
	t := nextState(h.king, h.streak, nowID)

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE koh_hills SET current_king_id=$2, streak=$3 WHERE id=$1`, h.id, nowID, t.streak); err != nil {
		return err
	}
	if t.change != "" {
		var before, after *string
		if h.king != nil {
			before = &h.kingName
		}
		if nowID != nil {
			after = &nowName
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO koh_hill_changes (arena_id, hill_id, tick_id, tick_time, before_player_id, after_player_id,
				before_name, after_name, change_type, description)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, a.id, h.id, tickID, a.tickTime, h.king, nowID, before, after, t.change,
			changeDescription(t.change, h.kingName, nowName)); err != nil {
			return err
		}
	}
	if nowID != nil {
		total, awards := TickScore(h.base, t.streak, t.takeover)
		type awardJSON struct {
			Type   string `json:"award_type"`
			Score  string `json:"award_score"`
			Reason string `json:"award_reason"`
		}
		list := []awardJSON{}
		for _, aw := range awards {
			list = append(list, awardJSON{aw.Type, aw.Score.String(), aw.Reason})
		}
		detail, _ := json.Marshal(map[string]any{"base_score": h.base.String(), "awards": list})
		if _, err := tx.Exec(ctx, `
			INSERT INTO koh_score_records (source_type, arena_id, hill_id, hill_code, player_id, tick_id, tick_time, score, detail, message)
			VALUES ('tick', $1, $2, $3, $4, $5, $6, ($7::bigint)::numeric / 10000, $8, $9)
		`, a.id, h.id, h.code, nowID, tickID, a.tickTime, int64(total), detail,
			tickMessage(nowName, h.code, h.base, total, awards)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
