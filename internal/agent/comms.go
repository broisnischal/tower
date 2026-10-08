package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Message is one hand-off through tower: an agent assigning a task to
// another, the reply coming back, or a message I sent or forwarded.
type Message struct {
	ID       string `json:"id"`
	Time     int64  `json:"t"`
	Kind     string `json:"kind"` // task | reply | message | forward
	From     string `json:"from"` // agent name, or "me"
	To       string `json:"to"`
	FromPane string `json:"from_pane,omitempty"`
	ToPane   string `json:"to_pane,omitempty"`
	Text     string `json:"text"`
	Re       string `json:"re,omitempty"` // the task a reply answers
	Failed   bool   `json:"failed,omitempty"`
}

func commsPath() string { return filepath.Join(Dir(), "comms.jsonl") }

// LogMessage records a hand-off and returns its id.
func LogMessage(m Message) string {
	if m.ID == "" {
		b := make([]byte, 4)
		rand.Read(b)
		m.ID = hex.EncodeToString(b)
	}
	if m.Time == 0 {
		m.Time = time.Now().Unix()
	}
	m.Text = clip(m.Text, 400)
	os.MkdirAll(Dir(), 0o700)
	f, err := os.OpenFile(commsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return m.ID
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	f.Write(append(b, '\n'))
	return m.ID
}

// Messages returns the most recent hand-offs, oldest first.
func Messages(n int) []Message {
	f, err := os.Open(commsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(0, st.Size()-256<<10)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	var out []Message
	for _, l := range lines {
		var m Message
		if json.Unmarshal([]byte(l), &m) == nil && m.ID != "" {
			out = append(out, m)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// OpenTasks are tasks still waiting for their reply, newest last. Tasks
// older than a day count as abandoned.
func OpenTasks(msgs []Message) []Message {
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Kind == "reply" {
			answered[m.Re] = true
		}
	}
	var out []Message
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for _, m := range msgs {
		if m.Kind == "task" && !answered[m.ID] && m.Time > cutoff {
			out = append(out, m)
		}
	}
	return out
}

// WaitDone waits for the turn started by a message sent at since to finish,
// riding out permission prompts along the way.
func WaitDone(pane string, since int64, timeout time.Duration) (Agent, error) {
	deadline := time.Now().Add(timeout)
	for {
		a, err := Wait(pane, since, time.Until(deadline))
		if err != nil || a.Status != Waiting {
			return a, err
		}
		if time.Now().After(deadline) {
			return a, fmt.Errorf("timed out; %s is still waiting on a prompt", a.Name)
		}
		since = 0 // the turn has started; now wait out the prompt
		time.Sleep(2 * time.Second)
	}
}

// Deliver sends text to an agent, waiting while it sits on a permission
// prompt (a paste would answer it), for up to an hour.
func Deliver(pane, text string) error {
	for start := time.Now(); time.Since(start) < time.Hour; time.Sleep(5 * time.Second) {
		for _, a := range Load(true) {
			if a.Pane != pane {
				continue
			}
			if a.Status == Waiting {
				break
			}
			return Send(a, text, false)
		}
	}
	return fmt.Errorf("could not deliver to %s", pane)
}
