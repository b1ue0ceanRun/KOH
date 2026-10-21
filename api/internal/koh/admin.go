package koh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cyberkiller/api/internal/db"
)

// Admin handlers live under /admin/koh behind the existing admin auth and use
// the admin panel's plain JSON style ({"error": ...}), not the agent envelope.

func adminJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func adminErr(w http.ResponseWriter, code int, msg string) {
	adminJSON(w, code, map[string]string{"error": msg})
}

func audit(ctx context.Context, action, detail string) {
	db.Pool.Exec(ctx, `INSERT INTO audit_log (actor, action, detail) VALUES ('admin',$1,$2)`, action, detail)
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// scoreInput accepts a JSON number (2.5) or string ("2.5000").
type scoreInput struct {
	set bool
	v   Units
}

func (s *scoreInput) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
	}
	u, err := ParseUnits(raw)
	if err != nil {
		return err
	}
	s.set, s.v = true, u
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		adminErr(w, 400, "invalid json: "+err.Error())
		return false
	}
	return true
}

// AdminRoutes registers the operator endpoints on an already-authenticated router.
func AdminRoutes(ar chi.Router) {
	ar.Get("/koh/arenas", adminListArenas)
	ar.Post("/koh/arenas", adminCreateArena)
	ar.Put("/koh/arenas/{code}", adminUpdateArena)
	ar.Delete("/koh/arenas/{code}", adminDeleteArena)
	ar.Post("/koh/arenas/{code}/start", adminStartArena)
	ar.Post("/koh/arenas/{code}/finish", adminFinishArena)
	ar.Post("/koh/arenas/{code}/hills", adminCreateHill)
	ar.Put("/koh/hills/{hill}", adminUpdateHill)
	ar.Delete("/koh/hills/{hill}", adminDeleteHill)
	ar.Post("/koh/hills/{hill}/flags", adminCreateFlag)
	ar.Put("/koh/flags/{id}", adminUpdateFlag)
	ar.Delete("/koh/flags/{id}", adminDeleteFlag)
	ar.Get("/koh/players", adminListPlayers)
	ar.Post("/koh/players/{handle}/rotate", adminRotatePlayer)
}

func adminListArenas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := db.Pool.Query(ctx, `
		SELECT id, arena_code, description, entrypoints, status, tick_seconds, next_tick_at, started_at, ended_at
		FROM koh_arenas ORDER BY created_at DESC
	`)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	type arenaOut struct {
		ID          int64            `json:"-"`
		Code        string           `json:"arena_code"`
		Description string           `json:"description"`
		Entrypoints json.RawMessage  `json:"entrypoints"`
		Status      string           `json:"status"`
		TickSeconds int              `json:"tick_seconds"`
		NextTickAt  *string          `json:"next_tick_at"`
		StartedAt   *string          `json:"started_at"`
		EndedAt     *string          `json:"ended_at"`
		Hills       []map[string]any `json:"hills"`
	}
	var arenas []*arenaOut
	byID := map[int64]*arenaOut{}
	for rows.Next() {
		a := &arenaOut{Hills: []map[string]any{}}
		var next, started, ended *time.Time
		if err := rows.Scan(&a.ID, &a.Code, &a.Description, &a.Entrypoints, &a.Status, &a.TickSeconds, &next, &started, &ended); err != nil {
			rows.Close()
			adminErr(w, 500, err.Error())
			return
		}
		a.NextTickAt, a.StartedAt, a.EndedAt = isoTimePtr(next), isoTimePtr(started), isoTimePtr(ended)
		arenas = append(arenas, a)
		byID[a.ID] = a
	}
	rows.Close()

	flagsByHill := map[int64][]map[string]any{}
	frows, err := db.Pool.Query(ctx, `
		SELECT f.id, f.hill_id, f.flag_value, (f.score*10000)::bigint, f.path, f.enabled,
			(SELECT COUNT(*) FROM koh_flag_solves s WHERE s.flag_id=f.id)
		FROM koh_flags f ORDER BY f.id
	`)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	for frows.Next() {
		var id, hillID, score int64
		var value, path string
		var enabled bool
		var solves int
		if frows.Scan(&id, &hillID, &value, &score, &path, &enabled, &solves) == nil {
			flagsByHill[hillID] = append(flagsByHill[hillID], map[string]any{
				"id": id, "flag": value, "score": Units(score).String(), "path": path, "enabled": enabled, "solves": solves,
			})
		}
	}
	frows.Close()

	hrows, err := db.Pool.Query(ctx, `
		SELECT h.id, h.arena_id, h.hill_code, h.os, (h.base_score*10000)::bigint, h.healthcheck_cmd, h.streak,
			p.handle, h.machine_id::text, k.image_name, host(k.arena_ip), k.status
		FROM koh_hills h
		LEFT JOIN players p ON p.id = h.current_king_id
		LEFT JOIN koth_machines k ON k.id = h.machine_id
		ORDER BY h.hill_code
	`)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	for hrows.Next() {
		var id, arenaID, base int64
		var code, osName, health string
		var streak int
		var king, machineID, image, ip, mstatus *string
		if hrows.Scan(&id, &arenaID, &code, &osName, &base, &health, &streak, &king, &machineID, &image, &ip, &mstatus) != nil {
			continue
		}
		if a := byID[arenaID]; a != nil {
			fl := flagsByHill[id]
			if fl == nil {
				fl = []map[string]any{}
			}
			a.Hills = append(a.Hills, map[string]any{
				"hill_code": code, "os": osName, "base_score": Units(base).String(), "healthcheck_cmd": health,
				"current_king": king, "streak": streak, "machine_id": machineID, "machine_image": image,
				"machine_ip": ip, "machine_status": mstatus, "flags": fl,
			})
		}
	}
	hrows.Close()
	if arenas == nil {
		arenas = []*arenaOut{}
	}
	adminJSON(w, 200, arenas)
}

