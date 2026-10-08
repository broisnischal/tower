// Command tower is a control plane for the Claude Code agents I run in tmux.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"tower/internal/agent"
	"tower/internal/sidebar"
	"tower/internal/thread"
	"tower/internal/tmux"
	"tower/internal/ui"
)

const help = `tower: control plane for Claude Code agents in tmux

  tower                          open the dashboard (same as: tower ui)
  tower grid                     every agent at once, live
  tower compose <agent>          write a message in an editor (@name, images)
  tower ls [--json]              list agents
  tower browse [-d] [--here] [url]   browser tab in a tmux window, driving my Chrome
  tower browse --shot <url> <out.png> [--viewport]   save a screenshot, print its path
  tower new [flags] [task]       start a headless agent thread that tower runs itself
        -e claude|codex  -n name  -d dir  -m model  -w (own worktree)
        --mode default|acceptEdits|plan|bypassPermissions  --wait [-t secs]
  tower threads [--json]         list headless threads
  tower approve <thread> [allow|always|deny]   answer its pending approval
  tower spawn [flags] [task]     start an agent in a new tmux window
        -n name  -d dir  -s session  -c command
        -w       give it its own git worktree on branch tower/<name>
        --wait   wait for the task, then print the reply (-t secs to cap it)
  tower send [--wait] [--force] [-t secs] <agent> <message | ->
  tower ask [--from a] <agent> <task>  assign a task; the answer comes back to the asker
  tower comms [-n 30]            who handed what to whom
  tower wait [-t secs] <agent>   block until the agent stops working
  tower last <agent>             print the agent's last reply
  tower peek [-n lines] <agent>  print the agent's screen
  tower jump <agent>             switch tmux to the agent
  tower name [agent] <name>      rename an agent (default: the calling agent)
  tower stop <agent>             interrupt the agent's turn (sends Esc)
  tower kill <agent>             close the agent's pane
  tower whoami                   print the calling agent

<agent> is a name, a pane id (%3), an index from tower ls, or a tmux target;
send, wait, last, stop and kill also take a thread name, id, or t<index>.
Flags go before positional arguments.

plumbing: tower hook | serve [stop] | status | sidebar | input | toggle | focus | follow
`

func main() {
	cmd, args := "ui", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if err := run(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "tower:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	switch cmd {
	case "hook": // never fails: a broken hook must not block a tool call
		if s, ok := agent.Hook(os.Stdin); ok && s.RetryAt > 0 {
			scheduleRetry(s)
		}
		return nil
	case "ui", "dashboard":
		o := ui.Options{}
		if len(args) == 2 && args[0] == "--thread" {
			o.Thread = args[1]
		}
		return ui.Run(o)
	case "grid":
		return ui.Run(ui.Options{Grid: true})
	case "sidebar":
		return ui.Run(ui.Options{Sidebar: true})
	case "input":
		return ui.Input()
	case "compose":
		if len(args) != 1 {
			return errors.New("usage: tower compose <agent>")
		}
		return ui.Compose(args[0])
	case "status":
		fmt.Print(status())
		return nil
	case "serve":
		if len(args) == 1 && args[0] == "stop" {
			c, err := thread.Dial(false)
			if err != nil {
				return errors.New("the daemon is not running")
			}
			defer c.Close()
			return c.Call("shutdown", nil, nil)
		}
		return thread.Serve()
	case "new":
		return newThread(args)
	case "threads":
		return listThreads(args)
	case "approve":
		return approve(args)
	case "ask":
		return ask(args)
	case "browse":
		return browse(args)
	case "comms":
		return comms(args)
	case "respawn": // restart docked panes in place after an upgrade
		sidebar.Respawn()
		return nil
	case "fit": // put docked panes back to their size after a window resize
		if len(args) != 1 {
			return errors.New("usage: tower fit <tmux-target>")
		}
		sidebar.Fit(args[0])
		return nil
	case "follow":
		if len(args) != 1 {
			return errors.New("usage: tower follow <tmux-target>")
		}
		sidebar.Follow(args[0])
		return nil
	case "toggle", "focus": // tower toggle|focus sidebar|input <tmux-target>
		if len(args) != 2 {
			return fmt.Errorf("usage: tower %s sidebar|input <tmux-target>", cmd)
		}
		k, ok := sidebar.ByName(args[0])
		if !ok {
			return fmt.Errorf("no pane kind %q (sidebar or input)", args[0])
		}
		if cmd == "toggle" {
			k.Toggle()
		} else {
			k.Focus(args[1])
		}
		return nil
	case "ls", "list":
		return list(args)
	case "spawn":
		return spawn(args)
	case "send":
		return send(args)
	case "wait":
		return wait(args)
	case "last", "peek", "jump", "stop", "kill":
		return act(cmd, args)
	case "name":
		return name(args)
	case "whoami":
		a, ok := agent.Self(agent.Load(true))
		if !ok {
			return errors.New("not running inside a tracked agent pane")
		}
		fmt.Println(a.Name, a.Pane, a.Where())
		return nil
	case "help", "-h", "--help":
		fmt.Print(help)
		return nil
	}
	return fmt.Errorf("unknown command %q (see: tower help)", cmd)
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// scheduleRetry hands a retry to the daemon, which outlives this hook. It
// must print nothing: hook output can land in the agent's context.
func scheduleRetry(s agent.State) {
	c, err := thread.Dial(true)
	if err != nil {
		return
	}
	defer c.Close()
	c.Call("retry", thread.RetryArgs{Session: s.SessionID, Pane: s.Pane, At: s.RetryAt, Attempt: s.Retries}, nil)
}

// status is the tmux status-right segment: counts of agents that are
// waiting on me, working and done.
func status() string {
	n := map[string]int{}
	for _, a := range agent.Load(true) {
		n[a.Status]++
	}
	if c, err := thread.Dial(false); err == nil {
		var ts []thread.Thread
		c.Call("list", nil, &ts)
		c.Close()
		for _, t := range ts {
			n[t.Status]++
		}
	}
	var parts []string
	for _, s := range []string{agent.Waiting, agent.Working, agent.Done} {
		if n[s] > 0 {
			parts = append(parts, fmt.Sprintf("#[fg=%s]%s %d", map[string]string{agent.Waiting: "red", agent.Working: "#D97757", agent.Done: "green"}[s],
				map[string]string{agent.Waiting: "◐", agent.Working: "✻", agent.Done: "✓"}[s], n[s]))
		}
	}
	if open := len(agent.OpenTasks(agent.Messages(200))); open > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=magenta]⇄ %d", open))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + "#[default] "
}

