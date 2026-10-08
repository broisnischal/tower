package agent

import (
	"testing"
	"time"
)

func TestBackoffDoublesUpToFiveMinutes(t *testing.T) {
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300}
	for i, w := range want {
		if got := Backoff(i + 1); got != w*time.Second {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
}

func TestRetryable(t *testing.T) {
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.Local)
	cases := []struct {
		kind, text string
		ok         bool
		until      string // HH:MM when a reset time applies
	}{
		{"server_error", "API Error: Connection lost mid-response.", true, ""},
		{"overloaded", "", true, ""},
		{"authentication_failed", "Not logged in · Please run /login", false, ""},
		{"rate_limit", "You've hit your session limit · resets 7:55pm (Asia/Kathmandu)", true, "19:55"},
		{"rate_limit", "You've hit your session limit · resets 2am", true, "02:00"},
		{"", "API Error: 529 overloaded_error", true, ""},
		{"", "stream disconnected before completion", true, ""},
		{"", "prompt is too long", false, ""},
	}
	for _, c := range cases {
		ok, until, reason := Retryable(c.kind, c.text, now)
		if ok != c.ok {
			t.Errorf("%q %q: ok = %v (%s)", c.kind, c.text, ok, reason)
		}
		if c.until != "" && until.Format("15:04") != c.until {
			t.Errorf("%q: until = %v, want %s", c.text, until, c.until)
		}
	}
}
