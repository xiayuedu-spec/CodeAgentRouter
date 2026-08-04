package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/config"
	"codeagentrouter/internal/quota"
	"codeagentrouter/internal/ratelimit"
	"codeagentrouter/internal/report"
	"codeagentrouter/internal/store"
	"codeagentrouter/internal/upstream"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxUserID
	ctxSession
)

// Server wires the relay, admin and user APIs plus the embedded web console.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	auth    *auth.Manager
	quota   *quota.Engine
	limiter *ratelimit.Limiter
	up      *upstream.Client
	reports *report.Service
	logs    *report.Logger

	relay *Relay
	admin *Admin
	user  *UserAPI
}

func NewServer(
	cfg *config.Config,
	st *store.Store,
	am *auth.Manager,
	eq *quota.Engine,
	rl *ratelimit.Limiter,
	up *upstream.Client,
	reports *report.Service,
	logs *report.Logger,
	webFS fs.FS,
) http.Handler {
	s := &Server{
		cfg:     cfg,
		store:   st,
		auth:    am,
		quota:   eq,
		limiter: rl,
		up:      up,
		reports: reports,
		logs:    logs,
	}
	s.relay = &Relay{Server: s}
	s.admin = &Admin{Server: s}
	s.user = &UserAPI{Server: s}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/chat/completions", s.relay.ServeChatCompletions)
	mux.HandleFunc("POST /v1/completions", s.relay.ServeCompletions)
	mux.HandleFunc("POST /v1/embeddings", s.relay.ServeEmbeddings)
	mux.HandleFunc("GET /v1/models", s.relay.ServeModels)

	mux.HandleFunc("POST /admin/login", s.admin.Login)
	mux.HandleFunc("GET /admin/users", s.admin.ListUsers)
	mux.HandleFunc("POST /admin/users", s.admin.CreateUser)
	mux.HandleFunc("DELETE /admin/users/{id}", s.admin.DeleteUser)
	mux.HandleFunc("PUT /admin/users/{id}/quota", s.admin.UpdateQuota)
	mux.HandleFunc("PUT /admin/users/{id}/enabled", s.admin.SetEnabled)
	mux.HandleFunc("POST /admin/users/{id}/password", s.admin.ResetPassword)
	mux.HandleFunc("POST /admin/users/{id}/access-keys", s.admin.CreateAccessKey)
	mux.HandleFunc("DELETE /admin/users/{id}/access-keys/{key}", s.admin.DeleteAccessKey)
	mux.HandleFunc("GET /admin/keys", s.admin.KeyPool)
	mux.HandleFunc("GET /admin/keys/{userID}", s.admin.ListUserKeys)
	mux.HandleFunc("POST /admin/keys/{userID}", s.admin.CreateKey)
	mux.HandleFunc("PUT /admin/keys/{userID}/{keyID}", s.admin.UpdateKey)
	mux.HandleFunc("DELETE /admin/keys/{userID}/{keyID}", s.admin.DeleteKey)
	mux.HandleFunc("GET /admin/usage/user/{id}", s.admin.UserUsage)
	mux.HandleFunc("GET /admin/reports/monthly", s.admin.MonthlyReport)
	mux.HandleFunc("GET /admin/reports/user/{id}", s.admin.UserReport)

	mux.HandleFunc("POST /user/login", s.user.Login)
	mux.HandleFunc("GET /user/me", s.user.Me)
	mux.HandleFunc("GET /user/keys", s.user.ListKeys)
	mux.HandleFunc("POST /user/keys", s.user.CreateKey)
	mux.HandleFunc("PUT /user/keys/{keyID}", s.user.UpdateKey)
	mux.HandleFunc("DELETE /user/keys/{keyID}", s.user.DeleteKey)
	mux.HandleFunc("GET /user/usage", s.user.Usage)
	mux.HandleFunc("GET /user/reports", s.user.MonthlyReport)

	web := http.FileServer(http.FS(webFS))
	mux.Handle("/", web)

	return s.withMiddleware(mux)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return requestIDMiddleware(recoverMiddleware(s.routeMiddleware(next)))
}

func (s *Server) routeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/v1/"):
			uid, err := s.relay.authenticate(r)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "invalid relay access key")
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserID, uid)
			next.ServeHTTP(w, r.WithContext(ctx))
		case strings.HasPrefix(p, "/admin/"):
			if p == "/admin/login" {
				next.ServeHTTP(w, r)
				return
			}
			if err := s.admin.authenticate(w, r); err != nil {
				return
			}
			next.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/user/"):
			if p == "/user/login" {
				next.ServeHTTP(w, r)
				return
			}
			req, err := s.user.authenticate(w, r)
			if err != nil {
				return
			}
			next.ServeHTTP(w, req)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = "req_" + randHex(12)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("internal error: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func randHex(n int) string {
	const chars = "0123456789abcdef"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = chars[int(now>>(uint(i)*4))&0xf]
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "relay_error", "code": strconv.Itoa(status)}})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func splitHostPort(addr string) (string, string, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "", errors.New("no port")
	}
	return addr[:idx], addr[idx+1:], nil
}
