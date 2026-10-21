package koh

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/cyberkiller/api/internal/db"
)

// standing is one player's totals within a scope (one arena, or all arenas).
type standing struct {
	PlayerID          uuid.UUID
	Name              string
	Disabled          bool
	Total, Tick, Flag Units
	Last              *time.Time
	Held              int
	Rank              int // 0 = unranked (disabled or zero score)

	// The same totals as of just before the scope's latest completed tick:
	// the baseline for rank_delta / score_delta.
	BaseTotal Units
	BaseLast  *time.Time
	BaseRank  int
}

type standings struct {
	List        []*standing // ranked players first (by Rank), then unranked
	ByPlayer    map[uuid.UUID]*standing
	Held        map[uuid.UUID]int // includes holders with no score records yet
	HasBaseline bool
}

// computeStandings ranks a scope. arenaID nil = all arenas (historical total).
// Ranking: total desc, then earlier last_score_time, then player_name; disabled
// and zero-score players are not ranked.
func computeStandings(ctx context.Context, arenaID *int64) (*standings, error) {
	var cutoff *time.Time
	if err := db.Pool.QueryRow(ctx, `
		SELECT MAX(started_at) FROM koh_ticks
		WHERE finished_at IS NOT NULL AND ($1::bigint IS NULL OR arena_id=$1)
	`, arenaID).Scan(&cutoff); err != nil {
		return nil, err
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT p.id, p.handle, p.banned,
			(COALESCE(SUM(r.score),0)*10000)::bigint,
			(COALESCE(SUM(r.score) FILTER (WHERE r.source_type='tick'),0)*10000)::bigint,
			(COALESCE(SUM(r.score) FILTER (WHERE r.source_type='flag'),0)*10000)::bigint,
			MAX(r.created_at),
			(COALESCE(SUM(r.score) FILTER (WHERE r.created_at < $2),0)*10000)::bigint,
			MAX(r.created_at) FILTER (WHERE r.created_at < $2)
		FROM koh_score_records r JOIN players p ON p.id = r.player_id
		WHERE ($1::bigint IS NULL OR r.arena_id = $1)
		GROUP BY p.id, p.handle, p.banned
	`, arenaID, cutoff)
	if err != nil {
		return nil, err
	}
	s := &standings{ByPlayer: map[uuid.UUID]*standing{}, HasBaseline: cutoff != nil}
	for rows.Next() {
		st := &standing{}
		var total, tick, flag, base int64
		if err := rows.Scan(&st.PlayerID, &st.Name, &st.Disabled, &total, &tick, &flag, &st.Last, &base, &st.BaseLast); err != nil {
			rows.Close()
			return nil, err
		}
		st.Total, st.Tick, st.Flag, st.BaseTotal = Units(total), Units(tick), Units(flag), Units(base)
		s.List = append(s.List, st)
		s.ByPlayer[st.PlayerID] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	held, err := heldCounts(ctx, arenaID)
	if err != nil {
		return nil, err
	}
	s.Held = held
	for pid, n := range held {
		if st := s.ByPlayer[pid]; st != nil {
			st.Held = n
		}
	}

	assignRanks(s.List,
		func(st *standing) (Units, *time.Time) { return st.BaseTotal, st.BaseLast },
		func(st *standing, r int) { st.BaseRank = r })
	assignRanks(s.List,
		func(st *standing) (Units, *time.Time) { return st.Total, st.Last },
		func(st *standing, r int) { st.Rank = r })
	return s, nil
}

// assignRanks sorts list by the given totals and numbers the eligible players
// 1..n. It leaves list in that order, so call the live ranking last.
func assignRanks(list []*standing, key func(*standing) (Units, *time.Time), set func(*standing, int)) {
	eligible := func(st *standing) bool { t, _ := key(st); return !st.Disabled && t > 0 }
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ea, eb := eligible(a), eligible(b); ea != eb {
			return ea
		}
		ta, la := key(a)
		tb, lb := key(b)
		if ta != tb {
			return ta > tb
		}
		if la != nil && lb != nil && !la.Equal(*lb) {
			return la.Before(*lb)
		}
		return a.Name < b.Name
	})
	rank := 0
	for _, st := range list {
		if eligible(st) {
			rank++
			set(st, rank)
		} else {
			set(st, 0)
		}
	}
}

// heldCounts is how many hills each player holds after the latest tick. For
// the all-arena scope this is final holdings of finished arenas plus live
// holdings of running ones (finished arenas keep their last king).
func heldCounts(ctx context.Context, arenaID *int64) (map[uuid.UUID]int, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT h.current_king_id, COUNT(*)
		FROM koh_hills h JOIN koh_arenas a ON a.id = h.arena_id
		WHERE h.current_king_id IS NOT NULL AND a.status <> 'pending'
		  AND ($1::bigint IS NULL OR h.arena_id = $1)
		GROUP BY h.current_king_id
	`, arenaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var pid uuid.UUID
		var n int
		if err := rows.Scan(&pid, &n); err != nil {
			return nil, err
		}
		out[pid] = n
	}
	return out, rows.Err()
}

func intPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
