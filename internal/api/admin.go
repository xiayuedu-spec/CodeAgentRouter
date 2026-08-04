package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/model"
	"codeagentrouter/internal/report"
	"codeagentrouter/internal/store"
)

type Admin struct {
	*Server
}

func (a *Admin) authenticate(w http.ResponseWriter, r *http.Request) error {
	token := bearerToken(r)
	if err := a.auth.RequireAdmin(token); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "admin authentication required")
		return err
	}
	return nil
}

func (a *Admin) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid login body")
		return
	}
	token, err := a.auth.LoginAdmin(req.Username, req.Password)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

type userView struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"display_name"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	HourlyTokens int64     `json:"hourly_tokens"`
	DailyTokens  int64     `json:"daily_tokens"`
	HourlyUsed   int64     `json:"hourly_used"`
	DailyUsed    int64     `json:"daily_used"`
	AccessKeys   int       `json:"access_keys"`
	UpstreamKeys int       `json:"upstream_keys"`
}

func (a *Admin) userView(u *model.User) userView {
	counters := a.store.UserCounters(u.ID)
	return userView{
		ID:           u.ID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Enabled:      u.Enabled,
		CreatedAt:    u.CreatedAt,
		HourlyTokens: u.HourlyTokens,
		DailyTokens:  u.DailyTokens,
		HourlyUsed:   counters.Hour.Value,
		DailyUsed:    counters.Day.Value,
		AccessKeys:   len(a.store.ListAccessKeys(u.ID)),
		UpstreamKeys: len(a.store.ListUpstreamKeys(u.ID)),
	}
}

func (a *Admin) ListUsers(w http.ResponseWriter, r *http.Request) {
	users := a.store.ListUsers()
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, a.userView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (a *Admin) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username     string `json:"username"`
		DisplayName  string `json:"display_name"`
		Password     string `json:"password"`
		Enabled      bool   `json:"enabled"`
		HourlyTokens int64  `json:"hourly_tokens"`
		DailyTokens  int64  `json:"daily_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid user body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	u, err := a.store.CreateUser(req.Username, req.DisplayName, auth.HashPassword(req.Password), req.Enabled)
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			writeJSONError(w, http.StatusConflict, "username already exists")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.HourlyTokens > 0 {
		_ = a.store.UpdateUserQuota(u.ID, req.HourlyTokens, req.DailyTokens)
	}
	key, _ := a.store.AddAccessKey(u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":       a.userView(u),
		"access_key": key,
	})
}

func (a *Admin) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.store.DeleteUser(id); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) UpdateQuota(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		HourlyTokens *int64 `json:"hourly_tokens"`
		DailyTokens  *int64 `json:"daily_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid quota body")
		return
	}
	u := a.store.GetUser(id)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	hourly, daily := u.HourlyTokens, u.DailyTokens
	if req.HourlyTokens != nil {
		hourly = *req.HourlyTokens
	}
	if req.DailyTokens != nil {
		daily = *req.DailyTokens
	}
	if err := a.store.UpdateUserQuota(id, hourly, daily); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": a.userView(a.store.GetUser(id))})
}

func (a *Admin) SetEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := a.store.SetUserEnabled(id, req.Enabled); err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": a.userView(a.store.GetUser(id))})
}

func (a *Admin) ResetPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		writeJSONError(w, http.StatusBadRequest, "password is required")
		return
	}
	if err := a.store.UpdateUserPassword(id, auth.HashPassword(req.Password)); err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) CreateAccessKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := a.store.AddAccessKey(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"access_key": key})
}

func (a *Admin) DeleteAccessKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.PathValue("key")
	if err := a.store.RemoveAccessKey(id, key); err != nil {
		writeJSONError(w, http.StatusNotFound, "access key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type keyView struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	Models      []string  `json:"models"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	HourlyLimit int64     `json:"hourly_limit"`
	HasKey      bool      `json:"has_key"`
	HourlyUsed  int64     `json:"hourly_used"`
	InFlight    int       `json:"in_flight"`
}

func (s *Server) keyView(k *model.UpstreamKey) keyView {
	return keyView{
		ID:          k.ID,
		UserID:      k.UserID,
		Name:        k.Name,
		BaseURL:     k.BaseURL,
		Models:      k.Models,
		Enabled:     k.Enabled,
		CreatedAt:   k.CreatedAt,
		HourlyLimit: k.HourlyLimit,
		HasKey:      len(k.APIKeyEnc) > 0,
		HourlyUsed:  s.store.KeyHourCounter(k.ID).Value,
		InFlight:    s.store.InFlight(k.ID),
	}
}

func (a *Admin) ListUserKeys(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	keys := a.store.ListUpstreamKeys(userID)
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, a.keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (a *Admin) KeyPool(w http.ResponseWriter, r *http.Request) {
	keys := a.store.AllUpstreamKeys()
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, a.keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (a *Admin) CreateKey(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
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
	k, err := a.store.CreateUpstreamKey(userID, req.Name, strings.TrimRight(req.BaseURL, "/"), req.APIKey, req.Models, req.Enabled, req.HourlyLimit)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": a.keyView(k)})
}

func (a *Admin) UpdateKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyID")
	k := a.store.GetUpstreamKey(keyID)
	if k == nil {
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
		enc, err := a.store.Encrypt(*req.APIKey)
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
	if err := a.store.ReplaceUpstreamKey(&updated); err != nil {
		writeJSONError(w, http.StatusNotFound, "upstream key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": a.keyView(a.store.GetUpstreamKey(keyID))})
}

func (a *Admin) DeleteKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyID")
	if err := a.store.DeleteUpstreamKey(keyID); err != nil {
		writeJSONError(w, http.StatusNotFound, "upstream key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) UserUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u := a.store.GetUser(id)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	counters := a.store.UserCounters(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":          id,
		"hourly_used":      counters.Hour.Value,
		"hourly_limit":     a.quota.HourlyLimit(u),
		"daily_used":       counters.Day.Value,
		"daily_limit":      a.quota.DailyLimit(u),
		"working_hour":     a.quota.IsWorkingHour(time.Now()),
		"per_minute_limit": a.cfg.Quota.PerMinuteRequests,
	})
}

func (a *Admin) MonthlyReport(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	rep, err := a.reports.Monthly(month)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=report-"+month+".csv")
		_, _ = w.Write([]byte(rep.CSV()))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (a *Admin) UserReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	rep, err := a.reports.Monthly(month)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := &report.Report{Month: month, Summary: rep.Summary}
	for _, row := range rep.Rows {
		if row.UserID == id {
			filtered.Rows = append(filtered.Rows, row)
		}
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=report-user-"+id+"-"+month+".csv")
		_, _ = w.Write([]byte(filtered.CSV()))
		return
	}
	writeJSON(w, http.StatusOK, filtered)
}
