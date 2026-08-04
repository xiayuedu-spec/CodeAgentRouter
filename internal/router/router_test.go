package router

import (
	"testing"

	"codeagentrouter/internal/model"
)

func key(id, user string, models []string, limit int64) *model.UpstreamKey {
	return &model.UpstreamKey{ID: id, UserID: user, Models: models, Enabled: true, HourlyLimit: limit}
}

func TestSelectPrefersOwnKey(t *testing.T) {
	keys := []*model.UpstreamKey{
		key("k_own", "u1", nil, 0),
		key("k_pool", "u2", nil, 0),
	}
	usage := func(string) int64 { return 0 }
	inflight := func(string) int { return 0 }
	got, err := Select(keys, "u1", "deepseek-chat", true, 1000, nil, usage, inflight)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "k_own" {
		t.Fatalf("selected %s, want k_own", got.ID)
	}
}

func TestSelectFallsBackToPoolWhenOwnExhausted(t *testing.T) {
	keys := []*model.UpstreamKey{
		key("k_own", "u1", nil, 0),
		key("k_pool", "u2", nil, 0),
	}
	usage := func(id string) int64 {
		if id == "k_own" {
			return 1000
		}
		return 100
	}
	inflight := func(string) int { return 0 }
	got, err := Select(keys, "u1", "deepseek-chat", true, 1000, nil, usage, inflight)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "k_pool" {
		t.Fatalf("selected %s, want k_pool", got.ID)
	}
}

func TestSelectHourlyExhausted(t *testing.T) {
	keys := []*model.UpstreamKey{key("k_own", "u1", nil, 0), key("k_pool", "u2", nil, 0)}
	usage := func(string) int64 { return 1000 }
	inflight := func(string) int { return 0 }
	if _, err := Select(keys, "u1", "deepseek-chat", true, 1000, nil, usage, inflight); err != ErrHourlyExhausted {
		t.Fatalf("err = %v, want ErrHourlyExhausted", err)
	}
}

func TestSelectOutsideWorkingHoursIgnoresLimit(t *testing.T) {
	keys := []*model.UpstreamKey{key("k_own", "u1", nil, 0)}
	usage := func(string) int64 { return 1000 }
	inflight := func(string) int { return 0 }
	got, err := Select(keys, "u1", "deepseek-chat", false, 1000, nil, usage, inflight)
	if err != nil || got.ID != "k_own" {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestSelectFiltersByModel(t *testing.T) {
	keys := []*model.UpstreamKey{
		key("k_gpt", "u2", []string{"gpt-4o"}, 0),
		key("k_ds", "u1", []string{"deepseek-chat"}, 0),
	}
	usage := func(string) int64 { return 0 }
	inflight := func(string) int { return 0 }
	got, err := Select(keys, "u1", "gpt-4o", true, 1000, nil, usage, inflight)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "k_gpt" {
		t.Fatalf("selected %s, want k_gpt", got.ID)
	}
	if _, err := Select(keys, "u1", "claude", true, 1000, nil, usage, inflight); err != ErrNoKey {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestSelectTieBreaksByInFlight(t *testing.T) {
	keys := []*model.UpstreamKey{key("k_a", "u1", nil, 0), key("k_b", "u1", nil, 0)}
	usage := func(string) int64 { return 0 }
	inflight := func(id string) int {
		if id == "k_a" {
			return 3
		}
		return 0
	}
	got, _ := Select(keys, "u1", "m", true, 1000, nil, usage, inflight)
	if got.ID != "k_b" {
		t.Fatalf("selected %s, want k_b", got.ID)
	}
}
