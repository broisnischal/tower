package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"tower/internal/agent"
	"tower/internal/thread"
)

// Headless threads: agents tower runs itself through the daemon.

func newThread(args []string) error {
	fs := flag.NewFlagSet("tower new", flag.ContinueOnError)
	var a thread.CreateArgs
	fs.StringVar(&a.Engine, "e", "claude", "engine: "+strings.Join(thread.Engines(), " or "))
	fs.StringVar(&a.Name, "n", "", "thread name")
	fs.StringVar(&a.Cwd, "d", "", "working directory (default: current)")
	fs.StringVar(&a.Model, "m", "", "model (default: the engine's)")
	fs.StringVar(&a.Mode, "mode", "default", "permissions: default (ask me), acceptEdits, plan, bypassPermissions")
	fs.BoolVar(&a.Worktree, "w", false, "own git worktree on branch tower/<name>")
	waitFor := fs.Bool("wait", false, "wait for the task, then print the reply")
	timeout := fs.Int("t", 0, "with --wait, give up after this many seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if a.Cwd == "" {
		a.Cwd, _ = os.Getwd()
	}
	a.Cwd, _ = filepath.Abs(a.Cwd)
	a.Prompt = strings.Join(fs.Args(), " ")
	c, err := thread.Dial(true)
	if err != nil {
		return fmt.Errorf("tower daemon: %w", err)
	}
	defer c.Close()
	var t thread.Thread
	if err := c.Call("create", a, &t); err != nil {
		return err
	}
	if !*waitFor {
		fmt.Println(t.Name, t.ID)
		return nil
	}
	fmt.Fprintf(os.Stderr, "started %s (%s), waiting for it\n", t.Name, t.Engine)
	return threadReply(c, t.ID, seconds(*timeout))
}

// threadReply waits for the thread's turn to end and prints what it said.
func threadReply(c *thread.Client, id string, timeout time.Duration) error {
	start := time.Now()
	time.Sleep(300 * time.Millisecond) // let the turn register
	for {
		t, err := findThread(c, id)
		if err != nil {
			return err
		}
		switch t.Status {
		case agent.Waiting:
			return fmt.Errorf("%s is waiting for approval (%s); answer with: tower approve %s", t.Name, t.Activity, t.Name)
		case agent.Done, agent.Idle:
			var ev []thread.Event
			if err := c.Call("history", thread.Ref(t.ID), &ev); err != nil {
				return err
			}
			fmt.Println(thread.LastReply(ev))
			return nil
		}
		if timeout > 0 && time.Since(start) > timeout {
			return fmt.Errorf("timed out; %s is still %s", t.Name, t.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func findThread(c *thread.Client, ref string) (thread.Thread, error) {
	var ts []thread.Thread
	if err := c.Call("list", nil, &ts); err != nil {
		return thread.Thread{}, err
	}
	for i, t := range ts {
		if t.ID == ref || t.Name == ref || fmt.Sprint(i+1) == strings.TrimPrefix(ref, "t") && strings.HasPrefix(ref, "t") {
			return t, nil
		}
	}
	return thread.Thread{}, fmt.Errorf("no agent or thread matches %q (see: tower ls)", ref)
}

func listThreads(args []string) error {
	fs := flag.NewFlagSet("tower threads", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := thread.Dial(false)
	if err != nil {
		if *asJSON {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("no threads (the daemon is not running)")
		return nil
	}
	defer c.Close()
	var ts []thread.Thread
	if err := c.Call("list", nil, &ts); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ts)
	}
	if len(ts) == 0 {
		fmt.Println("no threads")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSTATE\tNAME\tENGINE\tDIR\tACTIVITY")
	for i, t := range ts {
		fmt.Fprintf(tw, "t%d\t%s %s\t%s\t%s\t%s\t%s\n", i+1, agent.Icon[t.Status], t.Status, t.Name, t.Engine, t.Cwd, t.Activity)
	}
	return tw.Flush()
}

// threadAct runs send, wait, last, stop and kill against a thread, for
// targets that are not tmux agents.
func threadAct(cmd, ref string, text string, waitFor bool, timeout int) error {
	c, err := thread.Dial(false)
	if err != nil {
		return fmt.Errorf("no agent or thread matches %q (see: tower ls)", ref)
	}
	defer c.Close()
	t, err := findThread(c, ref)
	if err != nil {
		return err
	}
	switch cmd {
	case "send":
		if err := c.Call("send", map[string]string{"thread": t.ID, "text": text}, nil); err != nil {
			return err
		}
		if waitFor {
			return threadReply(c, t.ID, seconds(timeout))
		}
	case "wait":
		return threadReply(c, t.ID, seconds(timeout))
	case "last":
		var ev []thread.Event
		if err := c.Call("history", thread.Ref(t.ID), &ev); err != nil {
			return err
		}
		fmt.Println(thread.LastReply(ev))
	case "stop":
		return c.Call("interrupt", thread.Ref(t.ID), nil)
	case "kill":
		return c.Call("remove", thread.Ref(t.ID), nil)
	case "jump":
		return errors.New("threads have no pane to jump to; open them in the dashboard (prefix t)")
	}
	return nil
}

// approve answers the oldest pending approval of a thread.
func approve(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: tower approve <thread> [allow|always|deny]")
	}
	d := thread.Allow
	if len(args) == 2 {
		d = thread.Decision(args[1])
	}
	c, err := thread.Dial(false)
	if err != nil {
		return err
	}
	defer c.Close()
	t, err := findThread(c, args[0])
	if err != nil {
		return err
	}
	var ev []thread.Event
	if err := c.Call("history", thread.Ref(t.ID), &ev); err != nil {
		return err
	}
	var chat thread.Chat
	for _, e := range ev {
		chat.Apply(e)
	}
	p, ok := chat.PendingApproval()
	if !ok {
		return fmt.Errorf("%s has nothing waiting for approval", t.Name)
	}
	fmt.Printf("%s %s %s\n", d, p.Tool, p.Input)
	return c.Call("answer", map[string]any{"thread": t.ID, "approval": p.ID, "decision": d}, nil)
}
