package thread

import (
	"errors"
	"fmt"
	"log"
	"time"

	"tower/internal/agent"
)

// RelayArgs assigns a task from one tmux agent to another. From may be
// empty for a task I assign myself; then the reply only goes to the log.
type RelayArgs struct {
	From string `json:"from"` // pane id
	To   string `json:"to"`   // pane id
	Text string `json:"text"`
}

// relay delivers a task, waits for the other agent to finish it, and pastes
// its answer back into the agent that asked. It runs in the daemon so neither
// agent has to block.
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
	if from != nil {
		fromName = from.Name
	}
	note := "when you finish, your final answer goes back to it automatically"
	if from == nil {
		note = "I asked through tower"
	}
	since := time.Now().Unix()
	if err := agent.Send(*to, fmt.Sprintf("[task from %s via tower; %s]\n%s", quoteName(fromName), note, a.Text), false); err != nil {
		return err
	}
	id := agent.LogMessage(agent.Message{Kind: "task", From: fromName, To: to.Name, FromPane: a.From, ToPane: to.Pane, Text: a.Text})
	go func() {
		done, err := agent.WaitDone(to.Pane, since, 6*time.Hour)
		reply := agent.FinalReply(done.Transcript)
		if err != nil {
			reply = err.Error()
		}
		agent.LogMessage(agent.Message{Kind: "reply", From: to.Name, To: fromName, FromPane: to.Pane, ToPane: a.From, Text: reply, Re: id, Failed: err != nil})
		if from == nil {
			return
		}
		msg := fmt.Sprintf("[reply from agent %q to the task you gave it]\n%s", to.Name, reply)
		if err := agent.Deliver(a.From, msg); err != nil {
			log.Printf("relay %s: %v", id, err)
		}
	}()
	return nil
}

func quoteName(n string) string {
	if n == "me" {
		return "me"
	}
	return fmt.Sprintf("agent %q", n)
}
