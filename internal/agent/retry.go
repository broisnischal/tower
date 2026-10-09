package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"tower/internal/tmux"
)

// BackoffBase is the first wait; tests shorten it.
var BackoffBase = 5 * time.Second

// DefaultMaxRetries is how many times tower says "continue" after a turn
// dies on an API or network error, unless @tower_retry_max says otherwise.
const DefaultMaxRetries = 8

// RetryLimit is @tower_retry_max, or DefaultMaxRetries.
func RetryLimit() int {
	if n, err := strconv.Atoi(tmux.Option("@tower_retry_max", "")); err == nil && n > 0 {
		return n
	}
	return DefaultMaxRetries
}

// Backoff is the wait before retry n (counting from 1): 5s, 10s, 20s, 40s
// and so on, doubling up to five minutes.
func Backoff(n int) time.Duration {
	d := BackoffBase
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// Claude Code's error types that a retry can fix, as it names them in the
// StopFailure hook and in the transcript.
var retryable = map[string]string{
	"server_error":      "server or network error",
	"overloaded":        "API overloaded",
	"unknown":           "unknown API error",
	"max_output_tokens": "reply hit the output limit",
	"rate_limit":        "rate limited",
}

// ...and the ones it cannot.
var blocked = map[string]string{
	"authentication_failed":  "login required: run /login",
	"oauth_org_not_allowed":  "this org does not allow OAuth",
	"account_on_hold":        "account on hold",
	"verification_required":  "account verification required",
	"billing_error":          "billing problem",
	"invalid_request":        "the API rejected the request",
	"model_not_found":        "model not found",
	"cloud_credential_error": "cloud credentials failed",
}

// Text that marks an error worth retrying when no error type came with it
// (headless threads, Codex).
var transient = regexp.MustCompile(`(?i)overloaded|\b(429|500|502|503|504|529)\b|connection|timed? ?out|network|econnreset|econnrefused|fetch failed|socket|stream (error|disconnected)|internal server error|temporarily unavailable|server_error|try again`)

var resetsAt = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)

// Retryable decides whether a failed turn is worth retrying. kind is Claude
// Code's error type when it gave one. For a usage limit with a reset time
// it returns when to try again; otherwise until is zero and the caller
// backs off.
func Retryable(kind, text string, now time.Time) (ok bool, until time.Time, reason string) {
	if r, bad := blocked[kind]; bad {
		return false, time.Time{}, r
	}
	if kind == "rate_limit" || strings.Contains(strings.ToLower(text), "limit") {
		if m := resetsAt.FindStringSubmatch(text); m != nil {
			at := nextClock(now, m[1], m[2], m[3])
			return true, at.Add(30 * time.Second), "usage limit, resets " + at.Format("3:04pm")
		}
		if kind == "rate_limit" {
			return true, time.Time{}, retryable[kind]
		}
	}
	if r, good := retryable[kind]; good {
		return true, time.Time{}, r
	}
	if kind == "" && transient.MatchString(text) {
		return true, time.Time{}, clip(text, 60)
	}
	if kind == "" {
		return false, time.Time{}, clip(or(text, "the turn failed"), 60)
	}
	return false, time.Time{}, kind
}

// nextClock is the next time the clock reads h:mm am/pm, in local time.
func nextClock(now time.Time, h, m, ampm string) time.Time {
	hour, _ := strconv.Atoi(h)
	minute, _ := strconv.Atoi(m)
	hour %= 12
	if strings.EqualFold(ampm, "pm") {
		hour += 12
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}
