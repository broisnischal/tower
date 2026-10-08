package thread

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// codex drives OpenAI Codex through `codex app-server`: JSON-RPC over stdio,
// one JSON object per line, approvals as server-to-client requests.
type codex struct {
	o      StartOpts
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr *tail

	mu      sync.Mutex
	next    int
	waits   map[int]chan rpcReply
	thread  string
	turn    string                     // active turn id
	open    map[string]string          // agent message id -> text so far
	output  map[string]string          // command id -> output so far
	items   map[string]codexItem       // started items, for approval details
	pending map[string]json.RawMessage // approval id -> server request id
	usage   codexTokens
	prevOut int
	closing bool
}

type rpcReply struct {
	Result json.RawMessage
	Err    error
}

type codexTokens struct {
	Total, Last struct {
		Total  int `json:"totalTokens"`
		Input  int `json:"inputTokens"`
		Output int `json:"outputTokens"`
	}
	Window int `json:"modelContextWindow"`
}

type codexItem struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Summary  []string
	Command  string `json:"command"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exitCode"`
	Output   string `json:"aggregatedOutput"`
	Actions  []struct {
		Command string `json:"command"`
	} `json:"commandActions"`
	Changes []struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	} `json:"changes"`
	Query  string          `json:"query"`
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"arguments"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func init() { register("codex", newCodex) }

// codexPolicy maps tower's permission modes onto Codex's approval policy
// and sandbox.
func codexPolicy(mode string) (approval, sandbox string) {
	switch mode {
	case "acceptEdits":
		return "on-request", "workspace-write"
	case "plan":
		return "untrusted", "read-only"
	case "bypassPermissions":
		return "never", "danger-full-access"
	}
	return "untrusted", "workspace-write"
}

func newCodex(o StartOpts) (Engine, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, errors.New("codex is not on PATH")
	}
	c := &codex{
		o: o, stderr: &tail{}, waits: map[int]chan rpcReply{}, open: map[string]string{},
		output: map[string]string{}, items: map[string]codexItem{}, pending: map[string]json.RawMessage{},
	}
	c.cmd = exec.Command(bin, "app-server")
	c.cmd.Dir = o.Cwd
	c.cmd.Env = agentEnv(o.Thread)
	c.cmd.Stderr = c.stderr
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go c.read(out)
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *codex) handshake() error {
	if _, err := c.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "tower", "title": "tower", "version": "0.1"},
		"capabilities": map[string]any{"experimentalApi": false},
	}); err != nil {
		return fmt.Errorf("codex initialize: %w", err)
	}
	c.send(map[string]any{"method": "initialized"})
	approval, sandbox := codexPolicy(c.o.Mode)
	params := map[string]any{"cwd": c.o.Cwd, "approvalPolicy": approval, "sandbox": sandbox}
	if c.o.Model != "" {
		params["model"] = c.o.Model
	}
	method := "thread/start"
	if c.o.Resume != "" {
		// Resume forgets the policy, so it goes along every time.
		method, params["threadId"] = "thread/resume", c.o.Resume
	}
	res, err := c.call(method, params)
	if err != nil {
		return fmt.Errorf("codex %s: %w", method, err)
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	json.Unmarshal(res, &r)
	c.mu.Lock()
	c.thread = r.Thread.ID
	c.mu.Unlock()
	c.o.Emit(Event{Kind: KindSession, Text: r.Thread.ID, Input: r.Model})
	return nil
}

func (c *codex) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *codex) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan rpcReply, 1)
	c.waits[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("codex exited: " + c.stderr.String())
		}
		return r.Result, r.Err
	case <-time.After(2 * time.Minute):
		return nil, errors.New("codex did not answer " + method)
	}
}

func (c *codex) Send(m Message) error {
	input := []map[string]any{{"type": "text", "text": m.Text, "text_elements": []any{}}}
	for _, p := range m.Images {
		input = append(input, map[string]any{"type": "localImage", "path": p})
	}
	c.mu.Lock()
	thread, turn := c.thread, c.turn
	c.mu.Unlock()
	if turn != "" { // mid-turn: Codex folds it into the running turn
		_, err := c.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": input})
		return err
	}
	_, err := c.call("turn/start", map[string]any{"threadId": thread, "input": input, "summary": "auto"})
	return err
}

