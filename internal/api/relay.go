package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeagentrouter/internal/auth"
	"codeagentrouter/internal/model"
	"codeagentrouter/internal/quota"
	"codeagentrouter/internal/report"
	"codeagentrouter/internal/router"
	"codeagentrouter/internal/store"
	"codeagentrouter/internal/tokenize"
)

const maxBodyBytes = 32 << 20

// Relay serves the OpenAI-compatible proxy endpoints.
type Relay struct {
	*Server
	modelsMu   sync.Mutex
	modelsList []map[string]any
	modelsAt   time.Time
}

func (h *Relay) authenticate(r *http.Request) (string, error) {
	key := bearerToken(r)
	uid, ok := h.store.UserByAccessKey(key)
	if !ok {
		return "", auth.ErrUnauthorized
	}
	u := h.store.GetUser(uid)
	if u == nil || !u.Enabled {
		return "", auth.ErrUnauthorized
	}
	return uid, nil
}

func (h *Relay) ServeChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	payload, ok := parsePayload(w, body)
	if !ok {
		return
	}
	if !requireModel(w, payload) {
		return
	}
	prompt, completion := tokenize.EstimateChatPayload(payload)
	h.proxy(w, r, "/v1/chat/completions", body, prompt, completion, true)
}

func (h *Relay) ServeCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	payload, ok := parsePayload(w, body)
	if !ok {
		return
	}
	if !requireModel(w, payload) {
		return
	}
	prompt, completion := tokenize.EstimateCompletionPayload(payload)
	h.proxy(w, r, "/v1/completions", body, prompt, completion, true)
}

func (h *Relay) ServeEmbeddings(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	payload, ok := parsePayload(w, body)
	if !ok {
		return
	}
	if !requireModel(w, payload) {
		return
	}
	prompt := tokenize.EstimateEmbeddingPayload(payload)
	h.proxy(w, r, "/v1/embeddings", body, prompt, 0, false)
}

func (h *Relay) ServeModels(w http.ResponseWriter, r *http.Request) {
	list, err := h.models()
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
}

