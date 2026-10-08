package thread

import (
	"strings"
	"testing"
	"time"

	"tower/internal/agent"
)

// TestDaemonWithFakeEngine runs the daemon in-process and drives a thread
// through a full turn: message, streamed text, approval, tool result, done.
func TestDaemonWithFakeEngine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	go Serve()
	var c *Client
	var err error
	for range 50 {
		if c, err = Dial(false); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Watch(); err != nil {
		t.Fatal(err)
	}
	var th Thread
	if err := c.Call("create", CreateArgs{Engine: "fake", Name: "probe", Cwd: t.TempDir(), Prompt: "hello there"}, &th); err != nil {
		t.Fatal(err)
	}
	pushes := make(chan Push, 1024)
	go func() {
		for {
			p, ok := c.Next()
			if !ok {
				close(pushes)
				return
			}
			pushes <- p
		}
	}()
	var approval string
	timeout := time.After(5 * time.Second)
	for approval == "" {
		select {
		case p := <-pushes:
			if p.Event != nil && p.Event.Kind == KindApproval && p.Event.Status == Pending {
				approval = p.Event.ID
			}
		case <-timeout:
			t.Fatal("no approval request")
		}
	}
	status := func() string {
		var list []Thread
		c.Call("list", nil, &list)
		return list[0].Status
	}
	if st := status(); st != waiting {
		t.Fatalf("status while approval pending = %q", st)
	}
	if err := c.Call("answer", ref{Thread: "probe", Approval: approval, Decision: Allow}, nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); status() != done; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want done", status())
		}
	}

	var ev []Event
	if err := c.Call("history", Ref(th.ID), &ev); err != nil {
		t.Fatal(err)
	}
	var chat Chat
	for _, e := range ev {
		chat.Apply(e)
	}
	var kinds []string
	for _, it := range chat.Items {
		kinds = append(kinds, it.Kind+":"+it.Status)
	}
	want := []string{"user:", "turn:started", "text:", "tool:ok", "approval:allowed", "text:", "turn:done"}
	if len(kinds) != len(want) {
		t.Fatalf("chat = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("chat = %v, want %v", kinds, want)
		}
	}
	var list []Thread
	c.Call("list", nil, &list)
	if len(list) != 1 || list[0].Resume != "fake-"+th.ID || list[0].Usage.Output != 40 {
		t.Fatalf("thread = %+v", list)
	}
}

// TestThreadRetriesAfterNetworkErrors: a turn that dies on a dropped
// connection is retried with "continue" after a backoff, twice, and the
// third try goes through.
func TestThreadRetriesAfterNetworkErrors(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("TMUX", "")
	agent.BackoffBase = 50 * time.Millisecond
	defer func() { agent.BackoffBase = 5 * time.Second }()
	go Serve()
	var c *Client
	var err error
	for range 50 {
		if c, err = Dial(false); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var th Thread
	if err := c.Call("create", CreateArgs{Engine: "fake", Name: "flaky", Cwd: t.TempDir(), Prompt: "flaky task"}, &th); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var list []Thread
		c.Call("list", nil, &list)
		if len(list) == 1 && list[0].Status == waiting { // the third try reached its approval
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never recovered: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var ev []Event
	c.Call("history", Ref(th.ID), &ev)
	var failed, retried, continues int
	for _, e := range ev {
		switch {
		case e.Kind == KindTurn && e.Status == TurnFailed:
			failed++
		case e.Kind == KindInfo && strings.HasPrefix(e.Text, "↻"):
			retried++
		case e.Kind == KindUser && e.Text == "continue":
			continues++
		}
	}
	if failed != 2 || retried != 2 || continues != 2 {
		t.Fatalf("failed %d, retry notes %d, continues %d; want 2 each", failed, retried, continues)
	}
}
