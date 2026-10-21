package koh

// End-to-end tests against a real Postgres. Skipped unless KOH_TEST_DSN points
// at a disposable database (its players/arenas/machines are truncated):
//
//	docker run -d --rm --name koh-pg -e POSTGRES_PASSWORD=t -p 55432:5432 postgres:16-alpine
//	KOH_TEST_DSN='postgres://postgres:t@localhost:55432/postgres?sslmode=disable' go test ./internal/koh/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cyberkiller/api/internal/db"
)

var setupOnce sync.Once

func setupDB(t *testing.T) context.Context {
	t.Helper()
	dsn := os.Getenv("KOH_TEST_DSN")
	if dsn == "" {
		t.Skip("KOH_TEST_DSN not set")
	}
	ctx := context.Background()
	setupOnce.Do(func() {
		os.Setenv("DB_DSN", dsn)
		if err := db.Connect(ctx); err != nil {
			t.Fatalf("connect: %v", err)
		}
		schema, err := os.ReadFile("../db/schema.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, string(schema)); err != nil {
			t.Fatalf("schema: %v", err)
		}
		db.ApplyMigrations(ctx)
	})
	if _, err := db.Pool.Exec(ctx, `TRUNCATE players, koh_arenas, koth_machines CASCADE`); err != nil {
		t.Fatal(err)
	}
	return ctx
}

type world struct {
	t      *testing.T
	ctx    context.Context
	agent  http.Handler
	admin  http.Handler
	tokens map[string]string
	flags  map[string]string
	ids    map[string]uuid.UUID
}

func newWorld(t *testing.T, players ...string) *world {
	ctx := setupDB(t)
	adm := chi.NewRouter()
	adm.Route("/admin", func(r chi.Router) { AdminRoutes(r) })
	w := &world{t: t, ctx: ctx, agent: Router(func(*http.Request) string { return "test" }), admin: adm,
		tokens: map[string]string{}, flags: map[string]string{}, ids: map[string]uuid.UUID{}}
	for _, p := range players {
		var id uuid.UUID
		if err := db.Pool.QueryRow(ctx, `INSERT INTO players (handle) VALUES ($1) RETURNING id`, p).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := EnsureCredentials(ctx, id); err != nil {
			t.Fatal(err)
		}
		var tok, flag string
		db.Pool.QueryRow(ctx, `SELECT api_token, player_flag FROM players WHERE id=$1`, id).Scan(&tok, &flag)
		w.tokens[p], w.flags[p], w.ids[p] = tok, flag, id
	}
	return w
}

type resp struct {
	Status  int
	Header  http.Header
	Code    any
	Message string
	Data    map[string]any
}

