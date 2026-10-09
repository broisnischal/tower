package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tower/internal/tmux"
)

// Claude Code retries a failed API call by itself, up to ten times with
// growing waits, and fires no hook meanwhile: the agent just sits there with
// the error on screen. Tower spots that wait, presses Esc, and says
// "continue" 5 seconds later through the same retry path as a turn that died
// on an API error.

// apiWait matches what Claude Code shows while it waits to retry
// ("... · Retrying in 18s · attempt 4/10") and when a request got no answer.
var apiWait = regexp.MustCompile(`Retrying in [^·\n]{1,24}· attempt \d+/\d+|No response from the API after`)

// OnRetry hands a due retry to the daemon. main sets it, since this package
// can't import the daemon's.
var OnRetry func(State)

const (
	stallQuiet = 8               // seconds without a hook event before tower reads the screen
	stallPeek  = 3 * time.Second // how often it reads it, per agent, across all tower processes
)

// stalled returns the API wait line on the agent's screen, if it shows one.
// It is only called between tool calls, so a command's output is not on the
// last lines; and it only reads the status lines right above the input box,
// where Claude Code draws the wait, so the same words anywhere else on
// screen (a log, this file) don't count.
func stalled(s State) (string, bool) {
	mark := filepath.Join(Dir(), filepath.Base(s.SessionID)+".peek")
	if st, err := os.Stat(mark); err == nil && time.Since(st.ModTime()) < stallPeek {
		return "", false
	}
	os.WriteFile(mark, nil, 0o600)
	return waitLine(tmux.Run("capture-pane", "-p", "-t", s.Pane))
}

// waitLine looks at the last three lines above the input box's top rule.
func waitLine(screen string) (string, bool) {
	lines := strings.Split(screen, "\n")
	top := -1
	for i, n := len(lines)-1, 0; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); len([]rune(t)) >= 20 && strings.Trim(t, "─") == "" {
			if n++; n == 2 {
				top = i
				break
			}
		}
	}
	for i, seen := top-1, 0; top > 0 && i >= 0 && seen < 3; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if seen++; apiWait.MatchString(l) {
			return l, true
		}
	}
	return "", false
}

// retryStall interrupts an agent stuck in an API wait and schedules its
// "continue". Every tower process runs Load, and a second Esc would open
// Claude Code's rewind menu, so it takes a lock and re-reads the state
// before acting. It reports the state it wrote.
func retryStall(s State, line string, now int64) (State, bool) {
	lock := filepath.Join(Dir(), filepath.Base(s.SessionID)+".stall")
	if st, err := os.Stat(lock); err == nil && time.Since(st.ModTime()) > time.Minute {
		os.Remove(lock) // left behind by a process that died holding it
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return s, false
	}
	f.Close()
	defer os.Remove(lock)
	cur, err := Read(s.SessionID)
	if err != nil || cur.Status != Working || cur.RetryAt != 0 || cur.Updated != s.Updated {
		return s, false // another process got here first, or the agent moved on
	}
	limit := RetryLimit()
	cur.Retries++
	cur.Updated = now
	if cur.Retries > limit {
		cur.Status, cur.Activity = Waiting, fmt.Sprintf("✗ gave up after %d retries: API not answering", limit)
		Write(cur)
		AppendLog(cur.SessionID, now, cur.Activity)
		return cur, true
	}
	tmux.Run("send-keys", "-t", cur.Pane, "Escape")
	cur.RetryAt, cur.Stalled = now+int64(BackoffBase/time.Second), true
	cur.Activity = fmt.Sprintf("↻ API stalled · retry %d/%d", cur.Retries, limit)
	Write(cur)
	AppendLog(cur.SessionID, now, fmt.Sprintf("↻ stuck on the API (%s): Esc, retry %d/%d in %s",
		clip(line, 80), cur.Retries, limit, BackoffBase))
	if OnRetry != nil {
		OnRetry(cur)
	}
	return cur, true
}

// Draft is the text in an agent's input box, read off its screen: what sits
// between the last two rules, after the ❯. An Esc before the first reply
// puts my prompt back there, where a pasted "continue" would be tacked on.
func Draft(pane string) string { return draftOf(tmux.Run("capture-pane", "-p", "-t", pane)) }

func draftOf(screen string) string {
	lines := strings.Split(screen, "\n")
	var rules []int
	for i, l := range lines {
		if t := strings.TrimSpace(l); len([]rune(t)) >= 20 && strings.Trim(t, "─") == "" {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return ""
	}
	var out []string
	for _, l := range lines[rules[len(rules)-2]+1 : rules[len(rules)-1]] {
		out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "❯")))
	}
	d := strings.TrimSpace(strings.Join(out, "\n"))
	if strings.HasPrefix(d, `Try "`) { // the placeholder of an empty box
		return ""
	}
	return d
}
