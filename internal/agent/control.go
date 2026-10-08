package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tower/internal/tmux"
)

// Resolve finds the agent a target names: an agent name, a pane id, a Claude
// session id, a 1-based index from `tower ls`, or any tmux target.
func Resolve(target string, agents []Agent) (Agent, error) {
	for _, a := range agents {
		if target == a.Name || target == a.Pane || target == a.SessionID {
			return a, nil
		}
	}
	if i, err := strconv.Atoi(target); err == nil && i >= 1 && i <= len(agents) {
		return agents[i-1], nil
	}
	if pane := tmux.Run("display", "-p", "-t", target, "#{pane_id}"); pane != "" {
		for _, a := range agents {
			if a.Pane == pane {
				return a, nil
			}
		}
	}
	return Agent{}, fmt.Errorf("no agent matches %q (see: tower ls)", target)
}

// Route reads leading @mentions: "@api @docs check this" goes to api and
// docs, "@all ..." to every agent. Without a mention it goes to def.
func Route(text string, def, agents []Agent) ([]Agent, string, error) {
	var to []Agent
	seen := map[string]bool{}
	add := func(a Agent) {
		if !seen[a.Pane] {
			seen[a.Pane] = true
			to = append(to, a)
		}
	}
	rest := strings.TrimSpace(text)
	for strings.HasPrefix(rest, "@") {
		word, after, _ := strings.Cut(rest, " ")
		if name := word[1:]; name == "all" {
			for _, a := range agents {
				add(a)
			}
		} else {
			a, err := Resolve(name, agents)
			if err != nil {
				return nil, "", err
			}
			add(a)
		}
		rest = strings.TrimSpace(after)
	}
	if len(to) == 0 {
		to = def
	}
	return to, rest, nil
}

// Self is the agent running in the calling pane, if any.
func Self(agents []Agent) (Agent, bool) {
	me := os.Getenv("TMUX_PANE")
	for _, a := range agents {
		if me != "" && a.Pane == me {
			return a, true
		}
	}
	return Agent{}, false
}