func (h *Relay) models() ([]map[string]any, error) {
	h.modelsMu.Lock()
	defer h.modelsMu.Unlock()
	if len(h.modelsList) > 0 && time.Since(h.modelsAt) < time.Minute {
		return h.modelsList, nil
	}
	keys := h.store.AllUpstreamKeys()
	merged := map[string]map[string]any{}
	var lastErr error
	for _, k := range keys {
		if !k.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		items, err := h.up.Models(ctx, k)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		for _, item := range items {
			id, _ := item["id"].(string)
			if id == "" {
				continue
			}
			if _, ok := merged[id]; !ok {
				merged[id] = map[string]any{
					"id":       id,
					"object":   "model",
					"owned_by": k.UserID,
				}
			}
		}
	}
	if len(merged) == 0 {
		if len(h.modelsList) > 0 {
			return h.modelsList, nil
		}
		if lastErr != nil {
			return nil, fmt.Errorf("all upstream model lists failed: %v", lastErr)
		}
		return []map[string]any{}, nil
	}
	out := make([]map[string]any, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	h.modelsList = out
	h.modelsAt = time.Now()
	return out, nil
}

func (h *Relay) proxy(
	w http.ResponseWriter,
	r *http.Request,
	path string,
	body []byte,
	promptTokens, estCompletion int64,
	injectUsage bool,
) {
	start := time.Now()
	userID, _ := r.Context().Value(ctxUserID).(string)
	stream := bodyHasStream(body)
	modelName := modelFromBody(body)

	now := time.Now()
	if ok, retry := h.limiter.Allow(userID, now); !ok {
		secs := int64(math.Ceil(retry.Seconds()))
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		h.finish(w, r, start, report.Entry{
			UserID: userID, Model: modelName, Stream: stream, Status: http.StatusTooManyRequests,
			Error: "rate limit exceeded",
		}, 0, "", "")
		writeJSONError(w, http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded, retry after %ds", secs))
		return
	}

	est := promptTokens + estCompletion
	if err := h.quota.CheckAndReserve(userID, est, now); err != nil {
		status := http.StatusTooManyRequests
		msg := err.Error()
		switch {
		case errors.Is(err, store.ErrDailyQuota):
			msg = "daily quota exceeded"
		case errors.Is(err, store.ErrHourlyQuota):
			msg = hourlyExhaustedMsg(h.quota, now)
		}
		h.finish(w, r, start, report.Entry{
			UserID: userID, Model: modelName, Stream: stream, Status: status, Error: msg,
		}, 0, "", "")
		writeJSONError(w, status, msg)
		return
	}

	working := h.quota.IsWorkingHour(now)
	exclude := map[string]bool{}
	requestBody := body
	if injectUsage && stream {
		requestBody = injectStreamOptions(body)
	}
	stripped := false
	var usedKey *model.UpstreamKey
	var lastResp *http.Response
	var lastKey *model.UpstreamKey
	var resp *http.Response
	var err error

	for attempt := 0; attempt < 2; attempt++ {
		key, selErr := h.store.SelectKey(userID, modelName, working, h.cfg.Quota.DefaultHourlyTokens, exclude)
		if selErr != nil {
			h.quota.Release(userID, "", est)
			status := http.StatusBadGateway
			msg := selErr.Error()
			if errors.Is(selErr, router.ErrHourlyExhausted) {
				status = http.StatusTooManyRequests
				msg = hourlyExhaustedMsg(h.quota, now)
			}
			h.finish(w, r, start, report.Entry{
				UserID: userID, Model: modelName, Stream: stream, Status: status, Error: msg,
			}, 0, "", "")
			writeJSONError(w, status, msg)
			return
		}
		limit := key.HourlyLimit
		if limit <= 0 {
			limit = h.cfg.Quota.DefaultHourlyTokens
		}
		if kErr := h.store.ReserveKey(key.ID, est, limit, working); kErr != nil {
			exclude[key.ID] = true
			attempt--
			continue
		}
		resp, err = h.up.Do(r.Context(), key, http.MethodPost, path, requestBody, stream)
		if err != nil {
			h.store.ReleaseKey(key.ID, est)
			if r.Context().Err() != nil {
				h.quota.Release(userID, key.ID, est)
				h.finish(w, r, start, report.Entry{
					UserID: userID, Model: modelName, Stream: stream, Status: 499, Error: "client disconnected",
				}, 0, key.ID, routeType(userID, key))
				return
			}
			exclude[key.ID] = true
			continue
		}
		status := resp.StatusCode
		if status == http.StatusBadRequest && injectUsage && stream && !stripped {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			h.store.ReleaseKey(key.ID, est)
			requestBody = stripStreamOptions(body)
			stripped = true
			continue
		}
		if status >= 200 && status < 300 {
			usedKey = key
			break
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		h.store.ReleaseKey(key.ID, est)
		lastResp = resp
		lastKey = key
		if status == http.StatusTooManyRequests || status >= 500 {
			exclude[key.ID] = true
			continue
		}
		break
	}

	if usedKey == nil {
		if lastResp != nil {
			h.passthrough(w, lastResp)
			h.quota.Release(userID, "", est)
			h.finish(w, r, start, report.Entry{
				UserID: userID, Model: modelName, Stream: stream, Status: lastResp.StatusCode,
				Error: "upstream error",
			}, 0, lastKey.ID, routeType(userID, lastKey))
			return
		}
		h.quota.Release(userID, "", est)
		h.finish(w, r, start, report.Entry{
			UserID: userID, Model: modelName, Stream: stream, Status: http.StatusBadGateway,
			Error: "upstream request failed",
		}, 0, "", "")
		writeJSONError(w, http.StatusBadGateway, "upstream request failed")
		return
	}

	var actual int64
	if stream {
		actual = h.proxyStream(w, resp, promptTokens)
	} else {
		actual = h.proxyJSON(w, resp, promptTokens, estCompletion)
	}
	h.quota.Settle(userID, usedKey.ID, est, actual)
	h.finish(w, r, start, report.Entry{
		UserID: userID, Model: modelName, Stream: stream, Status: http.StatusOK,
		PromptTokens: promptTokens, CompletionTokens: estCompletion, TotalTokens: est,
	}, actual, usedKey.ID, routeType(userID, usedKey))
}

func (h *Relay) proxyJSON(w http.ResponseWriter, resp *http.Response, promptTokens, estCompletion int64) int64 {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
	if err != nil {
		return promptTokens + estCompletion
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		if u, ok := obj["usage"].(map[string]any); ok {
			p := numInt64(u["prompt_tokens"])
			c := numInt64(u["completion_tokens"])
			t := numInt64(u["total_tokens"])
			if t > 0 {
				return t
			}
			if p > 0 || c > 0 {
				return p + c
			}
		}
	}
	return promptTokens + estCompletion
}

func (h *Relay) proxyStream(w http.ResponseWriter, resp *http.Response, promptTokens int64) int64 {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	br := bufio.NewReader(resp.Body)
	defer resp.Body.Close()
	var usage *usagePayload
	var chars int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			u, delta := parseSSELine(line)
			if u != nil {
				usage = u
			}
			chars += delta
			if _, werr := w.Write(line); werr != nil {
				return promptTokens + completionEstimate(chars)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if usage != nil {
		if usage.TotalTokens > 0 {
			return usage.TotalTokens
		}
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			return usage.PromptTokens + usage.CompletionTokens
		}
	}
	return promptTokens + completionEstimate(chars)
}

// completionEstimate is a coarse fallback for streams whose upstream does not
// emit usage: a blend of the 1-token-per-CJK and 4-chars-per-token heuristics.
func completionEstimate(chars int64) int64 {
	if chars <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(chars) * 0.35))
}

type usagePayload struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func parseSSELine(line []byte) (*usagePayload, int64) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil, 0
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, 0
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil, 0
	}
	var u *usagePayload
	if raw, ok := obj["usage"].(map[string]any); ok {
		u = &usagePayload{
			PromptTokens:     numInt64(raw["prompt_tokens"]),
			CompletionTokens: numInt64(raw["completion_tokens"]),
			TotalTokens:      numInt64(raw["total_tokens"]),
		}
	}
	var delta int64
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if d, ok := ch["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					delta += int64(len([]rune(c)))
				}
			}
			if t, ok := ch["text"].(string); ok {
				delta += int64(len([]rune(t)))
			}
		}
	}
	return u, delta
}