type arenaBody struct {
	Code        string    `json:"arena_code"`
	Description *string   `json:"description"`
	Entrypoints *[]string `json:"entrypoints"`
	TickSeconds *int      `json:"tick_seconds"`
}

func validArenaFields(b arenaBody) string {
	if b.TickSeconds != nil && (*b.TickSeconds < 10 || *b.TickSeconds > 86400) {
		return "tick_seconds must be between 10 and 86400"
	}
	if b.Entrypoints != nil {
		for _, e := range *b.Entrypoints {
			if strings.TrimSpace(e) == "" || len(e) > 255 {
				return "entrypoints must be non-empty host:port strings"
			}
		}
	}
	return ""
}

func adminCreateArena(w http.ResponseWriter, r *http.Request) {
	var b arenaBody
	if !decode(w, r, &b) {
		return
	}
	if !codeRe.MatchString(b.Code) {
		adminErr(w, 400, "arena_code must be 1-64 chars: letters, digits, _ or -")
		return
	}
	if msg := validArenaFields(b); msg != "" {
		adminErr(w, 400, msg)
		return
	}
	desc, eps, tick := "", []string{}, 300
	if b.Description != nil {
		desc = *b.Description
	}
	if b.Entrypoints != nil {
		eps = *b.Entrypoints
	}
	if b.TickSeconds != nil {
		tick = *b.TickSeconds
	}
	epsJSON, _ := json.Marshal(eps)
	_, err := db.Pool.Exec(r.Context(), `
		INSERT INTO koh_arenas (arena_code, description, entrypoints, tick_seconds) VALUES ($1,$2,$3,$4)
	`, b.Code, desc, epsJSON, tick)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			adminErr(w, 409, "arena_code already exists")
			return
		}
		adminErr(w, 500, err.Error())
		return
	}
	audit(r.Context(), "koh_arena_create", b.Code)
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

