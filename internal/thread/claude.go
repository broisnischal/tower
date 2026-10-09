package thread

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"tower/internal/agent"
)

// claude drives Claude Code headlessly the way the Agent SDK does: stream-json
// on stdin and stdout, with permission prompts routed to me over the control
// protocol (--permission-prompt-tool stdio).
type claude struct {
	o      StartOpts
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr *tail

	mu       sync.Mutex
	pending  map[string]approvalReq // control request id -> what it asks
	denied   map[string]bool        // tool_use ids I declined
	tools    map[string]string      // tool_use id -> tool name
	lines    map[string]int         // assistant lines seen per message id = block index
	msgID    string                 // message being streamed
	blocks   map[int]string         // stream block index -> text item id
	ctx      int                    // context size at the last message_start
	cost     float64                // session cost so far, to turn totals into per-turn deltas
	sessSent bool
	closing  bool
	reqN     int
}

type approvalReq struct {
	toolUseID string
	tool      string
	input     json.RawMessage
}

func init() { register("claude", newClaude) }

func newClaude(o StartOpts) (Engine, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, errors.New("claude is not on PATH")
	}
	mode := o.Mode
	if mode == "" {
		mode = "default"
	}
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-prompt-tool", "stdio", "--permission-mode", mode,
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}
	c := &claude{
		o: o, stderr: &tail{},
		pending: map[string]approvalReq{}, denied: map[string]bool{}, tools: map[string]string{},
		lines: map[string]int{}, blocks: map[int]string{},
	}
	c.cmd = exec.Command(bin, args...)
	c.cmd.Dir = o.Cwd
	c.cmd.Env = claudeEnv(o.Thread)
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
	return c, nil
}

// claudeEnv drops what would tie the agent to whatever session started the
// daemon, and turns off claude-sudo's auto-arm: its gate approves every tool,
// so my approvals would never be asked.
func claudeEnv(thread string) []string {
	var env []string
	for _, kv := range agentEnv(thread) {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "NODE_OPTIONS" || k == "DEBUG" || k == "CLAUDE_SUDO_AUTOARM" ||
			(strings.HasPrefix(k, "CLAUDE") && k != "CLAUDE_CONFIG_DIR") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_SUDO_AUTOARM=off")
}

func (c *claude) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *claude) Send(m Message) error {
	var content any = m.Text
	if len(m.Images) > 0 {
		blocks := []map[string]any{}
		if m.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": m.Text})
		}
		for _, p := range m.Images {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
			if mt == "" {
				mt = "image/png"
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": mt, "data": base64.StdEncoding.EncodeToString(data)}})
		}
		content = blocks
	}
	return c.write(map[string]any{
		"type": "user", "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": content},
	})
}

func (c *claude) control(req map[string]any) error {
	c.mu.Lock()
	c.reqN++
	id := fmt.Sprintf("tower-%d", c.reqN)
	c.mu.Unlock()
	return c.write(map[string]any{"type": "control_request", "request_id": id, "request": req})
}

func (c *claude) Interrupt() error { return c.control(map[string]any{"subtype": "interrupt"}) }

func (c *claude) Answer(id string, d Decision) error {
	c.mu.Lock()
	req, ok := c.pending[id]
	delete(c.pending, id)
	if ok && d == Deny {
		c.denied[req.toolUseID] = true
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending approval %q", id)
	}
	resp := map[string]any{"behavior": "allow", "toolUseID": req.toolUseID}
	if len(req.input) > 0 {
		resp["updatedInput"] = req.input
	}
	switch d {
	case Deny:
		resp = map[string]any{"behavior": "deny", "message": "I declined this in tower.", "toolUseID": req.toolUseID}
	case Always: // for the rest of this process only; nothing is written to settings
		resp["updatedPermissions"] = []map[string]any{{
			"type": "addRules", "behavior": "allow", "destination": "session",
			"rules": []map[string]any{{"toolName": req.tool}},
		}}
	}
	status := Allowed
	if d == Deny {
		status = Denied
	}
	c.o.Emit(Event{Kind: KindApproval, ID: id, Status: status})
	return c.write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": resp}})
}

