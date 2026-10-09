package thread

import (
	"errors"
	"fmt"
	"log"
	"time"

	"tower/internal/agent"
)

// RelayArgs assigns a task to a tmux agent. From is the asking agent's pane,
// or FromPeer a peer's name; with neither, the task is mine and the reply
// only goes to the log.
type RelayArgs struct {
	From     string `json:"from"` // pane id
	FromPeer string `json:"from_peer,omitempty"`
	To       string `json:"to"` // pane id
	Text     string `json:"text"`
}

// relay delivers a task, waits for the other agent to finish it, and hands
// its answer back to whoever asked. It runs in the daemon so neither side
// has to block, and resumeRelays picks it up again after a restart.
func (s *server) relay(a RelayArgs) error {
	agents := agent.Load(true)
	var from, to *agent.Agent
	for i := range agents {
		switch agents[i].Pane {
		case a.From:
			from = &agents[i]
		case a.To:
			to = &agents[i]
		}
	}
	if to == nil {
		return errors.New("no agent in pane " + a.To)
	}
	fromName := "me"
	switch {
	case from != nil:
		fromName = from.Name
	case a.FromPeer != "":
		fromName = a.FromPeer
	}
	note := "when you finish, your final answer goes back to it automatically"
	if fromName == "me" {
		note = "I asked through tower"
	}
	since := time.Now().Unix()
	if err := agent.Send(*to, fmt.Sprintf("[task from %s via tower; %s]\n%s", quoteName(fromName), note, a.Text), false); err != nil {
		return err
	}
	m := agent.Message{Kind: "task", From: fromName, To: to.Name, FromPane: a.From, ToPane: to.Pane, Text: a.Text,
		FromPeer: a.FromPeer != "", Via: "relay"}
	m.ID = agent.LogMessage(m)
	m.Time = since
	go s.answer(m)
	return nil
}

// answer waits for the agent working on task m to finish, logs its reply,
// and delivers it to the asker.
func (s *server) answer(m agent.Message) {
	done, err := agent.WaitDone(m.ToPane, m.Time, max(time.Minute, 6*time.Hour-time.Since(time.Unix(m.Time, 0))))
	reply := agent.FinalReply(done.Transcript)
	if err != nil {
		reply = err.Error()
	}
	agent.LogMessage(agent.Message{Kind: "reply", From: m.To, To: m.From, FromPane: m.ToPane, ToPane: m.FromPane, Text: reply, Re: m.ID, Failed: err != nil})
	msg := fmt.Sprintf("[reply from agent %q to the task you gave it]\n%s", m.To, reply)
	switch {
	case m.FromPeer:
		if p, ok := agent.FindPeer(m.From); ok {
			err = p.Send(msg)
		}
	case m.FromPane != "":
		err = agent.Deliver(m.FromPane, msg)
	default:
		return
	}
	if err != nil {
		log.Printf("relay %s: %v", m.ID, err)
	}
}

// resumeRelays waits again on the tasks the daemon was relaying when it last
// stopped, so a restart (after a rebuild, say) doesn't lose their replies.
func (s *server) resumeRelays() {
	for _, m := range agent.OpenTasks(agent.Messages(500)) {
		if m.Via == "relay" {
			go s.answer(m)
		}
	}
}

func quoteName(n string) string {
	if n == "me" {
		return "me"
	}
	return fmt.Sprintf("agent %q", n)
}
