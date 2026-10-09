// Package agent tracks the Claude Code agents running in my tmux panes.
//
// The hook (tower hook) writes one JSON file per Claude session into Dir.
// Load joins those files with tmux list-panes, so the sidebar, the status
// segment and the CLI all read the same picture.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tower/internal/tmux"
)

const (
	Idle    = "idle"    // never prompted, or finished and already seen
	Done    = "done"    // finished a turn I have not looked at yet
	Working = "working" // running a turn
	Waiting = "waiting" // blocked on me: a permission prompt or a question
)

var (
	Rank      = map[string]int{Idle: 0, Done: 1, Working: 2, Waiting: 3}
	Icon      = map[string]string{Idle: "○", Done: "✓", Working: "●", Waiting: "◐"}
	TmuxColor = map[string]string{Idle: "brightblack", Done: "green", Working: "yellow", Waiting: "red"}
)

// State is what the hook knows about one Claude Code session.
type State struct {
	SessionID   string `json:"session_id"`
	Pane        string `json:"pane"`
	Cwd         string `json:"cwd"`
	Transcript  string `json:"transcript"`
	Status      string `json:"state"`
	Activity    string `json:"activity"`
	Prompt      string `json:"prompt,omitempty"`
	Started     int64  `json:"started"`
	Updated     int64  `json:"updated"`
	TurnStarted int64  `json:"turn_started,omitempty"`
	Finished    int64  `json:"finished,omitempty"`
	Retries     int    `json:"retries,omitempty"`  // API failures in a row
	RetryAt     int64  `json:"retry_at,omitempty"` // when tower says "continue"; 0 when none is due
	Stalled     bool   `json:"stalled,omitempty"`  // that retry follows an API wait tower interrupted
}

// Agent is a live session joined with the tmux pane it runs in.
type Agent struct {
	State
	Win  tmux.Pane
	Name string
}

// Where is the agent's tmux address, session:window.pane.
func (a Agent) Where() string {
	return a.Win.Session + ":" + a.Win.WindowIndex + "." + a.Win.Index
}

// Dir holds a <session>.json and <session>.log per agent. It sits in the
// runtime dir, so a reboot clears it along with the tmux server.
func Dir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "tower")
}

func statePath(id string) string { return filepath.Join(Dir(), filepath.Base(id)+".json") }

// LogPath is the activity log of one session: "<unix time>\t<text>" lines.
func LogPath(id string) string { return filepath.Join(Dir(), filepath.Base(id)+".log") }

// Read loads one session's state. A missing file gives a zero State.
func Read(id string) (State, error) {
	var s State
	b, err := os.ReadFile(statePath(id))
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}

// Write saves a session's state atomically, since the hook, the sidebar and
// the status segment all touch these files.
func Write(s State) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", statePath(s.SessionID), os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(s.SessionID))
}

// States reads every session's state file.
func States() []State {
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	var out []State
	for _, f := range files {
		if s, err := Read(strings.TrimSuffix(filepath.Base(f), ".json")); err == nil && s.SessionID != "" {
			out = append(out, s)
		}
	}
	return out
}

// Remove forgets a session.
func Remove(id string) {
	os.Remove(statePath(id))
	os.Remove(LogPath(id))
}

