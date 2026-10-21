package koh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/cyberkiller/api/internal/db"
)

// BasePath is where the agent API is mounted; KOH_BASE_URL handed to agents
// is http(s)://<api-host><BasePath>. Override with KOH_API_PREFIX.
func BasePath() string {
	p := strings.TrimRight(strings.TrimSpace(os.Getenv("KOH_API_PREFIX")), "/")
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/koh/v1"
	}
	return p
}

// Router is the agent API. It is mounted beside (not inside) the hub router
// so every response - including 404/405/429/500 - uses the agent envelope.
func Router(clientIP func(*http.Request) string) http.Handler {
	r := chi.NewRouter()
	r.Use(requestMeta(clientIP))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { notFound(w, "route not found") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusMethodNotAllowed, CodeNotFound, "method not allowed")
	})
	r.Group(func(g chi.Router) {
		g.Use(requireToken)
		g.Get("/me", limited("me", handleMe))
		g.Get("/arenas", limited("arenas", handleArenas))
		g.Get("/arenas/{arena_code}", limited("arena_detail", handleArenaDetail))
		g.Get("/me/scores", limited("me_scores", handleMyScores))
		g.Get("/rankings", limited("rankings", handleRankings))
		g.Post("/flag_submit", limited("flag_submit", handleFlagSubmit))
		// The quick-start section of the spec calls it POST /flags; same
		// handler and same rate-limit bucket.
		g.Post("/flags", limited("flag_submit", handleFlagSubmit))
	})
	return r
}

type arena struct {
	ID          int64
	Code        string
	Description string
	Status      string
	Entrypoints []string
}

