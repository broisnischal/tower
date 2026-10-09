package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Claude Code hands its status line figures that no hook carries: how big
// the context window is and how full, what the session has cost, and how
// much of the plan's usage limits is gone. `tower statusline` records them
// while it passes the JSON on to the rest of my status line, and headless
// threads add the limits from their rate_limit_event.

// Window is one usage limit: the share used, in percent, and when it resets.
type Window struct {
	Used     float64 `json:"used"`
	ResetsAt int64   `json:"resets_at"`
}

// Limits are the plan's usage limits. They belong to the account, so the
// latest report from any session covers every agent.
type Limits struct {
	FiveHour *Window `json:"five_hour,omitempty"`
	SevenDay *Window `json:"seven_day,omitempty"`
	Updated  int64   `json:"updated"`
	Source   string  `json:"source"` // statusline or headless
}

// Live is one session's figures from its status line.
type Live struct {
	CtxPct  float64 `json:"ctx_pct"`
	CtxSize int     `json:"ctx_size"`
	Cost    float64 `json:"cost"`
	Model   string  `json:"model"`
	Updated int64   `json:"updated"`
}

// Neither file ends in .json: Load reads every *.json in Dir as a session.
func livePath(id string) string { return filepath.Join(Dir(), filepath.Base(id)+".live") }
func limitsPath() string        { return filepath.Join(Dir(), "limits") }

// ReadLive returns a session's status line figures, if its status line ran.
func ReadLive(id string) (Live, bool) {
	var l Live
	b, err := os.ReadFile(livePath(id))
	return l, err == nil && json.Unmarshal(b, &l) == nil
}

// ReadLimits returns the latest usage limits any session reported.
func ReadLimits() (Limits, bool) {
	var l Limits
	b, err := os.ReadFile(limitsPath())
	return l, err == nil && json.Unmarshal(b, &l) == nil
}

// WriteLimits records usage limits.
func WriteLimits(l Limits) { writeAtomic(limitsPath(), l) }

// StatusLine copies Claude Code's status line JSON from r to w unchanged, so
// the rest of the status line sees it as before, and records its figures.
func StatusLine(r io.Reader, w io.Writer) error {
	raw, err := io.ReadAll(io.TeeReader(r, w))
	if err != nil {
		return err
	}
	var in struct {
		SessionID string `json:"session_id"`
		Model     struct {
			ID string `json:"id"`
		} `json:"model"`
		Cost struct {
			Total float64 `json:"total_cost_usd"`
		} `json:"cost"`
		Ctx struct {
			Size int      `json:"context_window_size"`
			Used *float64 `json:"used_percentage"`
		} `json:"context_window"`
		Limits map[string]struct {
			Used     *float64        `json:"used_percentage"`
			ResetsAt json.RawMessage `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil // not mine to judge: the rest of the chain still got it
	}
	now := time.Now().Unix()
	if in.SessionID != "" {
		l := Live{CtxSize: in.Ctx.Size, Cost: in.Cost.Total, Model: in.Model.ID, Updated: now, CtxPct: -1}
		if in.Ctx.Used != nil {
			l.CtxPct = *in.Ctx.Used
		}
		os.MkdirAll(Dir(), 0o700)
		writeAtomic(livePath(in.SessionID), l)
	}
	lim := Limits{Updated: now, Source: "statusline"}
	for name, w := range in.Limits {
		if w.Used == nil {
			continue
		}
		win := &Window{Used: *w.Used, ResetsAt: stamp(w.ResetsAt)}
		switch name {
		case "five_hour":
			lim.FiveHour = win
		case "seven_day":
			lim.SevenDay = win
		}
	}
	if lim.FiveHour != nil || lim.SevenDay != nil {
		WriteLimits(lim)
	}
	return nil
}

// stamp reads a time given as unix seconds, unix milliseconds or RFC 3339.
func stamp(raw json.RawMessage) int64 {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
		raw = json.RawMessage(s)
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return 0
	}
	if n > 1e12 {
		n /= 1000
	}
	return int64(n)
}

// writeAtomic writes v as JSON through a rename, so a reader never sees half.
func writeAtomic(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}
