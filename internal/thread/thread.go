// Package thread runs agents headlessly. tower starts Claude Code or Codex
// itself, speaks their structured protocols, and turns everything they do into
// one stream of Events that the dashboard folds into a chat.
//
// A daemon (tower serve) owns the agent processes so they outlive the UI.
// Clients talk to it over a Unix socket with newline-delimited JSON.
package thread

import (
	"strings"

	"tower/internal/agent"
)

// Event kinds.
const (
	KindSession  = "session"  // Text: engine session id for resume; Input: model
	KindUser     = "user"     // Text: what I sent
	KindText     = "text"     // ID, Role (assistant|reasoning), Text: the full block
	KindDelta    = "delta"    // ID, Role, Text: a chunk to append; streamed, never stored
	KindTool     = "tool"     // ID, Tool, Input, Status, Output, Diff: upserted by ID
	KindApproval = "approval" // ID, Tool, Input, Diff, Status: pending|allowed|denied
	KindTurn     = "turn"     // Status: started|done|interrupted|failed; Text: error; Usage
	KindInfo     = "info"     // Text: a notice; Status "error" for failures
)

// Tool statuses.
const (
	ToolRunning  = "running"
	ToolOK       = "ok"
	ToolFailed   = "failed"
	ToolDeclined = "declined"
)

// Approval statuses.
const (
	Pending = "pending"
	Allowed = "allowed"
	Denied  = "denied"
)

// Turn statuses.
const (
	TurnStarted     = "started"
	TurnDone        = "done"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
)

// Event is one normalized thing that happened in a thread.
type Event struct {
	Seq    int64  `json:"seq,omitempty"`
	Time   int64  `json:"t,omitempty"` // unix ms
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Role   string `json:"role,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Input  string `json:"input,omitempty"`  // one-line summary of the tool input
	Output string `json:"output,omitempty"` // tool output, trimmed
	Diff   string `json:"diff,omitempty"`   // unified diff of a file change
	Status string `json:"status,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Usage is token and cost accounting. On a turn event it covers that turn;
// on a Thread it is the running total, with Context as of the last turn.
type Usage struct {
	Input   int     `json:"input,omitempty"`
	Output  int     `json:"output,omitempty"`
	Context int     `json:"context,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Thread is one headless agent conversation.
type Thread struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Cwd         string `json:"cwd"`
	Model       string `json:"model,omitempty"`
	Mode        string `json:"mode,omitempty"`   // default (ask me), acceptEdits, plan, bypassPermissions
	Branch      string `json:"branch,omitempty"` // set when it runs in its own worktree
	Resume      string `json:"resume,omitempty"` // engine session id to pick the conversation back up
	Status      string `json:"status"`           // agent.Idle, Working, Waiting or Done
	Live        bool   `json:"live"`             // engine process running
	Activity    string `json:"activity,omitempty"`
	Prompt      string `json:"prompt,omitempty"` // last thing I sent, clipped
	Pending     int    `json:"pending,omitempty"`
	Usage       Usage  `json:"usage"`
	Created     int64  `json:"created"`
	Updated     int64  `json:"updated"`
	TurnStarted int64  `json:"turn_started,omitempty"`
	Retries     int    `json:"retries,omitempty"`  // failed turns in a row
	RetryAt     int64  `json:"retry_at,omitempty"` // when tower says "continue"; 0 when none is due
}

// Chat folds events into the entries the UI draws, in order, merging updates
// that share an ID (streamed text, a tool finishing, an approval answered).
type Chat struct {
	Items []Event
	index map[string]int
}

// Apply adds one event to the chat.
func (c *Chat) Apply(e Event) {
	if c.index == nil {
		c.index = map[string]int{}
	}
	switch e.Kind {
	case KindDelta:
		key := KindText + ":" + e.ID
		if i, ok := c.index[key]; ok {
			c.Items[i].Text += e.Text
			return
		}
		e.Kind = KindText
		c.add(key, e)
	case KindText, KindTool, KindApproval:
		key := e.Kind + ":" + e.ID
		if i, ok := c.index[key]; ok && e.ID != "" {
			c.Items[i] = merge(c.Items[i], e)
			return
		}
		c.add(key, e)
	case KindSession:
	default:
		c.Items = append(c.Items, e)
	}
}

func (c *Chat) add(key string, e Event) {
	c.index[key] = len(c.Items)
	c.Items = append(c.Items, e)
}

// merge lays the non-empty fields of an update over an earlier event.
func merge(old, e Event) Event {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&old.Role, e.Role)
	set(&old.Text, e.Text)
	set(&old.Tool, e.Tool)
	set(&old.Input, e.Input)
	set(&old.Output, e.Output)
	set(&old.Diff, e.Diff)
	set(&old.Status, e.Status)
	return old
}

// PendingApproval returns the oldest approval still waiting on me.
func (c *Chat) PendingApproval() (Event, bool) {
	for _, it := range c.Items {
		if it.Kind == KindApproval && it.Status == Pending {
			return it, true
		}
	}
	return Event{}, false
}

// Decision answers an approval.
type Decision string

const (
	Allow  Decision = "allow"  // this once
	Always Decision = "always" // this and the same kind of call for the rest of the session
	Deny   Decision = "deny"
)

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// statuses shared with tmux agents, so the UI and status bar treat both alike.
const (
	idle    = agent.Idle
	working = agent.Working
	waiting = agent.Waiting
	done    = agent.Done
)

// LastReply is what the agent said in its latest turn: every assistant text
// after the last message I sent.
func LastReply(events []Event) string {
	var chat Chat
	for _, e := range events {
		chat.Apply(e)
	}
	var out []string
	for _, it := range chat.Items {
		switch {
		case it.Kind == KindUser:
			out = nil
		case it.Kind == KindText && it.Role == "assistant":
			out = append(out, it.Text)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n\n"))
}
