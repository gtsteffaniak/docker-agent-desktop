package main

import (
	"testing"
	"time"
)

func TestIdleExpired(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	idle := 30 * time.Minute
	cases := []struct {
		name string
		last time.Time
		want bool
	}{
		{"just used", now.Add(-time.Minute), false},
		{"29 minutes", now.Add(-29 * time.Minute), false},
		{"exactly 30 minutes", now.Add(-30 * time.Minute), true},
		{"hours", now.Add(-3 * time.Hour), true},
	}
	for _, c := range cases {
		if got := idleExpired(c.last, now, idle); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if idleExpired(now.Add(-24*time.Hour), now, 0) {
		t.Error("a zero idle timeout must never expire")
	}
}

func TestBrowserIdleTimeoutEnv(t *testing.T) {
	t.Setenv("DAD_BROWSER_IDLE_MINUTES", "")
	if got := browserIdleTimeout(); got != 30*time.Minute {
		t.Errorf("default = %v", got)
	}
	t.Setenv("DAD_BROWSER_IDLE_MINUTES", "5")
	if got := browserIdleTimeout(); got != 5*time.Minute {
		t.Errorf("5 minutes = %v", got)
	}
	t.Setenv("DAD_BROWSER_IDLE_MINUTES", "0")
	if got := browserIdleTimeout(); got != 0 {
		t.Errorf("0 disables = %v", got)
	}
	t.Setenv("DAD_BROWSER_IDLE_MINUTES", "-3")
	if got := browserIdleTimeout(); got != 30*time.Minute {
		t.Errorf("negative falls back to the default = %v", got)
	}
}