// loadArena returns (nil, nil) when no arena has that code.
func loadArena(ctx context.Context, code string) (*arena, error) {
	a := &arena{}
	var eps []byte
	err := db.Pool.QueryRow(ctx, `
		SELECT id, arena_code, description, status, entrypoints FROM koh_arenas WHERE arena_code=$1
	`, code).Scan(&a.ID, &a.Code, &a.Description, &a.Status, &eps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(eps, &a.Entrypoints)
	if a.Entrypoints == nil {
		a.Entrypoints = []string{}
	}
	return a, nil
}

// GET /me
func handleMe(w http.ResponseWriter, r *http.Request) {
	p := playerFrom(r)
	status := "active"
	if p.Disabled {
		status = "disabled"
	}
	ok(w, map[string]any{
		"player_name": p.Name,
		"player_flag": p.Flag,
		"status":      status,
		"server_time": isoTime(time.Now()),
	})
}

// GET /arenas - running arenas only; none running is an empty list, not an error.
func handleArenas(w http.ResponseWriter, r *http.Request) {
	pg, ferr := pageParams(r.URL.Query())
	if ferr != nil {
		paramError(w, ferr.field, ferr.reason)
		return
	}
	ctx := r.Context()
	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM koh_arenas WHERE status='running'`).Scan(&total); err != nil {
		internalError(w, "arenas count", err)
		return
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT arena_code, entrypoints, description FROM koh_arenas WHERE status='running'
		ORDER BY started_at, id LIMIT $1 OFFSET $2
	`, pg.Size, pg.offset())
	if err != nil {
		internalError(w, "arenas list", err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var code, desc string
		var eps []byte
		if err := rows.Scan(&code, &eps, &desc); err != nil {
			internalError(w, "arenas scan", err)
			return
		}
		list := []string{}
		json.Unmarshal(eps, &list)
		items = append(items, map[string]any{"arena_code": code, "entrypoints": list, "description": desc})
	}
	ok(w, map[string]any{"items": items, "pagination": pagination(pg, total)})
}

// GET /arenas/{arena_code} - founded hills (claimed at least once), their
// current king, and ownership history newest first.
func handleArenaDetail(w http.ResponseWriter, r *http.Request) {
	includeChanges, ferr := boolParam(r.URL.Query(), "include_changes", true)
	if ferr != nil {
		paramError(w, ferr.field, ferr.reason)
		return
	}
	ctx := r.Context()
	a, err := loadArena(ctx, chi.URLParam(r, "arena_code"))
	if err != nil {
		internalError(w, "arena load", err)
		return
	}
	if a == nil || a.Status != "running" {
		notFound(w, "arena not found or not running")
		return
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT h.id, h.hill_code, p.handle
		FROM koh_hills h LEFT JOIN players p ON p.id = h.current_king_id
		WHERE h.arena_id=$1 AND EXISTS (SELECT 1 FROM koh_hill_changes c WHERE c.hill_id=h.id)
		ORDER BY h.hill_code
	`, a.ID)
	if err != nil {
		internalError(w, "arena hills", err)
		return
	}
	type hill struct {
		HillCode    string           `json:"hill_code"`
		CurrentKing *string          `json:"current_king"`
		Changes     []map[string]any `json:"change_records"`
	}
	hills := []*hill{}
	byID := map[int64]*hill{}
	for rows.Next() {
		var id int64
		h := &hill{Changes: []map[string]any{}}
		if err := rows.Scan(&id, &h.HillCode, &h.CurrentKing); err != nil {
			rows.Close()
			internalError(w, "arena hills scan", err)
			return
		}
		hills = append(hills, h)
		byID[id] = h
	}
	rows.Close()

	if includeChanges && len(hills) > 0 {
		crows, err := db.Pool.Query(ctx, `
			SELECT hill_id, created_at, tick_time, before_name, after_name, change_type, description
			FROM koh_hill_changes WHERE arena_id=$1 ORDER BY created_at DESC, id DESC
		`, a.ID)
		if err != nil {
			internalError(w, "arena changes", err)
			return
		}
		for crows.Next() {
			var hillID int64
			var at time.Time
			var tickTime *time.Time
			var before, after *string
			var typ, desc string
			if err := crows.Scan(&hillID, &at, &tickTime, &before, &after, &typ, &desc); err != nil {
				crows.Close()
				internalError(w, "arena changes scan", err)
				return
			}
			if h := byID[hillID]; h != nil {
				h.Changes = append(h.Changes, map[string]any{
					"time": isoTime(at), "tick_time": isoTimePtr(tickTime),
					"before_king": before, "after_king": after,
					"change_type": typ, "description": desc,
				})
			}
		}
		crows.Close()
	}

	ok(w, map[string]any{
		"arena": map[string]any{
			"arena_code": a.Code, "status": a.Status,
			"entrypoints": a.Entrypoints, "description": a.Description,
		},
		"founded_hills": hills,
	})
}

// scopeArena resolves an optional arena_code query param. Unknown codes are
// 404; finished arenas are allowed (historical results stay queryable).
func scopeArena(w http.ResponseWriter, r *http.Request) (a *arena, done bool) {
	code := r.URL.Query().Get("arena_code")
	if code == "" {
		return nil, false
	}
	a, err := loadArena(r.Context(), code)
	if err != nil {
		internalError(w, "arena load", err)
		return nil, true
	}
	if a == nil {
		notFound(w, "arena not found")
		return nil, true
	}
	return a, false
}

// GET /me/scores - summary (depends only on arena_code) + filtered ledger.
func handleMyScores(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pg, ferr := pageParams(q)
	var from, to *time.Time
	if ferr == nil {
		from, ferr = timeParam(q, "from_time")
	}
	if ferr == nil {
		to, ferr = timeParam(q, "to_time")
	}
	if ferr == nil && from != nil && to != nil && from.After(*to) {
		ferr = &fieldErr{"from_time", "must not be after to_time"}
	}
	source := q.Get("source_type")
	if ferr == nil && source != "" && source != "tick" && source != "flag" {
		ferr = &fieldErr{"source_type", "must be one of: tick, flag"}
	}
	if ferr != nil {
		paramError(w, ferr.field, ferr.reason)
		return
	}
	a, done := scopeArena(w, r)
	if done {
		return
	}
	var arenaID *int64
	if a != nil {
		arenaID = &a.ID
	}
	ctx := r.Context()
	p := playerFrom(r)

	st, err := computeStandings(ctx, arenaID)
	if err != nil {
		internalError(w, "standings", err)
		return
	}
	summary := map[string]any{
		"player_name": p.Name, "total_score": Units(0).String(), "tick_score": Units(0).String(),
		"flag_score": Units(0).String(), "rank": nil, "held_hill_count": st.Held[p.ID], "last_score_time": nil,
	}
	if me := st.ByPlayer[p.ID]; me != nil {
		summary["total_score"] = me.Total.String()
		summary["tick_score"] = me.Tick.String()
		summary["flag_score"] = me.Flag.String()
		summary["rank"] = intPtr(me.Rank)
		summary["last_score_time"] = isoTimePtr(me.Last)
	}

	where := []string{"r.player_id = $1"}
	args := []any{p.ID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if arenaID != nil {
		add("r.arena_id = $%d", *arenaID)
	}
	if h := q.Get("hill_code"); h != "" {
		add("r.hill_code = $%d", h)
	}
	if from != nil {
		add("r.created_at >= $%d", *from)
	}
	if to != nil {
		add("r.created_at <= $%d", *to)
	}
	if source != "" {
		add("r.source_type = $%d", source)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM koh_score_records r WHERE `+cond, args...).Scan(&total); err != nil {
		internalError(w, "scores count", err)
		return
	}
	args = append(args, pg.Size, pg.offset())
	rows, err := db.Pool.Query(ctx, fmt.Sprintf(`
		SELECT r.id, r.created_at, a.arena_code, r.hill_code, r.tick_time, r.source_type,
			r.message, (r.score*10000)::bigint, r.detail
		FROM koh_score_records r JOIN koh_arenas a ON a.id = r.arena_id
		WHERE %s
		ORDER BY r.created_at DESC, (r.source_type = 'flag') DESC, r.id DESC
		LIMIT $%d OFFSET $%d
	`, cond, len(args)-1, len(args)), args...)
	if err != nil {
		internalError(w, "scores list", err)
		return
	}
	defer rows.Close()
	records := []map[string]any{}
	for rows.Next() {
		var id, score int64
		var at time.Time
		var tickTime *time.Time
		var arenaCode, hillCode, src, msg string
		var detail json.RawMessage
		if err := rows.Scan(&id, &at, &arenaCode, &hillCode, &tickTime, &src, &msg, &score, &detail); err != nil {
			internalError(w, "scores scan", err)
			return
		}
		records = append(records, map[string]any{
			"time": isoTime(at), "arena_code": arenaCode, "hill_code": hillCode,
			"tick_time": isoTimePtr(tickTime), "source_type": src,
			"record_key": fmt.Sprintf("%s:%d", src, id), "message": msg,
			"score": Units(score).String(), "score_detail": detail,
		})
	}
	ok(w, map[string]any{"summary": summary, "records": records, "pagination": pagination(pg, total)})
}

// GET /rankings - live leaderboard for one arena, or the all-arena total.
func handleRankings(w http.ResponseWriter, r *http.Request) {
	limit, ferr := intParam(r.URL.Query(), "limit", 100, 1, 500)
	if ferr != nil {
		paramError(w, ferr.field, ferr.reason)
		return
	}
	a, done := scopeArena(w, r)
	if done {
		return
	}
	var arenaID *int64
	var arenaCode *string
	scope := "overall"
	if a != nil {
		arenaID, arenaCode, scope = &a.ID, &a.Code, "arena"
	}
	st, err := computeStandings(r.Context(), arenaID)
	if err != nil {
		internalError(w, "standings", err)
		return
	}
	items := []map[string]any{}
	for _, s := range st.List {
		if s.Rank == 0 || len(items) >= limit {
			break
		}
		var rankDelta *int
		var scoreDelta *string
		if st.HasBaseline {
			d := (s.Total - s.BaseTotal).String()
			scoreDelta = &d
			if s.BaseRank > 0 {
				rd := s.BaseRank - s.Rank
				rankDelta = &rd
			}
		}
		items = append(items, map[string]any{
			"rank": s.Rank, "rank_delta": rankDelta, "player_name": s.Name,
			"total_score": s.Total.String(), "tick_score": s.Tick.String(), "flag_score": s.Flag.String(),
			"score_delta": scoreDelta, "held_hill_count": s.Held, "last_score_time": isoTimePtr(s.Last),
		})
	}
	var selfRank *int
	if me := st.ByPlayer[playerFrom(r).ID]; me != nil {
		selfRank = intPtr(me.Rank)
	}
	ok(w, map[string]any{
		"scope": scope, "arena_code": arenaCode, "updated_at": isoTime(time.Now()),
		"items": items, "self_rank": selfRank,
	})
}

const maxSubmitBody = 16 << 10 // a 1024-char flag fully \u-escaped still fits

// POST /flag_submit {"arena_code","flag"}. Idempotent: a repeat of a scored
// answer is correct:true, score:0. Error messages never echo the answer.
func handleFlagSubmit(w http.ResponseWriter, r *http.Request) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		paramError(w, "body", "Content-Type must be application/json")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubmitBody))
	if err != nil {
		paramError(w, "body", "request body too large")
		return
	}
	if !utf8.Valid(raw) {
		fail(w, http.StatusBadRequest, CodeParam, "body: must be valid UTF-8")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		paramError(w, "body", "must be a JSON object")
		return
	}
	for k := range fields {
		if k != "arena_code" && k != "flag" {
			paramError(w, k, "extra fields not permitted")
			return
		}
	}
	str := func(name string) (string, bool) {
		v, present := fields[name]
		if !present {
			paramError(w, name, "field required")
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			paramError(w, name, "must be a string")
			return "", false
		}
		return s, true
	}
	code, okCode := str("arena_code")
	if !okCode {
		return
	}
	flag, okFlag := str("flag")
	if !okFlag {
		return
	}
	if n := utf8.RuneCountInString(code); n < 1 || n > 64 {
		paramError(w, "arena_code", "length must be between 1 and 64 characters")
		return
	}
	if n := utf8.RuneCountInString(flag); n < 1 || n > 1024 {
		paramError(w, "flag", "length must be between 1 and 1024 characters")
		return
	}
	if len(flag) > 1024 {
		fail(w, http.StatusBadRequest, CodeParam, "flag: UTF-8 encoding must not exceed 1024 bytes")
		return
	}
	if strings.IndexFunc(flag, unicode.IsControl) >= 0 {
		fail(w, http.StatusBadRequest, CodeParam, "flag: must not contain control characters")
		return
	}

	ctx := r.Context()
	a, err := loadArena(ctx, code)
	if err != nil {
		internalError(w, "arena load", err)
		return
	}
	if a == nil || a.Status != "running" {
		notFound(w, "arena not found or not running")
		return
	}

	var flagID, hillID int64
	var hillCode string
	var score int64
	var enabled bool
	err = db.Pool.QueryRow(ctx, `
		SELECT f.id, f.hill_id, h.hill_code, (f.score*10000)::bigint, f.enabled
		FROM koh_flags f JOIN koh_hills h ON h.id = f.hill_id
		WHERE f.arena_id=$1 AND f.flag_value=$2
	`, a.ID, flag).Scan(&flagID, &hillID, &hillCode, &score, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		ok(w, map[string]any{"correct": false, "score": 0})
		return
	}
	if err != nil {
		internalError(w, "flag lookup", err)
		return
	}
	if !enabled {
		ok(w, map[string]any{"correct": true, "score": 0})
		return
	}

	awarded, err := awardFlag(ctx, a.ID, flagID, hillID, hillCode, playerFrom(r), Units(score))
	if err != nil {
		internalError(w, "flag award", err)
		return
	}
	if !awarded {
		ok(w, map[string]any{"correct": true, "score": 0})
		return
	}
	ok(w, map[string]any{"correct": true, "score": Units(score).Float()})
}

// awardFlag records a first solve and its score record atomically; false if
// this player already scored this flag.
func awardFlag(ctx context.Context, arenaID, flagID, hillID int64, hillCode string, p *Player, score Units) (bool, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		INSERT INTO koh_flag_solves (flag_id, player_id) VALUES ($1,$2) ON CONFLICT DO NOTHING
	`, flagID, p.ID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	detail, _ := json.Marshal(map[string]string{"fixed_score": score.String()})
	_, err = tx.Exec(ctx, `
		INSERT INTO koh_score_records (source_type, arena_id, hill_id, hill_code, player_id, flag_id, score, detail, message)
		VALUES ('flag', $1, $2, $3, $4, $5, ($6::bigint)::numeric / 10000, $7, $8)
	`, arenaID, hillID, hillCode, p.ID, flagID, int64(score), detail,
		fmt.Sprintf("%s 提交 %s 的 Flag，获得 %s 分", p.Name, hillCode, score))
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