func (c *codex) Interrupt() error {
	c.mu.Lock()
	thread, turn := c.thread, c.turn
	c.mu.Unlock()
	if turn == "" {
		return nil
	}
	_, err := c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

func (c *codex) Answer(id string, d Decision) error {
	c.mu.Lock()
	reqID, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval %q", id)
	}
	decision, status := "accept", Allowed
	switch d {
	case Always:
		decision = "acceptForSession"
	case Deny:
		decision, status = "decline", Denied
	}
	c.o.Emit(Event{Kind: KindApproval, ID: id, Status: status})
	return c.send(map[string]any{"id": reqID, "result": map[string]any{"decision": decision}})
}

func (c *codex) Close() error {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	c.mu.Unlock()
	c.stdin.Close() // app-server exits cleanly on EOF
	go func() {
		time.Sleep(3 * time.Second)
		if c.cmd.ProcessState == nil {
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
			time.Sleep(5 * time.Second)
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	return nil
}

func (c *codex) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			c.request(m.ID, m.Method, m.Params)
		case m.Method != "":
			c.notify(m.Method, m.Params)
		default:
			var id int
			json.Unmarshal(m.ID, &id)
			c.mu.Lock()
			ch := c.waits[id]
			delete(c.waits, id)
			c.mu.Unlock()
			if ch != nil {
				r := rpcReply{Result: m.Result}
				if m.Error != nil {
					r.Err = errors.New(m.Error.Message)
				}
				ch <- r
			}
		}
	}
	c.mu.Lock()
	for id, ch := range c.waits {
		close(ch)
		delete(c.waits, id)
	}
	c.mu.Unlock()
	err := c.cmd.Wait()
	if err != nil && c.stderr.String() != "" {
		err = fmt.Errorf("%v: %s", err, c.stderr.String())
	}
	c.o.Exited(err)
}

func (c *codex) request(id json.RawMessage, method string, raw json.RawMessage) {
	var p struct {
		ItemID  string `json:"itemId"`
		Command string `json:"command"`
		Reason  string `json:"reason"`
		Actions []struct {
			Command string `json:"command"`
		} `json:"commandActions"`
	}
	json.Unmarshal(raw, &p)
	key := "req-" + string(id)
	switch method {
	case "item/commandExecution/requestApproval":
		cmd := p.Command
		if len(p.Actions) > 0 && p.Actions[0].Command != "" {
			cmd = p.Actions[0].Command
		}
		c.mu.Lock()
		c.pending[key] = id
		c.mu.Unlock()
		c.o.Emit(Event{Kind: KindApproval, ID: key, Tool: "shell", Input: clip(cmd, 160), Text: p.Reason, Status: Pending})
	case "item/fileChange/requestApproval":
		c.mu.Lock()
		c.pending[key] = id
		it := c.items[p.ItemID]
		c.mu.Unlock()
		c.o.Emit(Event{Kind: KindApproval, ID: key, Tool: "edit", Input: changedFiles(it), Diff: changeDiff(it), Text: p.Reason, Status: Pending})
	default:
		c.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "tower does not handle " + method}})
	}
}

