package koh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Error codes of the agent API. Success is the integer 0; failures are these
// strings, always with data:null.
const (
	CodeParam        = "PARAM_ERROR"
	CodeMissingToken = "AUTH_MISSING_TOKEN"
	CodeInvalidToken = "AUTH_INVALID_TOKEN"
	CodeUnauthorized = "AUTH_UNAUTHORIZED"
	CodeNotFound     = "NOT_FOUND"
	CodeRateLimited  = "RATE_LIMITED"
	CodeInternal     = "INTERNAL_ERROR"
)

type envelope struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func writeEnvelope(w http.ResponseWriter, status int, body envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func ok(w http.ResponseWriter, data any) {
	writeEnvelope(w, http.StatusOK, envelope{Code: 0, Message: "ok", Data: data})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeEnvelope(w, status, envelope{Code: code, Message: msg, Data: nil})
}

// paramError is a 422 validation failure, message "<field>: <reason>".
func paramError(w http.ResponseWriter, field, reason string) {
	fail(w, http.StatusUnprocessableEntity, CodeParam, field+": "+reason)
}

func notFound(w http.ResponseWriter, msg string) { fail(w, http.StatusNotFound, CodeNotFound, msg) }

func internalError(w http.ResponseWriter, where string, err error) {
	log.Printf("[koh] %s: %v", where, err)
	fail(w, http.StatusInternalServerError, CodeInternal, "internal server error")
}

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxPlayer
)

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// requestMeta stamps X-Request-ID and Cache-Control: no-store on every
// response, writes the access log (request_id, player, path, status, duration,
// client IP) the scoring-dispute process relies on, and turns a panic into an
// INTERNAL_ERROR envelope instead of a dropped connection.
func requestMeta(clientIP func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 16)
			rand.Read(b)
			rid := hex.EncodeToString(b)
			w.Header().Set("X-Request-ID", rid)
			// Responses carry the token-bound player_flag and live standings:
			// nothing here is safe for a client or proxy to cache.
			w.Header().Set("Cache-Control", "no-store")
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			holder := &Player{}
			ctx := context.WithValue(r.Context(), ctxRequestID, rid)
			ctx = context.WithValue(ctx, ctxPlayer, holder)
			start := time.Now()
			defer func() {
				if p := recover(); p != nil {
					log.Printf("[koh] panic request_id=%s path=%s: %v", rid, r.URL.Path, p)
					fail(rec, http.StatusInternalServerError, CodeInternal, "internal server error")
				}
				log.Printf("[koh] access request_id=%s player=%q method=%s path=%s status=%d duration_ms=%d ip=%s",
					rid, holder.Name, r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds(), clientIP(r))
			}()
			next.ServeHTTP(rec, r.WithContext(ctx))
		})
	}
}