func adminUpdateArena(w http.ResponseWriter, r *http.Request) {
	var b arenaBody
	if !decode(w, r, &b) {
		return
	}
	if msg := validArenaFields(b); msg != "" {
		adminErr(w, 400, msg)
		return
	}
	var epsJSON []byte
	if b.Entrypoints != nil {
		epsJSON, _ = json.Marshal(*b.Entrypoints)
	}
	tag, err := db.Pool.Exec(r.Context(), `
		UPDATE koh_arenas SET description=COALESCE($2,description), entrypoints=COALESCE($3,entrypoints),
			tick_seconds=COALESCE($4,tick_seconds)
		WHERE arena_code=$1
	`, chi.URLParam(r, "code"), b.Description, epsJSON, b.TickSeconds)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 404, "arena not found")
		return
	}
	audit(r.Context(), "koh_arena_update", chi.URLParam(r, "code"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

// adminStartArena opens a pending arena: hills start unowned, and the first
// tick lands one full period after the start.
func adminStartArena(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := chi.URLParam(r, "code")
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE koh_arenas SET status='running', started_at=NOW(), ended_at=NULL,
			next_tick_at=NOW() + tick_seconds * INTERVAL '1 second'
		WHERE arena_code=$1 AND status='pending' RETURNING id
	`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		adminErr(w, 409, "arena not found or not pending (only a pending arena can be started)")
		return
	}
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE koh_hills SET current_king_id=NULL, streak=0 WHERE arena_id=$1`, id); err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	audit(ctx, "koh_arena_start", code)
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

// adminFinishArena moves a running arena to settling; the tick loop marks it
// finished as soon as no tick is in flight. Final hill owners are kept.
func adminFinishArena(w http.ResponseWriter, r *http.Request) {
	tag, err := db.Pool.Exec(r.Context(), `
		UPDATE koh_arenas SET status='settling', next_tick_at=NULL WHERE arena_code=$1 AND status='running'
	`, chi.URLParam(r, "code"))
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 409, "arena not found or not running")
		return
	}
	audit(r.Context(), "koh_arena_finish", chi.URLParam(r, "code"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

func adminDeleteArena(w http.ResponseWriter, r *http.Request) {
	tag, err := db.Pool.Exec(r.Context(), `
		DELETE FROM koh_arenas WHERE arena_code=$1 AND status IN ('pending','finished')
	`, chi.URLParam(r, "code"))
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 409, "arena not found, or still running (finish it first)")
		return
	}
	audit(r.Context(), "koh_arena_delete", chi.URLParam(r, "code"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

type hillBody struct {
	HillCode       string     `json:"hill_code"`
	MachineID      *string    `json:"machine_id"`
	ArenaIP        *string    `json:"arena_ip"`
	OS             *string    `json:"os"`
	BaseScore      scoreInput `json:"base_score"`
	HealthcheckCmd *string    `json:"healthcheck_cmd"`
}

// resolveMachine binds by machine UUID or by the live box at an arena IP.
// Returns (nil, "") when neither is given.
func resolveMachine(ctx context.Context, b hillBody) (*uuid.UUID, string) {
	if b.MachineID != nil && *b.MachineID != "" {
		id, err := uuid.Parse(*b.MachineID)
		if err != nil {
			return nil, "machine_id must be a UUID"
		}
		var exists bool
		db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM koth_machines WHERE id=$1)`, id).Scan(&exists)
		if !exists {
			return nil, "machine not found"
		}
		return &id, ""
	}
	if b.ArenaIP != nil && *b.ArenaIP != "" {
		var id uuid.UUID
		err := db.Pool.QueryRow(ctx, `
			SELECT id FROM koth_machines WHERE host(arena_ip)=$1 AND status IN ('active','spinning')
			ORDER BY started_at DESC LIMIT 1
		`, *b.ArenaIP).Scan(&id)
		if err != nil {
			return nil, "no live machine at that arena_ip"
		}
		return &id, ""
	}
	return nil, ""
}

func validOS(o *string) bool { return o == nil || *o == "linux" || *o == "windows" }

func adminCreateHill(w http.ResponseWriter, r *http.Request) {
	var b hillBody
	if !decode(w, r, &b) {
		return
	}
	ctx := r.Context()
	if !codeRe.MatchString(b.HillCode) {
		adminErr(w, 400, "hill_code must be 1-64 chars: letters, digits, _ or -")
		return
	}
	if !validOS(b.OS) {
		adminErr(w, 400, "os must be linux or windows")
		return
	}
	machine, msg := resolveMachine(ctx, b)
	if msg != "" {
		adminErr(w, 400, msg)
		return
	}
	osName, health, base := "linux", "", DefaultBaseScore
	if b.OS != nil {
		osName = *b.OS
	}
	if b.HealthcheckCmd != nil {
		health = *b.HealthcheckCmd
	}
	if b.BaseScore.set {
		base = b.BaseScore.v
	}
	tag, err := db.Pool.Exec(ctx, `
		INSERT INTO koh_hills (arena_id, hill_code, machine_id, os, base_score, healthcheck_cmd)
		SELECT id, $2, $3, $4, ($5::bigint)::numeric / 10000, $6 FROM koh_arenas WHERE arena_code=$1
	`, chi.URLParam(r, "code"), b.HillCode, machine, osName, int64(base), health)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			adminErr(w, 409, "hill_code already exists")
			return
		}
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 404, "arena not found")
		return
	}
	audit(ctx, "koh_hill_create", b.HillCode)
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

func adminUpdateHill(w http.ResponseWriter, r *http.Request) {
	var b hillBody
	if !decode(w, r, &b) {
		return
	}
	ctx := r.Context()
	if !validOS(b.OS) {
		adminErr(w, 400, "os must be linux or windows")
		return
	}
	machine, msg := resolveMachine(ctx, b)
	if msg != "" {
		adminErr(w, 400, msg)
		return
	}
	var base *int64
	if b.BaseScore.set {
		v := int64(b.BaseScore.v)
		base = &v
	}
	tag, err := db.Pool.Exec(ctx, `
		UPDATE koh_hills SET machine_id=COALESCE($2,machine_id), os=COALESCE($3,os),
			base_score=COALESCE(($4::bigint)::numeric / 10000, base_score),
			healthcheck_cmd=COALESCE($5,healthcheck_cmd)
		WHERE hill_code=$1
	`, chi.URLParam(r, "hill"), machine, b.OS, base, b.HealthcheckCmd)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 404, "hill not found")
		return
	}
	audit(ctx, "koh_hill_update", chi.URLParam(r, "hill"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

func adminDeleteHill(w http.ResponseWriter, r *http.Request) {
	tag, err := db.Pool.Exec(r.Context(), `
		DELETE FROM koh_hills h USING koh_arenas a
		WHERE h.arena_id=a.id AND h.hill_code=$1 AND a.status='pending'
	`, chi.URLParam(r, "hill"))
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 409, "hill not found, or its arena already started (scores reference it)")
		return
	}
	audit(r.Context(), "koh_hill_delete", chi.URLParam(r, "hill"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

type flagBody struct {
	Flag    string     `json:"flag"`
	Score   scoreInput `json:"score"`
	Path    string     `json:"path"`
	Plant   bool       `json:"plant"`
	Enabled *bool      `json:"enabled"`
}

// adminCreateFlag registers a scattered flag on a hill. An empty flag gets a
// random flag{...}; with plant=true the value is also written to path inside
// the hill's (Linux) container.
func adminCreateFlag(w http.ResponseWriter, r *http.Request) {
	var b flagBody
	if !decode(w, r, &b) {
		return
	}
	ctx := r.Context()
	if b.Flag == "" {
		buf := make([]byte, 16)
		rand.Read(buf)
		b.Flag = "flag{" + hex.EncodeToString(buf) + "}"
	}
	if len(b.Flag) > 1024 || strings.ContainsFunc(b.Flag, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		adminErr(w, 400, "flag must be at most 1024 bytes with no control characters")
		return
	}
	score := DefaultFlagScore
	if b.Score.set {
		score = b.Score.v
	}
	var arenaID, hillID int64
	var hillOS, container string
	err := db.Pool.QueryRow(ctx, `
		SELECT h.arena_id, h.id, h.os, COALESCE(k.container_id,'')
		FROM koh_hills h LEFT JOIN koth_machines k ON k.id=h.machine_id WHERE h.hill_code=$1
	`, chi.URLParam(r, "hill")).Scan(&arenaID, &hillID, &hillOS, &container)
	if err != nil {
		adminErr(w, 404, "hill not found")
		return
	}
	if b.Plant {
		if b.Path == "" || !strings.HasPrefix(b.Path, "/") {
			adminErr(w, 400, "plant needs an absolute path")
			return
		}
		if hillOS != "linux" || container == "" {
			adminErr(w, 400, "planting needs a live Linux machine bound to the hill")
			return
		}
		if err := plantFile(ctx, container, b.Path, b.Flag); err != nil {
			adminErr(w, 502, "plant failed: "+err.Error())
			return
		}
	}
	var id int64
	err = db.Pool.QueryRow(ctx, `
		INSERT INTO koh_flags (arena_id, hill_id, flag_value, score, path) VALUES ($1,$2,$3,($4::bigint)::numeric / 10000,$5)
		RETURNING id
	`, arenaID, hillID, b.Flag, int64(score), b.Path).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			adminErr(w, 409, "that flag already exists in this arena")
			return
		}
		adminErr(w, 500, err.Error())
		return
	}
	audit(ctx, "koh_flag_create", fmt.Sprintf("hill=%s id=%d", chi.URLParam(r, "hill"), id))
	adminJSON(w, 200, map[string]any{"ok": "true", "id": id, "flag": b.Flag})
}

// plantFile writes value to path in the container as root. The path is passed
// as a positional argument and the value on stdin: nothing is interpolated
// into the shell script.
func plantFile(ctx context.Context, container, path, value string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-u", "0", container,
		"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(value + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func adminUpdateFlag(w http.ResponseWriter, r *http.Request) {
	var b flagBody
	if !decode(w, r, &b) {
		return
	}
	var score *int64
	if b.Score.set {
		v := int64(b.Score.v)
		score = &v
	}
	tag, err := db.Pool.Exec(r.Context(), `
		UPDATE koh_flags SET enabled=COALESCE($2,enabled), score=COALESCE(($3::bigint)::numeric / 10000, score)
		WHERE id=$1
	`, chi.URLParam(r, "id"), b.Enabled, score)
	if err != nil {
		adminErr(w, 400, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 404, "flag not found")
		return
	}
	audit(r.Context(), "koh_flag_update", chi.URLParam(r, "id"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

func adminDeleteFlag(w http.ResponseWriter, r *http.Request) {
	tag, err := db.Pool.Exec(r.Context(), `DELETE FROM koh_flags WHERE id=$1`, chi.URLParam(r, "id"))
	if err != nil {
		adminErr(w, 400, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		adminErr(w, 404, "flag not found")
		return
	}
	audit(r.Context(), "koh_flag_delete", chi.URLParam(r, "id"))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}

// adminListPlayers exposes each player's agent credentials so the operator can
// inject KOH_API_TOKEN into that player's agent container.
func adminListPlayers(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Pool.Query(r.Context(), `
		SELECT handle, COALESCE(api_token,''), COALESCE(player_flag,''), banned FROM players ORDER BY handle
	`)
	if err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var handle, token, flag string
		var banned bool
		if rows.Scan(&handle, &token, &flag, &banned) != nil {
			continue
		}
		status := "active"
		if banned {
			status = "disabled"
		}
		list = append(list, map[string]any{"player_name": handle, "api_token": token, "player_flag": flag, "status": status})
	}
	adminJSON(w, 200, list)
}

func adminRotatePlayer(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Token bool `json:"token"`
		Flag  bool `json:"flag"`
	}
	if !decode(w, r, &b) {
		return
	}
	if !b.Token && !b.Flag {
		adminErr(w, 400, "set token and/or flag to true")
		return
	}
	ctx := r.Context()
	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT id FROM players WHERE handle=$1`, chi.URLParam(r, "handle")).Scan(&id); err != nil {
		adminErr(w, 404, "player not found")
		return
	}
	if err := RotateCredentials(ctx, id, b.Token, b.Flag); err != nil {
		adminErr(w, 500, err.Error())
		return
	}
	audit(ctx, "koh_rotate_credentials", fmt.Sprintf("%s token=%v flag=%v", chi.URLParam(r, "handle"), b.Token, b.Flag))
	adminJSON(w, 200, map[string]string{"ok": "true"})
}