func (h *Relay) passthrough(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Relay) finish(
	w http.ResponseWriter,
	r *http.Request,
	start time.Time,
	e report.Entry,
	actual int64,
	keyID, route string,
) {
	e.TS = time.Now().Format(time.RFC3339)
	e.AccessKey = bearerToken(r)
	e.RequestID = requestID(r)
	e.ClientIP = clientIP(r)
	e.LatencyMs = time.Since(start).Milliseconds()
	e.UpstreamKeyID = keyID
	e.RouteType = route
	if actual > 0 {
		e.TotalTokens = actual
	}
	if e.Error != "" && e.Status >= 200 && e.Status < 300 {
		e.Status = http.StatusBadGateway
	}
	_ = h.logs.Write(e)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil, false
	}
	return body, true
}

func parsePayload(w http.ResponseWriter, body []byte) (map[string]any, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return nil, false
	}
	return payload, true
}

func requireModel(w http.ResponseWriter, payload map[string]any) bool {
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeJSONError(w, http.StatusBadRequest, "model is required")
		return false
	}
	return true
}

func modelFromBody(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	m, _ := payload["model"].(string)
	return m
}

func bodyHasStream(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	s, _ := payload["stream"].(bool)
	return s
}

func injectStreamOptions(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	if payload["stream"] != true {
		return body
	}
	if _, ok := payload["stream_options"]; ok {
		return body
	}
	payload["stream_options"] = map[string]any{"include_usage": true}
	out, _ := json.Marshal(payload)
	return out
}

func stripStreamOptions(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	delete(payload, "stream_options")
	out, _ := json.Marshal(payload)
	return out
}

func hourlyExhaustedMsg(e *quota.Engine, now time.Time) string {
	return "hourly quota exhausted, retry after " + nextWorkingStart(e, now).Format("15:04")
}

func nextWorkingStart(e *quota.Engine, now time.Time) time.Time {
	hours := e.WorkingHours()
	if len(hours) == 0 {
		return now.Add(time.Hour)
	}
	loc := e.Location()
	lt := now.In(loc)
	for _, wh := range hours {
		if lt.Hour() >= wh.Start && lt.Hour() < wh.End {
			next := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour()+1, 0, 0, 0, loc)
			return next
		}
	}
	for _, wh := range hours {
		if lt.Hour() < wh.Start {
			return time.Date(lt.Year(), lt.Month(), lt.Day(), wh.Start, 0, 0, 0, loc)
		}
	}
	next := lt.AddDate(0, 0, 1)
	return time.Date(next.Year(), next.Month(), next.Day(), hours[0].Start, 0, 0, 0, loc)
}

func routeType(userID string, key *model.UpstreamKey) string {
	if key.UserID == userID {
		return "own"
	}
	return "pool"
}

func numInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case json.Number:
		i, _ := x.Int64()
		return i
	}
	return 0
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
