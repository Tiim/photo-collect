package ratelimit

import (
	"testing"
	"time"
)

func newAt(perMinute int, now *time.Time) *Limiter {
	l := New(perMinute)
	l.now = func() time.Time { return *now }
	return l
}

func TestBurstThenReject(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(3, &now)
	for i := 0; i < 3; i++ {
		if _, ok := l.Allow("a"); !ok {
			t.Fatalf("request %d rejected within burst", i)
		}
	}
	wait, ok := l.Allow("a")
	if ok {
		t.Fatal("4th request allowed")
	}
	if wait < time.Second || wait > time.Minute {
		t.Errorf("retry-after = %v", wait)
	}
	if _, ok := l.Allow("b"); !ok {
		t.Error("other key must have its own bucket")
	}
}

func TestRefillsOverTime(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(60, &now) // 1 token per second
	for i := 0; i < 60; i++ {
		l.Allow("a")
	}
	if _, ok := l.Allow("a"); ok {
		t.Fatal("bucket should be empty")
	}
	now = now.Add(2 * time.Second)
	if _, ok := l.Allow("a"); !ok {
		t.Error("token should have refilled")
	}
}

func TestRejectedRequestsDoNotDeepenTheDebt(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(60, &now)
	for i := 0; i < 60; i++ {
		l.Allow("a")
	}
	for i := 0; i < 1000; i++ {
		l.Allow("a") // hammering while limited
	}
	now = now.Add(2 * time.Second)
	if _, ok := l.Allow("a"); !ok {
		t.Error("rejected requests must not consume tokens")
	}
}

func TestIdleKeysAreEvicted(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(10, &now)
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	if l.Len() != 3 {
		t.Fatalf("len = %d", l.Len())
	}
	now = now.Add(3 * time.Minute)
	l.Allow("d")
	if l.Len() != 1 {
		t.Errorf("idle keys not evicted, len = %d", l.Len())
	}
}

func TestKeyCountIsBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAt(10, &now)
	for i := 0; i < maxKeys+500; i++ {
		l.Allow(string(rune(i)) + "-" + time.Duration(i).String())
	}
	if l.Len() > maxKeys {
		t.Errorf("len = %d, want <= %d", l.Len(), maxKeys)
	}
}

func TestDisabled(t *testing.T) {
	var l *Limiter = New(0)
	if l != nil {
		t.Fatal("New(0) must return nil")
	}
	for i := 0; i < 1000; i++ {
		if _, ok := l.Allow("a"); !ok {
			t.Fatal("nil limiter must allow everything")
		}
	}
}