// Send types text into an agent's prompt as bracketed pastes and submits it.
// Claude Code queues it if the agent is mid-turn. It refuses an agent sitting
// on a permission prompt, where the Enter would approve whatever is asked.
//
// Image paths go in as pastes of their own: Claude Code turns a pasted image
// path into an [Image #N] attachment right where it lands.
func Send(a Agent, text string, force bool) error {
	if a.Status == Waiting && !force {
		return fmt.Errorf("%s is waiting on a prompt (%s); answer it first", a.Name, a.Activity)
	}
	buf := fmt.Sprintf("tower-%d", os.Getpid())
	for _, part := range splitImages(text) {
		if part.text == "" {
			continue
		}
		if err := tmux.RunInput(part.text, "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
		tmux.Run("paste-buffer", "-d", "-p", "-r", "-b", buf, "-t", a.Pane)
		if part.image {
			time.Sleep(250 * time.Millisecond) // it reads the file
		} else {
			time.Sleep(40 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond) // let the TUI finish the paste before Enter
	tmux.Run("send-keys", "-t", a.Pane, "Enter")
	return nil
}

type part struct {
	text  string
	image bool
}

var imagePath = regexp.MustCompile(`(?i)(?:~|/)[^\s'"]*\.(?:png|jpe?g|gif|webp)\b`)

// splitImages cuts text around paths of image files that exist.
func splitImages(text string) []part {
	var out []part
	last := 0
	for _, m := range imagePath.FindAllStringIndex(text, -1) {
		p := text[m[0]:m[1]]
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		out = append(out, part{text: text[last:m[0]]}, part{text: p, image: true})
		last = m[1]
	}
	return append(out, part{text: text[last:]})
}

// Jump switches the current client to the agent's pane.
func Jump(pane string) {
	tmux.Run("switch-client", "-t", pane)
	tmux.Run("select-window", "-t", pane)
	tmux.Run("select-pane", "-t", pane)
}

// Rename sets the name tower shows for the agent in pane.
func Rename(pane, name string) {
	if name = Slug(name); name != "" {
		tmux.Run("set", "-p", "-t", pane, "@tower_name", name)
	}
}

// Peek returns the last n lines of the agent's screen and scrollback.
func Peek(pane string, n int) string {
	lines := strings.Split(tmux.Run("capture-pane", "-p", "-J", "-t", pane, "-S", strconv.Itoa(-n)), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// SpawnOpts describes a new agent.
type SpawnOpts struct {
	Name     string
	Dir      string // working directory; default the current one
	Prompt   string // first message; empty starts an idle session
	Session  string // tmux session; default the current one
	Cmd      string // default @tower_claude, else "claude"
	Worktree bool   // run in a fresh git worktree on branch tower/<name>
}

// Spawn starts an agent in a new background tmux window and returns its pane
// id and name.
func Spawn(o SpawnOpts) (pane, name string, err error) {
	dir := o.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", "", err
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", "", err
	}
	if name = Slug(o.Name); name == "" {
		name = nextName()
	}
	if o.Worktree {
		if dir, err = Worktree(dir, name); err != nil {
			return "", "", err
		}
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-n", name, "-c", dir}
	if o.Session != "" {
		args = append(args, "-t", "="+o.Session+":")
	}
	if pane = tmux.Run(args...); pane == "" {
		return "", "", errors.New("tmux new-window failed (is tmux running?)")
	}
	tmux.Run("set", "-p", "-t", pane, "@tower_name", name)
	cmd := o.Cmd
	if cmd == "" {
		cmd = tmux.Option("@tower_claude", "claude")
	}
	if o.Prompt != "" {
		cmd += " " + ShellQuote(o.Prompt)
	}
	// Typed into a shell rather than run as the window command, so the
	// window survives claude exiting and my shell aliases apply.
	tmux.Run("send-keys", "-t", pane, "-l", cmd)
	tmux.Run("send-keys", "-t", pane, "Enter")
	return pane, name, nil
}

// Wait blocks until the agent in pane stops working. With since > 0 it first
// waits for a turn that started at or after since, so a message I just sent
// is not mistaken for the previous turn ending.
func Wait(pane string, since int64, timeout time.Duration) (Agent, error) {
	start := time.Now()
	var seen time.Time
	started := since == 0
	for {
		a, ok := find(pane)
		switch {
		case ok:
			if seen.IsZero() {
				seen = time.Now()
			}
			// A message sent mid-turn may be folded into that turn without a
			// new UserPromptSubmit, so stop waiting for one after a while.
			if !started && (a.TurnStarted >= since || time.Since(seen) > 20*time.Second) {
				started = true
			}
			if started && a.Status != Working {
				return a, nil
			}
		case !seen.IsZero():
			return Agent{}, fmt.Errorf("the agent in %s exited", pane)
		case trustPrompt(pane):
			return Agent{}, fmt.Errorf("%s is on Claude Code's folder trust prompt; answer it with: tower jump %s", pane, pane)
		case time.Since(start) > 90*time.Second:
			return Agent{}, fmt.Errorf("no Claude session showed up in %s", pane)
		}
		if timeout > 0 && time.Since(start) > timeout {
			return a, fmt.Errorf("timed out; %s is still %s", or(a.Name, pane), or(a.Status, "starting"))
		}
		time.Sleep(time.Second)
	}
}

// trustPrompt reports whether a starting claude is stuck on the folder trust
// question, which comes before any hook fires. tower never answers it.
func trustPrompt(pane string) bool {
	screen := tmux.Run("capture-pane", "-p", "-t", pane)
	return strings.Contains(screen, "trust this folder") || strings.Contains(screen, "trust the files in this folder")
}

func find(pane string) (Agent, bool) {
	for _, a := range Load(true) {
		if a.Pane == pane {
			return a, true
		}
	}
	return Agent{}, false
}

// IsGitRepo reports whether dir is inside a git work tree.
func IsGitRepo(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// Worktree creates <repo>-<name> next to the repo on a new branch tower/<name>.
func Worktree(dir, name string) (string, error) {
	top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repo", dir)
	}
	root := strings.TrimSpace(string(top))
	path := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+name)
	out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", "tower/"+name, path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(out)))
	}
	return path, nil
}

func nextName() string {
	used := map[string]bool{}
	for _, p := range tmux.Panes() {
		used[p.AgentName], used[p.WindowName] = true, true
	}
	for i := 1; ; i++ {
		if n := "agent-" + strconv.Itoa(i); !used[n] {
			return n
		}
	}
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Slug makes s safe for a tmux option, a branch and a directory name.
func Slug(s string) string { return strings.Trim(unsafe.ReplaceAllString(s, "-"), "-") }

// ShellQuote quotes s for sh.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
