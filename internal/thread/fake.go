package thread

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// fake is a scripted engine for tests and UI work: no model, no tokens.
// Every message gets a streamed reply around a shell command that needs
// approval, so each kind of event shows up.
type fake struct {
	o       StartOpts
	mu      sync.Mutex
	n       int
	pending string // approval id waiting on me
	cmd     string
	flaky   int // turns left that fail like a dropped connection
}

func init() { register("fake", newFake) }

func newFake(o StartOpts) (Engine, error) {
	f := &fake{o: o}
	go o.Emit(Event{Kind: KindSession, Text: "fake-" + o.Thread, Input: "fake-model"})
	return f, nil
}

func (f *fake) Send(m Message) error {
	text := m.Text
	f.mu.Lock()
	if strings.Contains(text, "flaky") {
		f.flaky = 2
	}
	if f.flaky > 0 {
		f.flaky--
		f.mu.Unlock()
		go func() {
			f.o.Emit(Event{Kind: KindTurn, Status: TurnStarted})
			f.o.Emit(Event{Kind: KindTurn, Status: TurnFailed, Text: "API Error: Connection error (simulated)"})
		}()
		return nil
	}
	f.n++
	id := fmt.Sprintf("t%d", f.n)
	f.pending, f.cmd = id, "echo "+strings.Fields(text + " x")[0]
	cmd := f.cmd
	f.mu.Unlock()
	go func() {
		f.o.Emit(Event{Kind: KindTurn, Status: TurnStarted})
		for _, w := range strings.Fields("Looking into that. I will run one command first.") {
			f.o.Emit(Event{Kind: KindDelta, ID: id + "a", Role: "assistant", Text: w + " "})
			time.Sleep(20 * time.Millisecond)
		}
		f.o.Emit(Event{Kind: KindText, ID: id + "a", Role: "assistant", Text: "Looking into that. I will run one command first."})
		f.o.Emit(Event{Kind: KindTool, ID: id, Tool: "Bash", Input: cmd, Status: ToolRunning})
		f.o.Emit(Event{Kind: KindApproval, ID: id, Tool: "Bash", Input: cmd, Status: Pending})
	}()
	return nil
}

func (f *fake) Answer(id string, d Decision) error {
	f.mu.Lock()
	if id != f.pending {
		f.mu.Unlock()
		return fmt.Errorf("no pending approval %q", id)
	}
	f.pending = ""
	cmd := f.cmd
	f.mu.Unlock()
	go func() {
		if d == Deny {
			f.o.Emit(Event{Kind: KindApproval, ID: id, Status: Denied})
			f.o.Emit(Event{Kind: KindTool, ID: id, Status: ToolDeclined})
			f.o.Emit(Event{Kind: KindText, ID: id + "b", Role: "assistant", Text: "Understood, I did not run it."})
		} else {
			f.o.Emit(Event{Kind: KindApproval, ID: id, Status: Allowed})
			f.o.Emit(Event{Kind: KindTool, ID: id, Status: ToolOK, Output: strings.TrimPrefix(cmd, "echo ")})
			f.o.Emit(Event{Kind: KindText, ID: id + "b", Role: "assistant", Text: "Done: the command printed its argument."})
		}
		f.o.Emit(Event{Kind: KindTurn, Status: TurnDone, Usage: &Usage{Input: 1200, Output: 40, Context: 1200, CostUSD: 0.001}})
	}()
	return nil
}

func (f *fake) Interrupt() error {
	f.mu.Lock()
	f.pending = ""
	f.mu.Unlock()
	go f.o.Emit(Event{Kind: KindTurn, Status: TurnInterrupted})
	return nil
}

func (f *fake) Close() error {
	go f.o.Exited(nil)
	return nil
}