func (c *codex) notify(method string, raw json.RawMessage) {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item       codexItem   `json:"item"`
		ItemID     string      `json:"itemId"`
		Delta      string      `json:"delta"`
		TokenUsage codexTokens `json:"tokenUsage"`
		RequestID  json.RawMessage
		Error      struct {
			Message string `json:"message"`
		} `json:"error"`
		WillRetry bool `json:"willRetry"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	switch method {
	case "turn/started":
		c.mu.Lock()
		c.turn = p.Turn.ID
		c.mu.Unlock()
		c.o.Emit(Event{Kind: KindTurn, Status: TurnStarted})
	case "turn/completed":
		c.turnDone(p.Turn.Status, p.Turn.Error)
	case "item/started":
		c.itemStarted(p.Item)
	case "item/completed":
		c.itemCompleted(p.Item)
	case "item/agentMessage/delta":
		c.mu.Lock()
		c.open[p.ItemID] += p.Delta
		c.mu.Unlock()
		c.o.Emit(Event{Kind: KindDelta, ID: p.ItemID, Role: "assistant", Text: p.Delta})
	case "item/reasoning/summaryTextDelta":
		c.o.Emit(Event{Kind: KindDelta, ID: p.ItemID, Role: "reasoning", Text: p.Delta})
	case "item/commandExecution/outputDelta":
		c.mu.Lock()
		c.output[p.ItemID] += p.Delta
		out := c.output[p.ItemID]
		c.mu.Unlock()
		c.o.Emit(Event{Kind: KindTool, ID: p.ItemID, Output: clipOutput(out)})
	case "thread/tokenUsage/updated":
		c.mu.Lock()
		c.usage = p.TokenUsage
		c.mu.Unlock()
	case "error":
		if !p.WillRetry && p.Error.Message != "" {
			c.o.Emit(Event{Kind: KindInfo, Status: "error", Text: p.Error.Message})
		}
	}
}

func (c *codex) itemStarted(it codexItem) {
	c.mu.Lock()
	c.items[it.ID] = it
	c.mu.Unlock()
	switch it.Type {
	case "commandExecution":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Tool: "shell", Input: clip(commandOf(it), 160), Status: ToolRunning})
	case "fileChange":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Tool: "edit", Input: changedFiles(it), Diff: changeDiff(it), Status: ToolRunning})
	case "webSearch":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Tool: "search", Input: it.Query, Status: ToolRunning})
	case "mcpToolCall":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Tool: it.Server + "." + it.Tool, Input: clip(string(it.Args), 160), Status: ToolRunning})
	}
}

func (c *codex) itemCompleted(it codexItem) {
	c.mu.Lock()
	delete(c.open, it.ID)
	delete(c.output, it.ID)
	c.mu.Unlock()
	status := ToolOK
	switch {
	case it.Status == "declined":
		status = ToolDeclined
	case it.Status == "failed", it.ExitCode != nil && *it.ExitCode != 0, it.Error != nil:
		status = ToolFailed
	}
	switch it.Type {
	case "agentMessage":
		c.o.Emit(Event{Kind: KindText, ID: it.ID, Role: "assistant", Text: it.Text})
	case "reasoning":
		if len(it.Summary) > 0 {
			c.o.Emit(Event{Kind: KindText, ID: it.ID, Role: "reasoning", Text: strings.Join(it.Summary, "\n")})
		}
	case "commandExecution":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Status: status, Output: clipOutput(it.Output)})
	case "fileChange":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Status: status, Diff: changeDiff(it)})
	case "webSearch":
		c.o.Emit(Event{Kind: KindTool, ID: it.ID, Status: status, Input: it.Query})
	case "mcpToolCall":
		e := Event{Kind: KindTool, ID: it.ID, Status: status}
		if it.Error != nil {
			e.Output = it.Error.Message
		}
		c.o.Emit(e)
	}
}

func (c *codex) turnDone(status string, terr *struct {
	Message string `json:"message"`
}) {
	c.mu.Lock()
	c.turn = ""
	open := c.open
	c.open = map[string]string{}
	pending := c.pending
	c.pending = map[string]json.RawMessage{}
	u := c.usage
	out := u.Total.Output - c.prevOut
	c.prevOut = u.Total.Output
	c.mu.Unlock()
	for id, text := range open { // interrupted messages never complete; keep what streamed
		c.o.Emit(Event{Kind: KindText, ID: id, Role: "assistant", Text: text})
	}
	for id := range pending {
		c.o.Emit(Event{Kind: KindApproval, ID: id, Status: Denied, Text: "withdrawn"})
	}
	e := Event{Kind: KindTurn, Status: TurnDone, Usage: &Usage{Output: max(0, out), Context: u.Last.Total}}
	switch status {
	case "interrupted":
		e.Status = TurnInterrupted
	case "failed":
		e.Status = TurnFailed
		if terr != nil {
			e.Text = terr.Message
		}
	}
	c.o.Emit(e)
}

func commandOf(it codexItem) string {
	if len(it.Actions) > 0 && it.Actions[0].Command != "" {
		return it.Actions[0].Command
	}
	return it.Command
}

func changedFiles(it codexItem) string {
	var names []string
	for _, ch := range it.Changes {
		names = append(names, filepath.Base(ch.Path))
	}
	return strings.Join(names, ", ")
}

func changeDiff(it codexItem) string {
	var b strings.Builder
	for _, ch := range it.Changes {
		fmt.Fprintf(&b, "--- %s\n+++ %s\n%s", ch.Path, ch.Path, ch.Diff)
		if !strings.HasSuffix(ch.Diff, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Kill ends the agent's whole process group at once.
func (c *codex) Kill() {
	if c.cmd.Process != nil {
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
}