func list(args []string) error {
	fs := flag.NewFlagSet("tower ls", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	agents := agent.Load(true)
	me := os.Getenv("TMUX_PANE")
	if *asJSON {
		type row struct {
			Index      int    `json:"index"`
			Name       string `json:"name"`
			Pane       string `json:"pane"`
			Where      string `json:"where"`
			State      string `json:"state"`
			Activity   string `json:"activity"`
			Prompt     string `json:"prompt,omitempty"`
			Cwd        string `json:"cwd"`
			SessionID  string `json:"session_id"`
			Transcript string `json:"transcript"`
			Self       bool   `json:"self,omitempty"`
		}
		rows := make([]row, len(agents))
		for i, a := range agents {
			rows[i] = row{i + 1, a.Name, a.Pane, a.Where(), a.Status, a.Activity, a.Prompt, a.Cwd, a.SessionID, a.Transcript, a.Pane == me}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(agents) == 0 {
		fmt.Println("no agents running")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSTATE\tNAME\tWHERE\tACTIVITY")
	for i, a := range agents {
		n := a.Name
		if a.Pane == me {
			n += " (you)"
		}
		fmt.Fprintf(tw, "%d\t%s %s\t%s\t%s\t%s\n", i+1, agent.Icon[a.Status], a.Status, n, a.Where(), a.Activity)
	}
	return tw.Flush()
}

func spawn(args []string) error {
	fs := flag.NewFlagSet("tower spawn", flag.ContinueOnError)
	var o agent.SpawnOpts
	fs.StringVar(&o.Name, "n", "", "agent name")
	fs.StringVar(&o.Dir, "d", "", "working directory (default: current)")
	fs.StringVar(&o.Session, "s", "", "tmux session (default: current)")
	fs.StringVar(&o.Cmd, "c", "", "command to start (default: @tower_claude, else claude)")
	fs.BoolVar(&o.Worktree, "w", false, "own git worktree on branch tower/<name>")
	waitFor := fs.Bool("wait", false, "wait for the task, then print the reply")
	timeout := fs.Int("t", 0, "with --wait, give up after this many seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o.Prompt = strings.Join(fs.Args(), " ")
	if *waitFor && o.Prompt == "" {
		return errors.New("--wait needs a task")
	}
	since := time.Now().Unix()
	pane, name, err := agent.Spawn(o)
	if err != nil {
		return err
	}
	if !*waitFor {
		fmt.Println(name, pane)
		return nil
	}
	fmt.Fprintf(os.Stderr, "started %s in %s, waiting for it\n", name, pane)
	return reply(pane, since, *timeout)
}

func send(args []string) error {
	fs := flag.NewFlagSet("tower send", flag.ContinueOnError)
	waitFor := fs.Bool("wait", false, "wait for the reply and print it")
	force := fs.Bool("force", false, "send even if the agent is on a permission prompt")
	timeout := fs.Int("t", 0, "with --wait, give up after this many seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: tower send [--wait] <agent> <message | ->")
	}
	msg := strings.Join(fs.Args()[1:], " ")
	if msg == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		msg = string(b)
	}
	msg = strings.TrimSpace(msg)
	agents := agent.Load(true)
	a, err := agent.Resolve(fs.Arg(0), agents)
	if err != nil {
		return threadAct("send", fs.Arg(0), msg, *waitFor, *timeout)
	}
	body, fromName, fromPane := msg, "me", ""
	if me, ok := agent.Self(agents); ok {
		fromName, fromPane = me.Name, me.Pane
		if me.Pane == a.Pane {
			return errors.New("that agent is the caller")
		}
		hint := fmt.Sprintf("reply with: tower send %s \"...\"", me.Name)
		if *waitFor {
			hint = "your final answer goes back to it automatically"
		}
		msg = fmt.Sprintf("[message from agent %q; %s]\n%s", me.Name, hint, msg)
	}
	since := time.Now().Unix()
	if err := agent.Send(a, msg, *force); err != nil {
		return err
	}
	kind := "message"
	if *waitFor {
		kind = "task"
	}
	id := agent.LogMessage(agent.Message{Kind: kind, From: fromName, To: a.Name, FromPane: fromPane, ToPane: a.Pane, Text: body})
	if !*waitFor {
		return nil
	}
	err = reply(a.Pane, since, *timeout)
	text := ""
	if done, ok := findAgent(a.Pane); ok {
		text = agent.FinalReply(done.Transcript)
	}
	if err != nil {
		text = err.Error()
	}
	agent.LogMessage(agent.Message{Kind: "reply", From: a.Name, To: fromName, FromPane: a.Pane, ToPane: fromPane, Text: text, Re: id, Failed: err != nil})
	return err
}

func findAgent(pane string) (agent.Agent, bool) {
	for _, a := range agent.Load(false) {
		if a.Pane == pane {
			return a, true
		}
	}
	return agent.Agent{}, false
}

// reply waits for the turn that started at since and prints its answer.
func reply(pane string, since int64, timeout int) error {
	a, err := agent.Wait(pane, since, seconds(timeout))
	if err != nil {
		return err
	}
	fmt.Println(agent.FinalReply(a.Transcript))
	if a.Status == agent.Waiting {
		return fmt.Errorf("%s is waiting on a prompt (%s); see: tower jump %s", a.Name, a.Activity, a.Name)
	}
	return nil
}

func wait(args []string) error {
	fs := flag.NewFlagSet("tower wait", flag.ContinueOnError)
	timeout := fs.Int("t", 0, "give up after this many seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: tower wait [-t secs] <agent>")
	}
	a, err := agent.Resolve(fs.Arg(0), agent.Load(true))
	if err != nil {
		return threadAct("wait", fs.Arg(0), "", true, *timeout)
	}
	if a, err = agent.Wait(a.Pane, 0, seconds(*timeout)); err != nil {
		return err
	}
	fmt.Println(a.Status)
	return nil
}

func act(cmd string, args []string) error {
	lines := 40
	if cmd == "peek" {
		fs := flag.NewFlagSet("tower peek", flag.ContinueOnError)
		fs.IntVar(&lines, "n", 40, "lines to print")
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: tower %s <agent>", cmd)
	}
	a, err := agent.Resolve(args[0], agent.Load(true))
	if err != nil {
		if cmd == "peek" {
			return err
		}
		return threadAct(cmd, args[0], "", false, 0)
	}
	switch cmd {
	case "last":
		fmt.Println(agent.LastReply(a.Transcript))
	case "peek":
		fmt.Println(agent.Peek(a.Pane, lines))
	case "jump":
		agent.Jump(a.Pane)
	case "stop":
		tmux.Run("send-keys", "-t", a.Pane, "Escape")
	case "kill":
		tmux.Run("kill-pane", "-t", a.Pane)
	}
	return nil
}

func name(args []string) error {
	agents := agent.Load(true)
	var a agent.Agent
	switch len(args) {
	case 1:
		var ok bool
		if a, ok = agent.Self(agents); !ok {
			return errors.New("not inside an agent pane; use: tower name <agent> <name>")
		}
	case 2:
		var err error
		if a, err = agent.Resolve(args[0], agents); err != nil {
			return err
		}
	default:
		return errors.New("usage: tower name [agent] <name>")
	}
	agent.Rename(a.Pane, args[len(args)-1])
	return nil
}
