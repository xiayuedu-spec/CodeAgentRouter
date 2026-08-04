package ratelimit

import (
	"testing"
	"time"
)

func TestSlidingWindow(t *testing.T) {
	l := New(3, time.Minute)
	now := time.Unix(1700000000, 0)
	for i := 0; i < 3; i++ {
		ok, _ := l.Allow("u1", now.Add(time.Duration(i)*time.Second))
		if !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if ok, _ := l.Allow("u1", now.Add(3*time.Second)); ok {
		t.Fatal("4th request should be denied")
	}
	if ok, _ := l.Allow("u1", now.Add(2*time.Minute)); !ok {
		t.Fatal("request after window should be allowed")
	}
	if ok, _ := l.Allow("u2", now); !ok {
		t.Fatal("different user should not be limited")
	}
}
