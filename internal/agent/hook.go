package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tower/internal/tmux"
)

// Hook records one Claude Code hook event (JSON on r) for the agent running in
// $TMUX_PANE, and returns the session's new state when it wrote one. It is
// wired to every lifecycle event and must stay silent: whatever a
// SessionStart or UserPromptSubmit hook prints lands in the agent's context.
func Hook(r io.Reader) (State, bool) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || os.Getenv("TOWER_THREAD") != "" { // headless threads report through the daemon
		return State{}, false
	}
	var ev struct {
		Event            string         `json:"hook_event_name"`
		SessionID        string         `json:"session_id"`
		Transcript       string         `json:"transcript_path"`
		Cwd              string         `json:"cwd"`
		Source           string         `json:"source"`
		Prompt           string         `json:"prompt"`
		Tool             string         `json:"tool_name"`
		Input            map[string]any `json:"tool_input"`
		Message          string         `json:"message"`
		NotificationType string         `json:"notification_type"`
		AgentID          string         `json:"agent_id"`
		Error            string         `json:"error"` // StopFailure: Claude Code's error type
		ErrorDetails     string         `json:"error_details"`
		LastMessage      string         `json:"last_assistant_message"`
	}
	if json.NewDecoder(r).Decode(&ev) != nil || ev.SessionID == "" {
		return State{}, false
	}
	// Subagents share the parent's session id. Only their tool calls are
	// worth showing; their Stop would end the parent's turn early.
	if ev.AgentID != "" && ev.Event != "PreToolUse" && ev.Event != "PostToolUse" {
		return State{}, false
	}
	if ev.Event == "SessionEnd" {
		Remove(ev.SessionID)
		tmux.Run("set", "-wu", "-t", pane, "@tower_state")
		return State{}, false
	}

	s, _ := Read(ev.SessionID)
	prev := s.Status
	now := time.Now().Unix()
	s.SessionID, s.Pane, s.Updated = ev.SessionID, pane, now
	if ev.Cwd != "" {
		s.Cwd = ev.Cwd
	}
	if ev.Transcript != "" {
		s.Transcript = ev.Transcript
	}
	if s.Started == 0 {
		s.Started = now
	}

	var line string
	switch ev.Event {
	case "SessionStart":
		s.Status, s.Activity = Idle, "ready"
		line = "session " + or(ev.Source, "start")
	case "UserPromptSubmit":
		s.Status, s.Activity, s.TurnStarted, s.RetryAt, s.Stalled = Working, "thinking", now, 0, false
		s.Prompt = clip(ev.Prompt, 200)
		line = "› " + s.Prompt
	case "PreToolUse":
		s.Status, s.Activity = Working, toolLabel(ev.Tool, ev.Input)
		if ev.AgentID != "" {
			s.Activity = "↳ " + s.Activity
		}
		line = s.Activity
	case "PostToolUse":
		if s.Status == Waiting { // I approved it; drop the prompt text
			s.Activity = "thinking"
		}
		s.Status = Working
	case "Notification":
		switch {
		case ev.NotificationType == "idle_prompt", ev.NotificationType == "auth_success",
			strings.Contains(ev.Message, "waiting for your input"):
			return State{}, false
		}
		s.Status, s.Activity = Waiting, clip(ev.Message, 120)
		line = "! " + s.Activity
	case "PreCompact":
		s.Activity, line = "compacting context", "compacting"
	case "Stop":
		s.Status, s.Activity, s.Finished = Done, "finished", now
		s.Retries, s.RetryAt = 0, 0
		if visible(pane) {
			s.Status = Idle
		}
		line = "✓ finished"
	case "StopFailure": // the turn died on an API or network error
		s.Finished = now
		line = failed(&s, ev.Error, strings.TrimSpace(ev.ErrorDetails+" "+ev.LastMessage), time.Unix(now, 0))
	default:
		return State{}, false
	}

	if Write(s) != nil {
		return State{}, false
	}
	if line != "" {
		AppendLog(s.SessionID, now, line)
	}
	if s.Status != prev {
		tmux.Run("set", "-w", "-t", pane, "@tower_state", s.Status)
		notify(pane, prev, s)
	}
	return s, true
}

// failed records a turn lost to an API error and, when a retry can fix it,
// when tower should say "continue": 5s, 10s, 20s ... after each failure in a
// row, or when a usage limit resets. Errors a retry cannot fix (a login, a
// billing problem) wait for me instead. It returns the log line.
func failed(s *State, kind, text string, now time.Time) string {
	s.Stalled = false
	ok, until, reason := Retryable(kind, text, now)
	if !ok || tmux.Option("@tower_retry", "on") == "off" {
		s.Status, s.Activity, s.RetryAt = Waiting, "✗ "+reason, 0
		return "✗ " + reason
	}
	limit := RetryLimit()
	s.Retries++
	if s.Retries > limit {
		s.Status, s.RetryAt = Waiting, 0
		s.Activity = fmt.Sprintf("✗ gave up after %d retries: %s", limit, reason)
		return s.Activity
	}
	wait := Backoff(s.Retries)
	if !until.IsZero() {
		wait = until.Sub(now)
	}
	s.Status, s.RetryAt = Working, now.Add(wait).Unix()
	s.Activity = fmt.Sprintf("↻ %s · retry %d/%d", reason, s.Retries, limit)
	return fmt.Sprintf("↻ %s; retry %d/%d in %s", reason, s.Retries, limit, wait.Round(time.Second))
}

// notify flashes a tmux message when an agent I am not looking at needs me or
// finishes. Turn it off with: set -g @tower_notify off
func notify(pane, prev string, s State) {
	switch {
	case s.Status == Waiting:
	case s.Status == Done && prev == Working:
	default:
		return
	}
	if tmux.Option("@tower_notify", "on") == "off" {
		return
	}
	who := tmux.Run("display", "-p", "-t", pane, "#{?@tower_name,#{@tower_name},#{window_name}} (#{session_name}:#{window_index})")
	msg := Icon[s.Status] + " " + who + ": " + s.Activity
	tmux.Run("display-message", "-d", "4000", strings.ReplaceAll(msg, "#", "##"))
}

func visible(pane string) bool {
	return tmux.Run("display", "-p", "-t", pane, "#{&&:#{window_active},#{session_attached}}") == "1"
}

// toolLabel turns a tool call into a short line such as "Edit refresh.ts".
func toolLabel(tool string, in map[string]any) string {
	str := func(k string) string { v, _ := in[k].(string); return v }
	target := ""
	if p := or(str("file_path"), str("notebook_path")); p != "" {
		target = filepath.Base(p)
	} else {
		target = or(str("command"), str("pattern"), str("url"), str("query"), str("description"), str("skill"), str("prompt"))
	}
	return clip(ToolName(tool)+" "+target, 100)
}

// ToolName makes an MCP tool name readable: mcp__pcvision__screenshot
// becomes "pcvision › screenshot".
func ToolName(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		server, name, found := strings.Cut(rest, "__")
		if found {
			return strings.TrimPrefix(server, "claude_ai_") + " › " + strings.ReplaceAll(name, "_", " ")
		}
		return rest
	}
	return tool
}

// clip collapses whitespace and cuts s to n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