// AppendLog adds one line to a session's activity log.
func AppendLog(id string, t int64, text string) {
	f, err := os.OpenFile(LogPath(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d\t%s\n", t, text)
}

// LogLine is one entry of an activity log.
type LogLine struct {
	Time  time.Time
	Text  string
	Agent string // set when lines from several agents are merged
}

// Tail returns the last n lines of a session's activity log.
func Tail(id string, n int) []LogLine {
	if n <= 0 {
		return nil
	}
	f, err := os.Open(LogPath(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(0, st.Size()-16<<10)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is probably cut
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]LogLine, 0, len(lines))
	for _, l := range lines {
		ts, text, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		sec, _ := strconv.ParseInt(ts, 10, 64)
		out = append(out, LogLine{Time: time.Unix(sec, 0), Text: text})
	}
	return out
}

var shells = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true, "ksh": true, "nu": true, "tcsh": true}

// Load returns the live agents sorted by tmux address.
//
// With tidy set it also does the bookkeeping no hook can do: it deletes state
// left by sessions that died without a SessionEnd, marks finished agents in a
// window I am looking at as seen, notices interrupted turns (Claude Code fires
// no Stop hook for Esc), interrupts and retries an agent stuck waiting on the
// API, and keeps each window's @tower_state icon in sync.
func Load(tidy bool) []Agent {
	ps := tmux.Panes()
	if len(ps) == 0 {
		return nil // no tmux server: say nothing, delete nothing
	}
	now := time.Now().Unix()
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.json"))
	live := map[string]State{}
	var dead []State
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s State
		if json.Unmarshal(b, &s) != nil || s.SessionID == "" {
			continue
		}
		p, ok := ps[s.Pane]
		if !ok || (shells[p.Command] && now-s.Updated > 10) {
			dead = append(dead, s)
			continue
		}
		if old, ok := live[s.Pane]; ok { // two sessions claim one pane: newest wins
			if old.Updated >= s.Updated {
				dead = append(dead, s)
				continue
			}
			dead = append(dead, old)
		}
		live[s.Pane] = s
	}

	agents := make([]Agent, 0, len(live))
	for _, s := range live {
		p := ps[s.Pane]
		if tidy {
			switch {
			case s.Status == Done && p.Visible():
				s.Status = Idle
				Write(s)
			case s.Status == Working && s.RetryAt == 0 && now-s.Updated > 5 && interrupted(s.Transcript):
				s.Status, s.Activity = Idle, "interrupted"
				Write(s)
				AppendLog(s.SessionID, now, "✗ interrupted")
			case s.Status == Working && s.Activity == "thinking" && s.RetryAt == 0 && now-s.Updated > stallQuiet &&
				tmux.Option("@tower_retry", "on") != "off":
				if line, ok := stalled(s); ok {
					s, _ = retryStall(s, line, now)
				}
			}
		}
		agents = append(agents, Agent{State: s, Win: p})
	}
	sort.Slice(agents, func(i, j int) bool {
		a, b := agents[i].Win, agents[j].Win
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.WindowIndex != b.WindowIndex {
			return atoi(a.WindowIndex) < atoi(b.WindowIndex)
		}
		return atoi(a.Index) < atoi(b.Index)
	})

	// A name I gave it wins; otherwise the agent's own folder. Not the
	// window name: tmux renames windows after whichever pane is focused.
	count := map[string]int{}
	for i := range agents {
		a := &agents[i]
		switch {
		case a.Win.AgentName != "":
			a.Name = a.Win.AgentName
		case a.Cwd != "":
			a.Name = filepath.Base(a.Cwd)
		default:
			a.Name = a.Win.WindowName
		}
		count[a.Name]++
	}
	perWindow := map[string]int{}
	for i := range agents {
		if a := &agents[i]; count[a.Name] > 1 {
			perWindow[a.Name+"-"+a.Win.WindowIndex]++
		}
	}
	for i := range agents {
		a := &agents[i]
		if a.Win.AgentName != "" || count[a.Name] < 2 {
			continue
		}
		if perWindow[a.Name+"-"+a.Win.WindowIndex] > 1 {
			a.Name += "-" + a.Win.WindowIndex + "." + a.Win.Index
		} else {
			a.Name += "-" + a.Win.WindowIndex
		}
	}

	if tidy {
		for _, s := range dead {
			Remove(s.SessionID)
		}
		syncIcons(ps, agents)
	}
	return agents
}

// syncIcons sets each window's @tower_state to its most urgent agent, which
// the window-status format turns into an icon.
func syncIcons(ps map[string]tmux.Pane, agents []Agent) {
	want := map[string]string{}
	for _, a := range agents {
		if cur, ok := want[a.Win.WindowID]; !ok || Rank[a.Status] > Rank[cur] {
			want[a.Win.WindowID] = a.Status
		}
	}
	done := map[string]bool{}
	for _, p := range ps {
		if done[p.WindowID] {
			continue
		}
		done[p.WindowID] = true
		switch w := want[p.WindowID]; {
		case w == p.AgentState:
		case w == "":
			tmux.Run("set", "-wu", "-t", p.WindowID, "@tower_state")
		default:
			tmux.Run("set", "-w", "-t", p.WindowID, "@tower_state", w)
		}
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