func (w *world) call(player, method, path string, body any) resp {
	w.t.Helper()
	var rd *bytes.Reader
	if s, isStr := body.(string); isStr {
		rd = bytes.NewReader([]byte(s))
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if player != "" {
		req.Header.Set("Authorization", "Bearer "+w.tokens[player])
	}
	rec := httptest.NewRecorder()
	w.agent.ServeHTTP(rec, req)
	var env struct {
		Code    any            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		w.t.Fatalf("%s %s: non-JSON body %q", method, path, rec.Body.String())
	}
	return resp{rec.Code, rec.Header(), env.Code, env.Message, env.Data}
}

func (w *world) adminCall(method, path string, body any) map[string]any {
	w.t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	w.admin.ServeHTTP(rec, req)
	if rec.Code != 200 {
		w.t.Fatalf("admin %s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// machine inserts a live koth_machines row for a hill to bind to.
func (w *world) machine(ip string) string {
	var id string
	if err := db.Pool.QueryRow(w.ctx, `
		INSERT INTO koth_machines (image_name, arena_ip, tier, status, container_id, expires_at)
		VALUES ('box', $1::inet, 'easy', 'active', 'fake-'||$1, 'infinity') RETURNING id::text
	`, ip).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// script sets what each hill's KingFile reads as on the next tick; a hill
// missing from the map is "down".
func (w *world) tick(arena string, at time.Time, kingFiles map[string]string) {
	w.t.Helper()
	observe = func(_ context.Context, h tickHill) (string, error) {
		if c, found := kingFiles[h.code]; found {
			return c, nil
		}
		return "", errors.New("down")
	}
	defer func() { observe = observeHill }()
	var a dueArena
	var secs int
	db.Pool.QueryRow(w.ctx, `SELECT id, arena_code, tick_seconds FROM koh_arenas WHERE arena_code=$1`, arena).Scan(&a.id, &a.code, &secs)
	a.period, a.tickTime = time.Duration(secs)*time.Second, at
	if err := runTick(w.ctx, a); err != nil {
		w.t.Fatal(err)
	}
}

func field(t *testing.T, m any, path ...any) any {
	t.Helper()
	cur := m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, isMap := cur.(map[string]any)
			if !isMap {
				t.Fatalf("path %v: %T is not an object", path, cur)
			}
			cur = mm[k]
		case int:
			arr, isArr := cur.([]any)
			if !isArr || k >= len(arr) {
				t.Fatalf("path %v: no index %d in %v", path, k, cur)
			}
			cur = arr[k]
		}
	}
	return cur
}

func TestAgentAPIEndToEnd(t *testing.T) {
	w := newWorld(t, "neo01", "neo02", "neo03")
	m1, m2 := w.machine("10.66.20.51"), w.machine("10.66.20.52")
	w.adminCall("POST", "/admin/koh/arenas", map[string]any{
		"arena_code": "arena-live", "description": "夜间友谊赛", "entrypoints": []string{"10.40.0.11:443"}, "tick_seconds": 300})
	w.adminCall("POST", "/admin/koh/arenas/arena-live/hills", map[string]any{"hill_code": "arena-live-01", "machine_id": m1, "base_score": "2.0000"})
	w.adminCall("POST", "/admin/koh/arenas/arena-live/hills", map[string]any{"hill_code": "arena-live-02", "arena_ip": "10.66.20.52", "base_score": 1.5})
	_ = m2

	// Pending arenas are invisible to agents.
	if r := w.call("neo01", "GET", "/arenas", nil); r.Code != float64(0) || len(field(t, r.Data, "items").([]any)) != 0 {
		t.Fatalf("pending arena listed: %+v", r)
	}
	if r := w.call("neo01", "GET", "/arenas/arena-live", nil); r.Status != 404 || r.Code != CodeNotFound {
		t.Fatalf("pending detail: %+v", r)
	}
	w.adminCall("POST", "/admin/koh/arenas/arena-live/start", nil)

	// GET /me
	me := w.call("neo01", "GET", "/me", nil)
	if me.Status != 200 || me.Code != float64(0) || me.Message != "ok" {
		t.Fatalf("/me: %+v", me)
	}
	if me.Data["player_name"] != "neo01" || me.Data["player_flag"] != w.flags["neo01"] || me.Data["status"] != "active" {
		t.Fatalf("/me data: %+v", me.Data)
	}
	if st := me.Data["server_time"].(string); !strings.HasSuffix(st, "+08:00") {
		t.Fatalf("server_time %s", st)
	}
	if me.Header.Get("Cache-Control") != "no-store" || len(me.Header.Get("X-Request-ID")) != 32 {
		t.Fatalf("headers: %v", me.Header)
	}

	// GET /arenas
	ar := w.call("neo01", "GET", "/arenas?page=1&page_size=50", nil)
	if field(t, ar.Data, "items", 0, "arena_code") != "arena-live" || field(t, ar.Data, "items", 0, "entrypoints", 0) != "10.40.0.11:443" ||
		field(t, ar.Data, "pagination", "total") != float64(1) {
		t.Fatalf("/arenas: %+v", ar.Data)
	}
	// Nothing claimed yet: no founded hills.
	if hills := field(t, w.call("neo01", "GET", "/arenas/arena-live", nil).Data, "founded_hills").([]any); len(hills) != 0 {
		t.Fatalf("unclaimed hills visible: %v", hills)
	}

	t0 := time.Date(2026, 9, 22, 14, 10, 0, 0, cst)
	tk := func(i int) time.Time { return t0.Add(time.Duration(i) * 5 * time.Minute) }
	// Tick 0: neo03 claims hill 1; hill 2 has junk. Tick 1: neo02 takes hill 1
	// (KingFile with a trailing newline + spaces still matches), neo01 claims 2.
	w.tick("arena-live", tk(0), map[string]string{"arena-live-01": w.flags["neo03"], "arena-live-02": "not a flag"})
	w.tick("arena-live", tk(1), map[string]string{"arena-live-01": w.flags["neo02"] + "\n  ", "arena-live-02": w.flags["neo01"]})

	detail := w.call("neo01", "GET", "/arenas/arena-live?include_changes=true", nil)
	if field(t, detail.Data, "arena", "status") != "running" {
		t.Fatalf("detail arena: %+v", detail.Data)
	}
	h1 := field(t, detail.Data, "founded_hills", 0)
	if field(t, h1, "hill_code") != "arena-live-01" || field(t, h1, "current_king") != "neo02" {
		t.Fatalf("hill 1: %+v", h1)
	}
	if field(t, h1, "change_records", 0, "change_type") != "takeover" ||
		field(t, h1, "change_records", 0, "before_king") != "neo03" ||
		field(t, h1, "change_records", 0, "description") != "neo02从neo03手中抢夺了山头" ||
		field(t, h1, "change_records", 0, "tick_time") != "2026-09-22T14:15:00+08:00" ||
		field(t, h1, "change_records", 1, "change_type") != "claim" ||
		field(t, h1, "change_records", 1, "before_king") != nil {
		t.Fatalf("hill 1 history: %+v", field(t, h1, "change_records"))
	}
	noChanges := w.call("neo01", "GET", "/arenas/arena-live?include_changes=false", nil)
	if len(field(t, noChanges.Data, "founded_hills", 0, "change_records").([]any)) != 0 {
		t.Fatal("include_changes=false must give empty change_records")
	}
	if r := w.call("neo01", "GET", "/arenas/arena-live?include_changes=maybe", nil); r.Status != 422 || r.Code != CodeParam {
		t.Fatalf("bad bool: %+v", r)
	}

	// Ticks 2..6: neo02 keeps hill 1 (streak 2..6), hill 2 goes down -> lost.
	for i := 2; i <= 6; i++ {
		w.tick("arena-live", tk(i), map[string]string{"arena-live-01": w.flags["neo02"]})
	}
	h2 := field(t, w.call("neo01", "GET", "/arenas/arena-live", nil).Data, "founded_hills", 1)
	if field(t, h2, "current_king") != nil || field(t, h2, "change_records", 0, "change_type") != "lost" ||
		field(t, h2, "change_records", 0, "after_king") != nil {
		t.Fatalf("hill 2 should be lost: %+v", h2)
	}

	// neo02: 3.0 (takeover) + 2.0 x4 (streak 2..5) + 2.2 (streak 6) = 13.2
	sc := w.call("neo02", "GET", "/me/scores?arena_code=arena-live&page_size=2", nil)
	sum := sc.Data["summary"].(map[string]any)
	if sum["total_score"] != "13.2000" || sum["tick_score"] != "13.2000" || sum["flag_score"] != "0.0000" ||
		sum["rank"] != float64(1) || sum["held_hill_count"] != float64(1) {
		t.Fatalf("neo02 summary: %+v", sum)
	}
	if field(t, sc.Data, "pagination", "total") != float64(6) || len(sc.Data["records"].([]any)) != 2 {
		t.Fatalf("neo02 pagination: %+v", sc.Data["pagination"])
	}
	latest := field(t, sc.Data, "records", 0)
	if field(t, latest, "score") != "2.2000" || field(t, latest, "score_detail", "base_score") != "2.0000" ||
		field(t, latest, "score_detail", "awards", 0, "award_type") != "streak" ||
		field(t, latest, "score_detail", "awards", 0, "award_score") != "0.2000" ||
		!strings.HasPrefix(field(t, latest, "record_key").(string), "tick:") {
		t.Fatalf("streak record: %+v", latest)
	}
	takeover := field(t, w.call("neo02", "GET", "/me/scores?arena_code=arena-live&page=6&page_size=1", nil).Data, "records", 0)
	if field(t, takeover, "score") != "3.0000" || field(t, takeover, "score_detail", "awards", 0, "award_type") != "takeover" ||
		field(t, takeover, "message") != "neo02 在 arena-live-01 得分 3.0000 （基础 2.0000 + 易主奖励 +1.0000）" {
		t.Fatalf("takeover record: %+v", takeover)
	}

	// Flags: register one at 2.5 on hill 2, then exercise submission rules.
	created := w.adminCall("POST", "/admin/koh/hills/arena-live-02/flags", map[string]any{"flag": "flag{Example}", "score": 2.5})
	if created["flag"] != "flag{Example}" {
		t.Fatalf("flag create: %v", created)
	}
	submit := func(p string, body any) resp { return w.call(p, "POST", "/flag_submit", body) }
	if r := submit("neo01", map[string]any{"arena_code": "arena-live", "flag": "flag{example}"}); r.Data["correct"] != false || r.Data["score"] != float64(0) {
		t.Fatalf("case-sensitive wrong answer: %+v", r)
	}
	if r := submit("neo01", map[string]any{"arena_code": "arena-live", "flag": " flag{Example}"}); r.Data["correct"] != false {
		t.Fatalf("whitespace must not be ignored: %+v", r)
	}
	if r := submit("neo01", map[string]any{"arena_code": "arena-live", "flag": "flag{Example}"}); r.Data["correct"] != true || r.Data["score"] != 2.5 {
		t.Fatalf("first solve: %+v", r)
	}
	if r := w.call("neo01", "POST", "/flags", map[string]any{"arena_code": "arena-live", "flag": "flag{Example}"}); r.Data["correct"] != true || r.Data["score"] != float64(0) {
		t.Fatalf("repeat solve (via /flags alias): %+v", r)
	}
	for _, c := range []struct {
		name   string
		body   any
		status int
	}{
		{"extra field", map[string]any{"arena_code": "arena-live", "flag": "x", "hill": "y"}, 422},
		{"missing flag", map[string]any{"arena_code": "arena-live"}, 422},
		{"flag not a string", map[string]any{"arena_code": "arena-live", "flag": 7}, 422},
		{"empty flag", map[string]any{"arena_code": "arena-live", "flag": ""}, 422},
		{"flag over 1024 chars", map[string]any{"arena_code": "arena-live", "flag": strings.Repeat("a", 1025)}, 422},
		{"arena_code over 64 chars", map[string]any{"arena_code": strings.Repeat("a", 65), "flag": "x"}, 422},
		{"control char", map[string]any{"arena_code": "arena-live", "flag": "flag{\u0007}"}, 400},
		{"over 1024 UTF-8 bytes", map[string]any{"arena_code": "arena-live", "flag": strings.Repeat("中", 400)}, 400},
		{"not JSON", "flag=1", 422},
		{"unknown arena", map[string]any{"arena_code": "nope", "flag": "x"}, 404},
	} {
		r := submit("neo01", c.body)
		if r.Status != c.status || r.Data != nil {
			t.Errorf("%s: got %d %v %q", c.name, r.Status, r.Code, r.Message)
		}
		if strings.Contains(r.Message, "flag{") || strings.Contains(r.Message, "中中") {
			t.Errorf("%s: error echoes the answer: %q", c.name, r.Message)
		}
	}
	// Wrong Content-Type is rejected even with a valid body.
	req := httptest.NewRequest("POST", "/flag_submit", strings.NewReader(`{"arena_code":"arena-live","flag":"x"}`))
	req.Header.Set("Authorization", "Bearer "+w.tokens["neo01"])
	req.Header.Set("Content-Type", "text/plain")
	if rec := httptest.NewRecorder(); func() int { w.agent.ServeHTTP(rec, req); return rec.Code }() != 422 {
		t.Error("text/plain body accepted")
	}

	// Ledger: flag record is first (newest), filters narrow records only.
	mine := w.call("neo01", "GET", "/me/scores?arena_code=arena-live", nil)
	if field(t, mine.Data, "records", 0, "source_type") != "flag" || field(t, mine.Data, "records", 0, "score_detail", "fixed_score") != "2.5000" ||
		field(t, mine.Data, "records", 0, "tick_time") != nil || field(t, mine.Data, "records", 0, "hill_code") != "arena-live-02" {
		t.Fatalf("flag record: %+v", field(t, mine.Data, "records", 0))
	}
	if s := mine.Data["summary"].(map[string]any); s["total_score"] != "4.0000" || s["flag_score"] != "2.5000" || s["tick_score"] != "1.5000" {
		t.Fatalf("neo01 summary: %+v", s)
	}
	onlyTick := w.call("neo01", "GET", "/me/scores?arena_code=arena-live&source_type=tick", nil)
	if field(t, onlyTick.Data, "pagination", "total") != float64(1) || field(t, onlyTick.Data, "summary", "total_score") != "4.0000" {
		t.Fatalf("source_type filter must narrow records only: %+v", onlyTick.Data)
	}
	if r := w.call("neo01", "GET", "/me/scores?hill_code=nope", nil); r.Status != 200 || len(r.Data["records"].([]any)) != 0 {
		t.Fatalf("unknown hill_code: %+v", r)
	}
	if r := w.call("neo01", "GET", "/me/scores?from_time=2026-09-22T14:20:00%2B08:00&to_time=2026-09-22T14:10:00", nil); r.Status != 422 {
		t.Fatalf("from>to: %+v", r)
	}
	for _, bad := range []string{"source_type=both", "from_time=yesterday", "page_size=201", "page=0"} {
		if r := w.call("neo01", "GET", "/me/scores?"+bad, nil); r.Status != 422 || r.Code != CodeParam {
			t.Errorf("%s: %+v", bad, r)
		}
	}
	if r := w.call("neo01", "GET", "/me/scores?arena_code=nope", nil); r.Status != 404 {
		t.Fatalf("unknown arena scores: %+v", r)
	}

	// Rankings: neo02 13.2, neo01 4.0, neo03 2.0.
	rk := w.call("neo03", "GET", "/rankings?arena_code=arena-live&limit=2", nil)
	if rk.Data["scope"] != "arena" || rk.Data["arena_code"] != "arena-live" || rk.Data["self_rank"] != float64(3) {
		t.Fatalf("rankings header: %+v", rk.Data)
	}
	items := rk.Data["items"].([]any)
	if len(items) != 2 || field(t, items, 0, "player_name") != "neo02" || field(t, items, 1, "player_name") != "neo01" ||
		field(t, items, 1, "total_score") != "4.0000" || field(t, items, 1, "flag_score") != "2.5000" {
		t.Fatalf("rankings items: %+v", items)
	}
	// Baseline = before the latest completed tick (tick 6, where only neo02
	// scored 2.2): neo02 was already #1; neo01's flag came after that tick.
	if field(t, items, 0, "score_delta") != "2.2000" || field(t, items, 0, "rank_delta") != float64(0) ||
		field(t, items, 1, "score_delta") != "2.5000" {
		t.Fatalf("deltas: %+v", items)
	}
	overall := w.call("neo03", "GET", "/rankings", nil)
	if overall.Data["scope"] != "overall" || overall.Data["arena_code"] != nil {
		t.Fatalf("overall: %+v", overall.Data)
	}
	if r := w.call("neo03", "GET", "/rankings?limit=501", nil); r.Status != 422 {
		t.Fatalf("limit 501: %+v", r)
	}

	// Disabled players still authenticate and score but are not ranked.
	db.Pool.Exec(w.ctx, `UPDATE players SET banned=true WHERE handle='neo03'`)
	if r := w.call("neo03", "GET", "/me", nil); r.Data["status"] != "disabled" {
		t.Fatalf("disabled /me: %+v", r)
	}
	rk = w.call("neo03", "GET", "/rankings?arena_code=arena-live", nil)
	if rk.Data["self_rank"] != nil || len(rk.Data["items"].([]any)) != 2 {
		t.Fatalf("disabled player ranked: %+v", rk.Data)
	}
	if s := field(t, w.call("neo03", "GET", "/me/scores?arena_code=arena-live", nil).Data, "summary"); field(t, s, "rank") != nil || field(t, s, "total_score") != "2.0000" {
		t.Fatalf("disabled summary: %+v", s)
	}

	// Finishing: settling -> finished; live endpoints 404, history stays readable.
	w.adminCall("POST", "/admin/koh/arenas/arena-live/finish", nil)
	if r := submit("neo01", map[string]any{"arena_code": "arena-live", "flag": "flag{Example}"}); r.Status != 404 {
		t.Fatalf("submit while settling: %+v", r)
	}
	runDue(w.ctx)
	var status string
	db.Pool.QueryRow(w.ctx, `SELECT status FROM koh_arenas WHERE arena_code='arena-live'`).Scan(&status)
	if status != "finished" {
		t.Fatalf("arena status %s", status)
	}
	if r := w.call("neo01", "GET", "/arenas", nil); len(r.Data["items"].([]any)) != 0 {
		t.Fatal("finished arena still listed")
	}
	if r := w.call("neo01", "GET", "/arenas/arena-live", nil); r.Status != 404 {
		t.Fatalf("finished detail: %+v", r)
	}
	if r := w.call("neo02", "GET", "/rankings?arena_code=arena-live", nil); r.Status != 200 || field(t, r.Data, "items", 0, "held_hill_count") != float64(1) {
		t.Fatalf("final standings: %+v", r)
	}
}

func TestAgentAPIAuthAndRouting(t *testing.T) {
	w := newWorld(t, "neo01")
	cases := []struct {
		name, auth, method, path string
		status                   int
		code                     string
	}{
		{"no header", "", "GET", "/me", 401, CodeMissingToken},
		{"wrong scheme", "Basic abc", "GET", "/me", 401, CodeMissingToken},
		{"empty bearer", "Bearer   ", "GET", "/me", 401, CodeMissingToken},
		{"unknown token", "Bearer koh_nope", "GET", "/me", 401, CodeInvalidToken},
		{"unknown route", "Bearer " + w.tokens["neo01"], "GET", "/nope", 404, CodeNotFound},
		{"wrong method", "Bearer " + w.tokens["neo01"], "GET", "/flag_submit", 405, CodeNotFound},
		{"token with padding", "Bearer  " + w.tokens["neo01"] + "  ", "GET", "/me", 200, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		w.agent.ServeHTTP(rec, req)
		var env map[string]any
		json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.status)
		}
		if c.code != "" && (env["code"] != c.code || env["data"] != nil) {
			t.Errorf("%s: envelope %v", c.name, env)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s: no X-Request-ID", c.name)
		}
	}
}

func TestAgentAPIRateLimit(t *testing.T) {
	w := newWorld(t, "neo01", "neo02")
	for {
		// Start early in a minute so the window can't roll over mid-test.
		if time.Now().Second() < 50 {
			break
		}
		time.Sleep(time.Second)
	}
	minute := time.Now().Unix() / 60
	for i := 0; i < 20; i++ {
		if r := w.call("neo01", "GET", "/rankings", nil); r.Status != 200 {
			t.Fatalf("request %d: %+v", i+1, r)
		}
	}
	r := w.call("neo01", "GET", "/rankings", nil)
	if time.Now().Unix()/60 != minute {
		t.Skip("minute rolled over")
	}
	if r.Status != 429 || r.Code != CodeRateLimited || r.Header.Get("Retry-After") != "" {
		t.Fatalf("21st request: %+v", r)
	}
	// Buckets are per player and per route.
	if r := w.call("neo02", "GET", "/rankings", nil); r.Status != 200 {
		t.Fatalf("other player throttled: %+v", r)
	}
	if r := w.call("neo01", "GET", "/me", nil); r.Status != 200 {
		t.Fatalf("other route throttled: %+v", r)
	}
	// /arenas/{code} shares one bucket across codes.
	db.Pool.Exec(w.ctx, `INSERT INTO koh_arenas (arena_code, status) VALUES ('a', 'running'), ('b', 'running')`)
	for i := 0; i < 20; i++ {
		w.call("neo02", "GET", []string{"/arenas/a", "/arenas/b"}[i%2], nil)
	}
	if r := w.call("neo02", "GET", "/arenas/b", nil); r.Status != 429 && time.Now().Unix()/60 == minute {
		t.Fatalf("route template bucket not shared: %+v", r)
	}
}
