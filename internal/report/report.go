package report

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one line of the request JSONL log.
type Entry struct {
	TS               string `json:"ts"`
	UserID           string `json:"user_id"`
	AccessKey        string `json:"access_key"`
	RequestID        string `json:"request_id"`
	Model            string `json:"model"`
	Stream           bool   `json:"stream"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	UpstreamKeyID    string `json:"upstream_key_id"`
	RouteType        string `json:"route_type"`
	Status           int    `json:"status"`
	Error            string `json:"error"`
	LatencyMs        int64  `json:"latency_ms"`
	ClientIP         string `json:"client_ip"`
}

// Logger appends one JSON line per request to daily files.
type Logger struct {
	mu         sync.Mutex
	dir        string
	day        string
	file       *os.File
	invalidate func(month string)
}

func NewLogger(dir string, invalidate func(month string)) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{dir: dir, invalidate: invalidate}, nil
}

func (l *Logger) Write(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	day := todayFrom(e.TS)
	if err := l.openLocked(day); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := l.file.Write(append(data, '\n')); err != nil {
		return err
	}
	if l.invalidate != nil {
		l.invalidate(day[:7])
	}
	return nil
}

func (l *Logger) openLocked(day string) error {
	if l.file != nil && l.day == day {
		return nil
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	path := filepath.Join(l.dir, "requests-"+day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	l.day = day
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func todayFrom(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return time.Now().Format("2006-01-02")
}

// Row is one user x model x day aggregation row.
type Row struct {
	UserID           string  `json:"user_id"`
	Model            string  `json:"model"`
	Day              string  `json:"day"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Errors           int64   `json:"errors"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	MaxLatencyMs     int64   `json:"max_latency_ms"`
}

type Summary struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Errors           int64   `json:"errors"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	MaxLatencyMs     int64   `json:"max_latency_ms"`
}

type Report struct {
	Month   string  `json:"month"`
	Summary Summary `json:"summary"`
	Rows    []Row   `json:"rows"`
}

type rowAcc struct {
	req, prompt, completion, total, errs, latSum, latMax, latMin int64
}

// Service builds monthly reports and caches them until a new log line is
// written for that month.
type Service struct {
	dir   string
	mu    sync.Mutex
	cache map[string]*Report
}

func NewService(dir string) *Service {
	return &Service{dir: dir, cache: map[string]*Report{}}
}

func (s *Service) Invalidate(month string) {
	s.mu.Lock()
	delete(s.cache, month)
	s.mu.Unlock()
}

func (s *Service) Monthly(month string) (*Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.cache[month]; ok {
		return r, nil
	}
	r, err := buildReport(month, s.dir)
	if err != nil {
		return nil, err
	}
	s.cache[month] = r
	return r, nil
}

func buildReport(month, dir string) (*Report, error) {
	prefix := "requests-" + month + "-"
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	accs := map[string]*rowAcc{}
	for _, path := range matches {
		if err := aggregateFile(path, accs); err != nil {
			return nil, err
		}
	}
	r := &Report{Month: month}
	keys := make([]string, 0, len(accs))
	for k := range accs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := accs[k]
		parts := strings.SplitN(k, "\x00", 3)
		avg := 0.0
		if a.req > 0 {
			avg = float64(a.latSum) / float64(a.req)
		}
		row := Row{
			UserID:           parts[0],
			Model:            parts[1],
			Day:              parts[2],
			Requests:         a.req,
			PromptTokens:     a.prompt,
			CompletionTokens: a.completion,
			TotalTokens:      a.total,
			Errors:           a.errs,
			AvgLatencyMs:     round1(avg),
			MaxLatencyMs:     a.latMax,
		}
		r.Rows = append(r.Rows, row)
		r.Summary.Requests += row.Requests
		r.Summary.PromptTokens += row.PromptTokens
		r.Summary.CompletionTokens += row.CompletionTokens
		r.Summary.TotalTokens += row.TotalTokens
		r.Summary.Errors += row.Errors
		r.Summary.MaxLatencyMs = max64(r.Summary.MaxLatencyMs, row.MaxLatencyMs)
	}
	if r.Summary.Requests > 0 {
		r.Summary.AvgLatencyMs = round1(float64(sumLatency(accs)) / float64(r.Summary.Requests))
	}
	return r, nil
}

func sumLatency(accs map[string]*rowAcc) int64 {
	var total int64
	for _, a := range accs {
		total += a.latSum
	}
	return total
}

func aggregateFile(path string, accs map[string]*rowAcc) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		day := todayFrom(e.TS)
		if len(day) > 7 && day[:7] != pathMonth(path) {
			continue
		}
		key := e.UserID + "\x00" + e.Model + "\x00" + day
		a := accs[key]
		if a == nil {
			a = &rowAcc{latMin: -1}
			accs[key] = a
		}
		a.req++
		a.prompt += e.PromptTokens
		a.completion += e.CompletionTokens
		a.total += e.TotalTokens
		if e.Status >= 400 || e.Error != "" {
			a.errs++
		}
		a.latSum += e.LatencyMs
		if e.LatencyMs > a.latMax {
			a.latMax = e.LatencyMs
		}
		if a.latMin < 0 || e.LatencyMs < a.latMin {
			a.latMin = e.LatencyMs
		}
	}
	return sc.Err()
}

func pathMonth(path string) string {
	name := filepath.Base(path)
	if len(name) >= len("requests-YYYY-MM") {
		return name[len("requests-"):len("requests-YYYY-MM")]
	}
	return ""
}

func (r *Report) CSV() string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"user_id", "model", "day", "requests", "prompt_tokens", "completion_tokens", "total_tokens", "errors", "avg_latency_ms", "max_latency_ms"})
	for _, row := range r.Rows {
		_ = w.Write([]string{
			row.UserID, row.Model, row.Day,
			strconv.FormatInt(row.Requests, 10),
			strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10),
			strconv.FormatInt(row.TotalTokens, 10),
			strconv.FormatInt(row.Errors, 10),
			strconv.FormatFloat(row.AvgLatencyMs, 'f', 1, 64),
			strconv.FormatInt(row.MaxLatencyMs, 10),
		})
	}
	w.Flush()
	return b.String()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