func (c *claude) Close() error {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	c.mu.Unlock()
	c.stdin.Close()
	go func() { // what the SDK does: EOF, then TERM after 2 s, then KILL
		time.Sleep(2 * time.Second)
		if c.cmd.ProcessState == nil {
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
			time.Sleep(5 * time.Second)
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	return nil
}

// msg is the subset of every stdout line tower reads.
type claudeMsg struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"session_id"`
	Model          string          `json:"model"`
	Status         *string         `json:"status"`
	RequestID      string          `json:"request_id"`
	Request        json.RawMessage `json:"request"`
	Event          json.RawMessage `json:"event"`
	Message        json.RawMessage `json:"message"`
	Parent         *string         `json:"parent_tool_use_id"`
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	Errors         []string        `json:"errors"`
	TerminalReason string          `json:"terminal_reason"`
	TotalCost      float64         `json:"total_cost_usd"`
	Usage          *apiUsage       `json:"usage"`
	ToolName       string          `json:"tool_name"`
	DecisionReason string          `json:"decision_reason"`
	RateLimit      *rateLimitInfo  `json:"rate_limit_info"`
}

// rateLimitInfo is the plan's usage as a rate_limit_event reports it.
type rateLimitInfo struct {
	Windows map[string]struct {
		Utilization float64 `json:"utilization"` // 0 to 1
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

type apiUsage struct {
	Input       int `json:"input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	Output      int `json:"output_tokens"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (c *claude) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m claudeMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		c.handle(m)
	}
	err := c.cmd.Wait()
	if err != nil && c.stderr.String() != "" {
		err = fmt.Errorf("%v: %s", err, c.stderr.String())
	}
	c.o.Exited(err)
}

func (c *claude) handle(m claudeMsg) {
	sub := m.Parent != nil && *m.Parent != "" // a subagent's traffic
	switch m.Type {
	case "rate_limit_event": // the account's usage limits, which every agent shares
		if m.RateLimit != nil {
			l := agent.Limits{Updated: time.Now().Unix(), Source: "headless"}
			for name, w := range m.RateLimit.Windows {
				win := &agent.Window{Used: w.Utilization * 100, ResetsAt: w.ResetsAt}
				switch name {
				case "five_hour":
					l.FiveHour = win
				case "seven_day":
					l.SevenDay = win
				}
			}
			if l.FiveHour != nil || l.SevenDay != nil {
				agent.WriteLimits(l)
			}
		}
	case "system":
		switch m.Subtype {
		case "init": // start of every turn
			c.mu.Lock()
			first := !c.sessSent
			c.sessSent = true
			c.mu.Unlock()
			if first {
				c.o.Emit(Event{Kind: KindSession, Text: m.SessionID, Input: m.Model})
			}
			c.o.Emit(Event{Kind: KindTurn, Status: TurnStarted})
		case "status":
			if m.Status != nil && *m.Status == "compacting" {
				c.o.Emit(Event{Kind: KindInfo, Text: "compacting context"})
			}
		case "permission_denied":
			c.o.Emit(Event{Kind: KindInfo, Status: "error", Text: "denied " + m.ToolName + ": " + m.DecisionReason})
		}
	case "stream_event":
		if !sub {
			c.stream(m.Event)
		}
	case "assistant":
		c.assistant(m.Message, sub)
	case "user":
		c.toolResults(m.Message)
	case "control_request":
		c.request(m.RequestID, m.Request)
	case "control_cancel_request":
		c.mu.Lock()
		_, ok := c.pending[m.RequestID]
		delete(c.pending, m.RequestID)
		c.mu.Unlock()
		if ok {
			c.o.Emit(Event{Kind: KindApproval, ID: m.RequestID, Status: Denied, Text: "withdrawn"})
		}
	case "result":
		c.result(m)
	}
}

