package koh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cyberkiller/api/internal/db"
)

// Player is the authenticated caller. Disabled (banned) players still
// authenticate, hold hills and score - they are only hidden from rankings.
type Player struct {
	ID       uuid.UUID
	Name     string
	Flag     string
	Disabled bool
}

func playerFrom(r *http.Request) *Player {
	p, _ := r.Context().Value(ctxPlayer).(*Player)
	return p
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// Surrounding whitespace is ignored; anything else is a malformed header.
func bearerToken(h string) (string, bool) {
	h = strings.TrimSpace(h)
	scheme, tok, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

func requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, okTok := bearerToken(r.Header.Get("Authorization"))
		if !okTok {
			fail(w, http.StatusUnauthorized, CodeMissingToken, "missing or malformed bearer token")
			return
		}
		p := playerFrom(r)
		err := db.Pool.QueryRow(r.Context(), `
			SELECT id, handle, COALESCE(player_flag,''), banned FROM players WHERE api_token=$1
		`, tok).Scan(&p.ID, &p.Name, &p.Flag, &p.Disabled)
		if err != nil {
			fail(w, http.StatusUnauthorized, CodeInvalidToken, "invalid api token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Per-minute quotas, counted per player per route template (so /arenas/a and
// /arenas/b share one bucket).
var routeLimits = map[string]int{
	"me": 60, "arenas": 60, "flag_submit": 60,
	"arena_detail": 20, "me_scores": 20, "rankings": 20,
}

// limited enforces a fixed natural-minute window: the bucket key embeds the
// current minute, so the quota resets at :00 exactly as the spec promises.
// There is no Retry-After header by design.
func limited(route string, next http.HandlerFunc) http.HandlerFunc {
	limit := routeLimits[route]
	return func(w http.ResponseWriter, r *http.Request) {
		minute := time.Now().Unix() / 60
		key := fmt.Sprintf("koh:%s:%s:%d", route, playerFrom(r).ID, minute)
		if !db.RateLimit(r.Context(), key, limit, 70*time.Second) {
			fail(w, http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded: retry in the next minute")
			return
		}
		next(w, r)
	}
}

const flagAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// NewPlayerFlag returns a 20-char alphanumeric KingFile marker. Alphanumeric
// on purpose: it survives any shell quoting, Windows Set-Content, and strip().
func NewPlayerFlag() string {
	b := make([]byte, 20)
	max := big.NewInt(int64(len(flagAlphabet)))
	for i := range b {
		n, _ := rand.Int(rand.Reader, max)
		b[i] = flagAlphabet[n.Int64()]
	}
	return string(b)
}

// NewAPIToken returns a 192-bit bearer token.
func NewAPIToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "koh_" + hex.EncodeToString(b)
}

// EnsureCredentials mints whichever of api_token / player_flag the player is
// missing. Called at signup and (via BackfillCredentials) at boot.
func EnsureCredentials(ctx context.Context, playerID uuid.UUID) error {
	// Retry on the (astronomically unlikely) unique collision.
	var err error
	for i := 0; i < 3; i++ {
		_, err = db.Pool.Exec(ctx, `
			UPDATE players SET api_token=COALESCE(api_token,$2), player_flag=COALESCE(player_flag,$3)
			WHERE id=$1
		`, playerID, NewAPIToken(), NewPlayerFlag())
		if err == nil {
			return nil
		}
	}
	return err
}

// RotateCredentials replaces a player's token and/or flag (operator action,
// e.g. a leaked token). Rotating the flag forfeits hills held with the old one
// at the next tick, which is the point.
func RotateCredentials(ctx context.Context, playerID uuid.UUID, token, flag bool) error {
	tok, fl := "", ""
	if token {
		tok = NewAPIToken()
	}
	if flag {
		fl = NewPlayerFlag()
	}
	_, err := db.Pool.Exec(ctx, `
		UPDATE players SET api_token=COALESCE(NULLIF($2,''),api_token), player_flag=COALESCE(NULLIF($3,''),player_flag)
		WHERE id=$1
	`, playerID, tok, fl)
	return err
}

// BackfillCredentials gives every existing player agent credentials.
func BackfillCredentials(ctx context.Context) {
	rows, err := db.Pool.Query(ctx, `SELECT id FROM players WHERE api_token IS NULL OR player_flag IS NULL`)
	if err != nil {
		log.Printf("[koh] credential backfill: %v", err)
		return
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := EnsureCredentials(ctx, id); err != nil {
			log.Printf("[koh] credential backfill %s: %v", id, err)
		}
	}
}
