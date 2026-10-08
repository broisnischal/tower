package thread

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"tower/internal/agent"
	"tower/internal/tmux"
)

// RetryArgs is a "continue" a tmux agent is due after an API failure.
type RetryArgs struct {
	Session string `json:"session"`
	Pane    string `json:"pane"`
	At      int64  `json:"at"`      // unix time
	Attempt int    `json:"attempt"` // which failure in a row this answers
}

func retryMessage() string { return tmux.Option("@tower_retry_message", "continue") }

func retryLimit() int {
	if n, err := strconv.Atoi(tmux.Option("@tower_retry_max", "")); err == nil && n > 0 {
		return n
	}
	return agent.DefaultMaxRetries
}

// scheduleRetry arms the timer for a tmux agent's retry, replacing any
// earlier one for the same session.
func (s *server) scheduleRetry(a RetryArgs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.retries[a.Session]; t != nil {
		t.Stop()
	}
	s.retries[a.Session] = time.AfterFunc(max(0, time.Until(time.Unix(a.At, 0))), func() { s.fireRetry(a) })
}

// fireRetry says "continue" to the agent, unless the session moved on
// meanwhile: I typed something, it recovered, or it ended.
func (s *server) fireRetry(a RetryArgs) {
	st, err := agent.Read(a.Session)
	if err != nil || st.RetryAt != a.At || st.Retries != a.Attempt || st.Pane != a.Pane {
		return
	}
	for _, ag := range agent.Load(true) {
		if ag.Pane != a.Pane || ag.SessionID != a.Session {
			continue
		}
		msg := retryMessage()
		if err := agent.Send(ag, msg, false); err != nil {
			log.Printf("retry %s: %v", ag.Name, err)
			return
		}
		agent.AppendLog(a.Session, time.Now().Unix(), fmt.Sprintf("↻ said %q (retry %d)", msg, a.Attempt))
		return
	}
}

// resumeRetries re-arms retries that were due when the daemon last stopped.
func (s *server) resumeRetries() {
	for _, st := range agent.States() {
		if st.RetryAt > 0 {
			s.scheduleRetry(RetryArgs{Session: st.SessionID, Pane: st.Pane, At: st.RetryAt, Attempt: st.Retries})
		}
	}
}

// retryThread backs off and says "continue" to a headless thread whose turn
// died on an error a retry can fix.
func (s *server) retryThread(t *live, text string) {
	now := time.Now()
	ok, until, reason := agent.Retryable("", text, now)
	if !ok || tmux.Option("@tower_retry", "on") == "off" {
		return
	}
	limit := retryLimit()
	var attempt int
	s.update(t, func(th *Thread) { th.Retries++; attempt = th.Retries })
	if attempt > limit {
		t.events <- Event{Kind: KindInfo, Status: "error", Text: fmt.Sprintf("gave up after %d retries: %s", limit, reason)}
		return
	}
	wait := agent.Backoff(attempt)
	if !until.IsZero() {
		wait = until.Sub(now)
	}
	at := now.Add(wait).Unix()
	s.update(t, func(th *Thread) {
		th.RetryAt = at
		th.Activity = fmt.Sprintf("↻ %s · retry %d/%d", reason, attempt, limit)
	})
	t.events <- Event{Kind: KindInfo, Text: fmt.Sprintf("↻ %s; retry %d/%d in %s", reason, attempt, limit, wait.Round(time.Second))}
	time.AfterFunc(wait, func() {
		cur := s.snapshot(t)
		if cur.RetryAt != at || cur.Status == working { // I or the agent moved on
			return
		}
		if err := s.send(t, Message{Text: retryMessage()}); err != nil {
			t.events <- Event{Kind: KindInfo, Status: "error", Text: "retry failed: " + err.Error()}
		}
	})
}
