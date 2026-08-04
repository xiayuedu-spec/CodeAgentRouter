package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "state.json"), "test-encrypt-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestReserveReleaseSettle(t *testing.T) {
	st := newTestStore(t)
	u, err := st.CreateUser("alice", "Alice", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.ReserveUser(u.ID, 100, 10000, 10000, false, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveUser(u.ID, 100, 10000, 10000, false, now); err != nil {
		t.Fatal(err)
	}
	st.SettleUser(u.ID, 100, 150)
	c := st.UserCounters(u.ID)
	if c.Day.Value != 250 {
		t.Fatalf("day = %d, want 250", c.Day.Value)
	}
	st.ReleaseUser(u.ID, 100)
	c = st.UserCounters(u.ID)
	if c.Day.Value != 150 {
		t.Fatalf("day = %d, want 150", c.Day.Value)
	}
}

func TestDailyGate(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("alice", "Alice", "hash", true)
	now := time.Now()
	if err := st.ReserveUser(u.ID, 600, 10000, 1000, false, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveUser(u.ID, 500, 10000, 1000, false, now); !errors.Is(err, ErrDailyQuota) {
		t.Fatalf("err = %v, want ErrDailyQuota", err)
	}
}

func TestHourlyGateOnlyDuringWorkingHours(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("alice", "Alice", "hash", true)
	now := time.Now()
	if err := st.ReserveUser(u.ID, 600, 1000, 100000, true, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveUser(u.ID, 500, 1000, 100000, true, now); !errors.Is(err, ErrHourlyQuota) {
		t.Fatalf("err = %v, want ErrHourlyQuota", err)
	}
	if err := st.ReserveUser(u.ID, 500, 1000, 100000, false, now); err != nil {
		t.Fatalf("hourly should not block outside working hours: %v", err)
	}
}

func TestKeyCounterAndInFlight(t *testing.T) {
	st := newTestStore(t)
	u, _ := st.CreateUser("alice", "Alice", "hash", true)
	k, err := st.CreateUpstreamKey(u.ID, "deepseek", "https://api.deepseek.com", "sk-secret", nil, true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveKey(k.ID, 100, 1000, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveKey(k.ID, 950, 1000, true); !errors.Is(err, ErrHourlyQuota) {
		t.Fatalf("err = %v, want ErrHourlyQuota", err)
	}
	if got := st.InFlight(k.ID); got != 1 {
		t.Fatalf("in flight = %d, want 1", got)
	}
	st.SettleKey(k.ID, 100, 80)
	if c := st.KeyHourCounter(k.ID); c.Value != 80 {
		t.Fatalf("key hour = %d, want 80", c.Value)
	}
	if got := st.InFlight(k.ID); got != 0 {
		t.Fatalf("in flight = %d, want 0", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := New(path, "test-encrypt-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateUser("alice", "Alice", "hash", true)
	_, _ = st.AddAccessKey(u.ID)
	_, _ = st.CreateUpstreamKey(u.ID, "deepseek", "https://api.deepseek.com", "sk-secret", nil, true, 0)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, err := New(path, "test-encrypt-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st2.GetUser(u.ID) == nil {
		t.Fatal("user not persisted")
	}
	keys := st2.ListUpstreamKeys(u.ID)
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	plain, err := st2.DecryptAPIKey(keys[0])
	if err != nil || plain != "sk-secret" {
		t.Fatalf("decrypt = %q, err %v", plain, err)
	}
}