func (c *claude) stream(raw json.RawMessage) {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string    `json:"id"`
			Usage *apiUsage `json:"usage"`
		} `json:"message"`
		Block block `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Type {
	case "message_start":
		c.msgID, c.blocks = ev.Message.ID, map[int]string{}
		if u := ev.Message.Usage; u != nil {
			c.ctx = u.Input + u.CacheCreate + u.CacheRead
		}
	case "content_block_start":
		if ev.Block.Type == "text" {
			c.blocks[ev.Index] = fmt.Sprintf("%s:%d", c.msgID, ev.Index)
		}
	case "content_block_delta":
		if id, ok := c.blocks[ev.Index]; ok && ev.Delta.Type == "text_delta" {
			c.o.Emit(Event{Kind: KindDelta, ID: id, Role: "assistant", Text: ev.Delta.Text})
		}
	}
}

func (c *claude) assistant(raw json.RawMessage, sub bool) {
	var msg struct {
		ID      string  `json:"id"`
		Content []block `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	for _, b := range msg.Content {
		c.mu.Lock()
		idx := c.lines[msg.ID]
		c.lines[msg.ID]++
		if b.Type == "tool_use" {
			c.tools[b.ID] = b.Name
		}
		c.mu.Unlock()
		switch b.Type {
		case "text":
			if !sub {
				c.o.Emit(Event{Kind: KindText, ID: fmt.Sprintf("%s:%d", msg.ID, idx), Role: "assistant", Text: b.Text})
			}
		case "tool_use":
			in := decodeInput(b.Input)
			name := agent.ToolName(b.Name)
			if sub {
				name = "↳ " + name
			}
			c.o.Emit(Event{Kind: KindTool, ID: b.ID, Tool: name, Input: toolInput(in), Diff: editDiff(b.Name, in), Status: ToolRunning})
		}
	}
}

func (c *claude) toolResults(raw json.RawMessage) {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	var blocks []block
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		c.mu.Lock()
		denied := c.denied[b.ToolUseID]
		_, known := c.tools[b.ToolUseID]
		c.mu.Unlock()
		if !known {
			continue
		}
		status := ToolOK
		switch {
		case denied:
			status = ToolDeclined
		case b.IsError:
			status = ToolFailed
		}
		c.o.Emit(Event{Kind: KindTool, ID: b.ToolUseID, Status: status, Output: clipOutput(resultText(b.Content))})
	}
}

func (c *claude) request(id string, raw json.RawMessage) {
	var req struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Reason    string          `json:"decision_reason"`
		AgentID   string          `json:"agent_id"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return
	}
	if req.Subtype != "can_use_tool" {
		c.write(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": id, "error": "tower does not handle " + req.Subtype}})
		return
	}
	c.mu.Lock()
	c.pending[id] = approvalReq{toolUseID: req.ToolUseID, tool: req.ToolName, input: req.Input}
	c.mu.Unlock()
	in := decodeInput(req.Input)
	tool := req.ToolName
	if req.AgentID != "" {
		tool = "↳ " + tool
	}
	c.o.Emit(Event{Kind: KindApproval, ID: id, Tool: tool, Input: toolInput(in), Diff: editDiff(req.ToolName, in),
		Text: stripANSI(req.Reason), Status: Pending})
}

func (c *claude) result(m claudeMsg) {
	c.mu.Lock()
	cost := m.TotalCost - c.cost
	c.cost = m.TotalCost
	ctx := c.ctx
	c.pending = map[string]approvalReq{}
	c.mu.Unlock()
	u := &Usage{Context: ctx, CostUSD: max(0, cost)}
	if m.Usage != nil {
		u.Input = m.Usage.Input + m.Usage.CacheCreate + m.Usage.CacheRead
		u.Output = m.Usage.Output
	}
	e := Event{Kind: KindTurn, Status: TurnDone, Usage: u}
	switch {
	case strings.HasPrefix(m.TerminalReason, "aborted"):
		e.Status = TurnInterrupted
	case m.IsError:
		e.Status, e.Text = TurnFailed, strings.Join(m.Errors, "; ")
		if e.Text == "" {
			e.Text = m.Result
		}
	}
	c.o.Emit(e)
}

// tail keeps the last few KB a process wrote to stderr.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// Kill ends the agent's whole process group at once.
func (c *claude) Kill() {
	if c.cmd.Process != nil {
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
}
