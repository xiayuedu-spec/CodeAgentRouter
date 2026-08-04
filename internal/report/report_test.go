package report

import "testing"

func TestMonthlyReportAggregation(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	logger, err := NewLogger(dir, svc.Invalidate)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{TS: "2026-08-03T10:00:00+08:00", UserID: "u1", Model: "m1", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Status: 200, LatencyMs: 100},
		{TS: "2026-08-03T10:01:00+08:00", UserID: "u1", Model: "m1", PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30, Status: 500, LatencyMs: 200},
		{TS: "2026-08-04T09:00:00+08:00", UserID: "u2", Model: "m2", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, Status: 200, LatencyMs: 50},
	}
	for _, e := range entries {
		if err := logger.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	rep, err := svc.Monthly("2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Requests != 3 || rep.Summary.TotalTokens != 48 || rep.Summary.Errors != 1 {
		t.Fatalf("summary = %+v", rep.Summary)
	}
	if rep.Summary.MaxLatencyMs != 200 {
		t.Fatalf("max latency = %d, want 200", rep.Summary.MaxLatencyMs)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rep.Rows))
	}
	if rep.Rows[0].Requests != 2 || rep.Rows[0].TotalTokens != 45 || rep.Rows[0].Errors != 1 {
		t.Fatalf("u1 row = %+v", rep.Rows[0])
	}
}

func TestReportCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	logger, _ := NewLogger(dir, svc.Invalidate)
	_ = logger.Write(Entry{TS: "2026-07-01T00:00:00+08:00", UserID: "u1", Model: "m1", TotalTokens: 10, Status: 200})
	rep, err := svc.Monthly("2026-07")
	if err != nil || rep.Summary.Requests != 1 {
		t.Fatalf("initial report = %+v, err %v", rep, err)
	}
	_ = logger.Write(Entry{TS: "2026-07-02T00:00:00+08:00", UserID: "u1", Model: "m1", TotalTokens: 20, Status: 200})
	rep, err = svc.Monthly("2026-07")
	if err != nil || rep.Summary.Requests != 2 {
		t.Fatalf("after invalidation report = %+v, err %v", rep, err)
	}
	_ = logger.Close()
}
