package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"tower/internal/agent"
	"tower/internal/thread"
)

// ask assigns a task to another agent without blocking: the daemon delivers
// it, waits for the answer, and pastes the answer back into the asker.
func ask(args []string) error {
	fs := flag.NewFlagSet("tower ask", flag.ContinueOnError)
	from := fs.String("from", "", "agent or peer the reply goes back to (default: the calling agent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: tower ask [--from agent|peer] <agent> <task>")
	}
	agents := agent.Load(true)
	to, err := agent.Resolve(fs.Arg(0), agents)
	if err != nil {
		return err
	}
	var fromPane, fromPeer string
	switch me, ok := agent.Self(agents); {
	case *from != "":
		if p, isPeer := agent.FindPeer(*from); isPeer {
			fromPeer = p.Name
			break
		}
		f, err := agent.Resolve(*from, agents)
		if err != nil {
			return err
		}
		fromPane = f.Pane
	case ok:
		fromPane = me.Pane
	}
	if fromPane != "" && fromPane == to.Pane {
		return errors.New("an agent cannot assign a task to itself")
	}
	c, err := thread.Dial(true)
	if err != nil {
		return fmt.Errorf("tower daemon: %w", err)
	}
	defer c.Close()
	if err := c.Call("relay", thread.RelayArgs{From: fromPane, FromPeer: fromPeer, To: to.Pane, Text: strings.Join(fs.Args()[1:], " ")}, nil); err != nil {
		return err
	}
	if fromPane != "" || fromPeer != "" {
		fmt.Printf("assigned to %s; its answer will arrive here as a message\n", to.Name)
	} else {
		fmt.Printf("assigned to %s; see the answer with: tower comms\n", to.Name)
	}
	return nil
}

// comms prints the recent hand-offs between agents.
func comms(args []string) error {
	fs := flag.NewFlagSet("tower comms", flag.ContinueOnError)
	n := fs.Int("n", 30, "how many")
	if err := fs.Parse(args); err != nil {
		return err
	}
	msgs := agent.Messages(*n)
	if len(msgs) == 0 {
		fmt.Println("no messages between agents yet")
		return nil
	}
	open := map[string]bool{}
	for _, t := range agent.OpenTasks(msgs) {
		open[t.ID] = true
	}
	for _, m := range msgs {
		state := ""
		if open[m.ID] {
			state = "  (working on it)"
		}
		fmt.Printf("%s  %s -> %s  %s: %s%s\n", time.Unix(m.Time, 0).Format("15:04:05"), m.From, m.To, m.Kind,
			strings.Join(strings.Fields(m.Text), " "), state)
	}
	return nil
}

// peer lists, adds or removes the participants that have no pane.
func peer(args []string) error {
	switch {
	case len(args) == 0:
		ps := agent.Peers()
		if len(ps) == 0 {
			fmt.Println("no peers (add one with: tower peer add <name> <command...>)")
		}
		for _, p := range ps {
			fmt.Printf("%-12s %s\n", p.Name, strings.Join(p.Cmd, " "))
		}
		return nil
	case args[0] == "add" && len(args) >= 3:
		return agent.AddPeer(args[1], args[2:])
	case args[0] == "rm" && len(args) == 2:
		return agent.RemovePeer(args[1])
	}
	return errors.New("usage: tower peer [add <name> <command...> | rm <name>]")
}
