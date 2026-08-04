package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/report"
)

type UserAPI struct {
	*Server
}

func (u *UserAPI) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, error) {
	token := bearerToken(r)
	s, err := u.auth.RequireUser(token)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "user authentication required")
		return nil, err
	}
	return r.WithContext(withSession(r.Context(), s)), nil
}

func withSession(ctx context.Context, s *auth.Session) context.Context {
	return context.WithValue(ctx, ctxSession, s)
}

func (u *UserAPI) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid login body")
		return
	}
	token, err := u.auth.LoginUser(req.Username, req.Password)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (u *UserAPI) session(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(ctxSession).(*auth.Session)
	return s
}

func (u *UserAPI) Me(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	user := u.store.GetUser(s.UserID)
	if user == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	keys := u.store.ListAccessKeys(user.ID)
	counters := u.store.UserCounters(user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            user.ID,
		"username":      user.Username,
		"display_name":  user.DisplayName,
		"enabled":       user.Enabled,
		"hourly_tokens": user.HourlyTokens,
		"daily_tokens":  user.DailyTokens,
		"hourly_used":   counters.Hour.Value,
		"daily_used":    counters.Day.Value,
		"hourly_limit":  u.quota.HourlyLimit(user),
		"daily_limit":   u.quota.DailyLimit(user),
		"access_keys":   keys,
	})
}

func (u *UserAPI) ListKeys(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	keys := u.store.ListUpstreamKeys(s.UserID)
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, u.keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (u *UserAPI) CreateKey(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	var req struct {
		Name        string   `json:"name"`
		BaseURL     string   `json:"base_url"`
		APIKey      string   `json:"api_key"`
		Models      []string `json:"models"`
		Enabled     bool     `json:"enabled"`
		HourlyLimit int64    `json:"hourly_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid key body")
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" || strings.TrimSpace(req.APIKey) == "" {
		writeJSONError(w, http.StatusBadRequest, "base_url and api_key are required")
		return
	}
	k, err := u.store.CreateUpstreamKey(s.UserID, req.Name, strings.TrimRight(req.BaseURL, "/"), req.APIKey, req.Models, req.Enabled, req.HourlyLimit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": u.keyView(k)})
}

func (u *UserAPI) UpdateKey(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	keyID := r.PathValue("keyID")
	k := u.store.GetUpstreamKey(keyID)
	if k == nil || k.UserID != s.UserID {
		writeJSONError(w, http.StatusNotFound, "upstream key not found")
		return
	}
	var req struct {
		Name        *string   `json:"name"`
		BaseURL     *string   `json:"base_url"`
		APIKey      *string   `json:"api_key"`
		Models      *[]string `json:"models"`
		Enabled     *bool     `json:"enabled"`
		HourlyLimit *int64    `json:"hourly_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	updated := *k
	if req.Name != nil {
		updated.Name = *req.Name
	}
	if req.BaseURL != nil {
		updated.BaseURL = strings.TrimRight(*req.BaseURL, "/")
	}
	if req.APIKey != nil {
		enc, err := u.store.Encrypt(*req.APIKey)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		updated.APIKeyEnc = enc
	}
	if req.Models != nil {
		updated.Models = *req.Models
	}
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if req.HourlyLimit != nil {
		updated.HourlyLimit = *req.HourlyLimit
	}
	if err := u.store.ReplaceUpstreamKey(&updated); err != nil {
		writeJSONError(w, http.StatusNotFound, "upstream key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": u.keyView(u.store.GetUpstreamKey(keyID))})
}

func (u *UserAPI) DeleteKey(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	keyID := r.PathValue("keyID")
	k := u.store.GetUpstreamKey(keyID)
	if k == nil || k.UserID != s.UserID {
		writeJSONError(w, http.StatusNotFound, "upstream key not found")
		return
	}
	if err := u.store.DeleteUpstreamKey(keyID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UserAPI) Usage(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	user := u.store.GetUser(s.UserID)
	if user == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	counters := u.store.UserCounters(user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":          user.ID,
		"hourly_used":      counters.Hour.Value,
		"hourly_limit":     u.quota.HourlyLimit(user),
		"daily_used":       counters.Day.Value,
		"daily_limit":      u.quota.DailyLimit(user),
		"working_hour":     u.quota.IsWorkingHour(time.Now()),
		"per_minute_limit": u.cfg.Quota.PerMinuteRequests,
	})
}

func (u *UserAPI) MonthlyReport(w http.ResponseWriter, r *http.Request) {
	s := u.session(r)
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	rep, err := u.reports.Monthly(month)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := &report.Report{Month: month, Summary: rep.Summary}
	for _, row := range rep.Rows {
		if row.UserID == s.UserID {
			filtered.Rows = append(filtered.Rows, row)
		}
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=report-user-"+s.UserID+"-"+month+".csv")
		_, _ = w.Write([]byte(filtered.CSV()))
		return
	}
	writeJSON(w, http.StatusOK, filtered)
}
