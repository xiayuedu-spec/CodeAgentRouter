package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/config"
	"codeagentrouter/internal/quota"
	"codeagentrouter/internal/ratelimit"
	"codeagentrouter/internal/report"
	"codeagentrouter/internal/store"
	"codeagentrouter/internal/upstream"
	"codeagentrouter/web"
)

const defaultTestConfig = `
server:
  addr: "127.0.0.1:0"
  timezone: "Asia/Shanghai"
  working_hours:
    - {start: 0, end: 24}
quota:
  default_hourly_tokens: 10000000
  default_daily_tokens: 400000000
  per_minute_requests: 10
security:
  admin_username: "admin"
  admin_password: "adminpass"
logging:
  dir: "logs"
`

type fakeUpstream struct {
	mu         sync.Mutex
	failFirst  map[string]bool
	callCount  map[string]int
	streamOpts map[string]bool
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		failFirst:  map[string]bool{},
		callCount:  map[string]int{},
		streamOpts: map[string]bool{},
	}
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	apiKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.URL.Path == "/v1/models" {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "deepseek-chat", "object": "model"},
				{"id": "gpt-4o", "object": "model"},
			},
		})
		return
	}

	f.mu.Lock()
	f.callCount[apiKey]++
	if f.failFirst[apiKey] && f.callCount[apiKey] == 1 {
		f.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	stream, _ := req["stream"].(bool)
	_, hasStreamOpts := req["stream_options"].(map[string]any)
	f.streamOpts[apiKey] = hasStreamOpts
	f.mu.Unlock()

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-1",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		"usage":   map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10},
	})
}

type testEnv struct {
	server      *httptest.Server
	upstreamURL string
	store       *store.Store
	upstream    *fakeUpstream
	userID      string
	accessKey   string
	logDir      string
}

func newTestEnv(t *testing.T, cfgYAML string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "state.json"), "test-encrypt-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeUpstream()
	fakeSrv := httptest.NewServer(fake)
	t.Cleanup(fakeSrv.Close)

	u, err := st.CreateUser("alice", "Alice", auth.HashPassword("pw"), true)
	if err != nil {
		t.Fatal(err)
	}
	accessKey, err := st.AddAccessKey(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUpstreamKey(u.ID, "fake", fakeSrv.URL, "sk-fake", nil, true, 0); err != nil {
		t.Fatal(err)
	}

	reports := report.NewService(dir)
	logs, err := report.NewLogger(dir, reports.Invalidate)
	if err != nil {
		t.Fatal(err)
	}
	am := auth.New(st, "admin", "adminpass")
	eq := quota.New(cfg, st)
	rl := ratelimit.New(cfg.Quota.PerMinuteRequests, time.Minute)
	up := upstream.New(st)
	handler := NewServer(cfg, st, am, eq, rl, up, reports, logs, web.FS)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = logs.Close() })

	return &testEnv{server: srv, upstreamURL: fakeSrv.URL, store: st, upstream: fake, userID: u.ID, accessKey: accessKey, logDir: dir}
}

func do(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRelayNonStreamUsage(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey,
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":5}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	c := e.store.UserCounters(e.userID)
	if c.Day.Value != 10 {
		t.Fatalf("day usage = %d, want 10 (real usage settled)", c.Day.Value)
	}
}

func TestRelayStreamUsage(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey,
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":5}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(out, []byte("[DONE]")) || !bytes.Contains(out, []byte(`"total_tokens":5`)) {
		t.Fatalf("stream body missing chunks: %s", out)
	}
	c := e.store.UserCounters(e.userID)
	if c.Day.Value != 5 {
		t.Fatalf("day usage = %d, want 5", c.Day.Value)
	}
	e.upstream.mu.Lock()
	injected := e.upstream.streamOpts["sk-fake"]
	e.upstream.mu.Unlock()
	if !injected {
		t.Fatal("stream_options.include_usage was not injected")
	}
}

func TestRelayRateLimit(t *testing.T) {
	cfg := strings.Replace(defaultTestConfig, "per_minute_requests: 10", "per_minute_requests: 2", 1)
	e := newTestEnv(t, cfg)
	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
	for i := 0; i < 2; i++ {
		resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i+1, resp.StatusCode)
		}
	}
	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestRelayDailyQuota(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	if err := e.store.UpdateUserQuota(e.userID, 0, 10); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`
	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	msg, _ := payload["error"].(map[string]any)
	if !strings.Contains(msg["message"].(string), "daily quota exceeded") {
		t.Fatalf("unexpected error: %v", msg)
	}
}

func TestRelayRetriesToPoolKey(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	// bob owns a healthy pool key; alice's own key fails its first request.
	bob, err := e.store.CreateUser("bob", "Bob", auth.HashPassword("pw"), true)
	if err != nil {
		t.Fatal(err)
	}
	e.upstream.mu.Lock()
	e.upstream.failFirst["sk-fail"] = true
	e.upstream.mu.Unlock()

	// alice's key was created with sk-fake; replace it with sk-fail.
	keys := e.store.ListUpstreamKeys(e.userID)
	updated := *keys[0]
	enc, _ := e.store.Encrypt("sk-fail")
	updated.APIKeyEnc = enc
	if err := e.store.ReplaceUpstreamKey(&updated); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateUpstreamKey(bob.ID, "pool", e.upstreamURL, "sk-ok", nil, true, 0); err != nil {
		t.Fatal(err)
	}

	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey,
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	e.upstream.mu.Lock()
	defer e.upstream.mu.Unlock()
	if e.upstream.callCount["sk-fail"] != 1 || e.upstream.callCount["sk-ok"] != 1 {
		t.Fatalf("call counts: fail=%d ok=%d, want 1/1", e.upstream.callCount["sk-fail"], e.upstream.callCount["sk-ok"])
	}
}

func TestRelayModels(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	resp := do(t, http.MethodGet, e.server.URL+"/v1/models", e.accessKey, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	ids := map[string]bool{}
	for _, m := range payload.Data {
		id, _ := m["id"].(string)
		ids[id] = true
	}
	if !ids["deepseek-chat"] || !ids["gpt-4o"] {
		t.Fatalf("models = %v", ids)
	}
}

func TestAdminLoginAndUserSelfService(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	resp := do(t, http.MethodPost, e.server.URL+"/admin/login", "", `{"username":"admin","password":"adminpass"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login status = %d", resp.StatusCode)
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	if login.Token == "" {
		t.Fatal("admin token empty")
	}

	resp = do(t, http.MethodPost, e.server.URL+"/user/login", "", `{"username":"alice","password":"pw"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user login status = %d", resp.StatusCode)
	}
	var userLogin struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&userLogin)
	if userLogin.Token == "" {
		t.Fatal("user token empty")
	}
	resp = do(t, http.MethodGet, e.server.URL+"/user/me", userLogin.Token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user me status = %d", resp.StatusCode)
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me.ID != e.userID || me.Username != "alice" {
		t.Fatalf("me = %+v", me)
	}
}

func TestRequestLogWritten(t *testing.T) {
	e := newTestEnv(t, defaultTestConfig)
	resp := do(t, http.MethodPost, e.server.URL+"/v1/chat/completions", e.accessKey,
		`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	day := time.Now().Format("2006-01-02")
	data, err := os.ReadFile(filepath.Join(e.logDir, "requests-"+day+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"user_id":"`+e.userID+`"`) ||
		!strings.Contains(string(data), `"route_type":"own"`) {
		t.Fatalf("log missing fields: %s", data)
	}
}
