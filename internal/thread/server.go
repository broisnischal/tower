package thread

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"tower/internal/agent"
)

// SocketPath is where the daemon listens.
func SocketPath() string { return filepath.Join(agent.Dir(), "tower.sock") }

// wire is every message on the socket. A request has ID and Op; its reply
// has ID with OK or Error; a push from the daemon has Push.
type wire struct {
	ID    int             `json:"id,omitempty"`
	Op    string          `json:"op,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
	OK    bool            `json:"ok,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`

	Push     string  `json:"push,omitempty"` // thread | event | removed
	ThreadID string  `json:"thread_id,omitempty"`
	Thread   *Thread `json:"thread,omitempty"`
	Event    *Event  `json:"event,omitempty"`
}

// CreateArgs describes a new thread.
type CreateArgs struct {
	Engine   string `json:"engine"`
	Name     string `json:"name,omitempty"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

// live is a thread the daemon is managing.
type live struct {
	Thread
	eng     Engine
	gen     int              // bumps per engine start, to ignore exits of old ones
	partial map[string]Event // streamed text not final yet
	events  chan Event       // engine output, drained in order by pump
	op      sync.Mutex       // serialises start, send, stop on this thread
	removed bool
}

type server struct {
	mu      sync.Mutex
	threads map[string]*live
	conns   map[*conn]bool
	ln      net.Listener
	retries map[string]*time.Timer // pending "continue" per tmux session
}

type conn struct {
	c     net.Conn
	out   chan []byte
	watch bool
	once  sync.Once
}

// Serve runs the daemon until it is told to shut down or gets a signal.
func Serve() error {
	sock := SocketPath()
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return errors.New("already running")
	}
	os.Unsetenv("TMUX_PANE") // agents started from here are not tmux pane agents
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	s := &server{threads: map[string]*live{}, conns: map[*conn]bool{}, ln: ln, retries: map[string]*time.Timer{}}
	for _, t := range loadThreads() {
		t.Live, t.Pending = false, 0
		if t.Status == working || t.Status == waiting {
			t.Status, t.Activity = idle, "stopped: tower restarted"
		}
		s.add(t)
	}
	s.resumeRetries()
	s.resumeRelays()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; s.shutdown() }()
	log.Printf("tower serve: %d threads, listening on %s", len(s.threads), sock)
	for {
		c, err := ln.Accept()
		if err != nil {
			return nil // listener closed by shutdown
		}
		go s.handle(c)
	}
}

func (s *server) add(t Thread) *live {
	l := &live{Thread: t, partial: map[string]Event{}, events: make(chan Event, 4096)}
	s.threads[t.ID] = l
	go s.pump(l)
	return l
}

func (s *server) shutdown() {
	s.mu.Lock()
	var engs []Engine
	for _, t := range s.threads {
		if t.eng != nil {
			engs = append(engs, t.eng)
		}
	}
	s.mu.Unlock()
	for _, e := range engs {
		e.Close()
	}
	if len(engs) > 0 { // give agents a moment to exit on EOF, then make sure
		time.Sleep(1500 * time.Millisecond)
		for _, e := range engs {
			if k, ok := e.(interface{ Kill() }); ok {
				k.Kill()
			}
		}
	}
	s.ln.Close()
	os.Remove(SocketPath())
	os.Exit(0)
}

func (s *server) handle(nc net.Conn) {
	c := &conn{c: nc, out: make(chan []byte, 1024)}
	go func() {
		for b := range c.out {
			if _, err := nc.Write(b); err != nil {
				nc.Close()
				return
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.close()
	}()
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var req wire
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		data, err := s.do(c, req.Op, req.Args)
		res := wire{ID: req.ID, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		} else if data != nil {
			res.Data, _ = json.Marshal(data)
		}
		c.send(res)
	}
}

func (c *conn) send(w wire) {
	b, err := json.Marshal(w)
	if err != nil {
		return
	}
	select {
	case c.out <- append(b, '\n'):
	default: // a client that stopped reading loses its connection
		c.close()
	}
}

func (c *conn) close() {
	c.once.Do(func() { close(c.out); c.c.Close() })
}

func (s *server) broadcast(w wire) {
	s.mu.Lock()
	var cs []*conn
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.send(w)
	}
}

func (s *server) find(ref string) (*live, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.threads[ref]; ok {
		return t, nil
	}
	for _, t := range s.threads {
		if t.Name == ref {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no thread %q", ref)
}

func (s *server) list() []Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Thread, 0, len(s.threads))
	for _, t := range s.threads {
		out = append(out, t.Thread)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

type ref struct {
	Thread   string   `json:"thread"`
	Text     string   `json:"text,omitempty"`
	Name     string   `json:"name,omitempty"`
	Images   []string `json:"images,omitempty"`
	Approval string   `json:"approval,omitempty"`
	Decision Decision `json:"decision,omitempty"`
}

func (s *server) do(c *conn, op string, raw json.RawMessage) (any, error) {
	var r ref
	if op != "create" && op != "relay" && op != "retry" && len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
	}
	switch op {
	case "ping":
		return "pong", nil
	case "list":
		return s.list(), nil
	case "watch":
		s.mu.Lock()
		s.conns[c] = true
		s.mu.Unlock()
		return s.list(), nil
	case "create":
		var a CreateArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return s.create(a)
	case "shutdown":
		go s.shutdown()
		return nil, nil
	case "relay":
		var a RelayArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return nil, s.relay(a)
	case "retry":
		var a RetryArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		s.scheduleRetry(a)
		return nil, nil
	}

	t, err := s.find(r.Thread)
	if err != nil {
		return nil, err
	}
	switch op {
	case "history":
		ev, err := history(t.ID)
		s.mu.Lock()
		for _, p := range t.partial {
			ev = append(ev, p)
		}
		s.mu.Unlock()
		return ev, err
	case "send":
		return nil, s.send(t, Message{Text: r.Text, Images: r.Images})
	case "answer":
		eng, err := s.engine(t, false)
		if err != nil {
			return nil, err
		}
		return nil, eng.Answer(r.Approval, r.Decision)
	case "interrupt":
		eng, err := s.engine(t, false)
		if err != nil {
			return nil, err
		}
		return nil, eng.Interrupt()
	case "stop":
		s.stop(t)
		return nil, nil
	case "seen":
		s.update(t, func(th *Thread) {
			if th.Status == done {
				th.Status = idle
			}
		})
		return nil, nil
	case "rename":
		name := agent.Slug(r.Name)
		if name == "" {
			return nil, errors.New("empty name")
		}
		s.update(t, func(th *Thread) { th.Name = name })
		return nil, nil
	case "remove":
		s.stop(t)
		s.mu.Lock()
		t.removed = true
		delete(s.threads, t.ID)
		s.mu.Unlock()
		s.broadcast(wire{Push: "removed", ThreadID: t.ID})
		return nil, removeThread(t.ID)
	}
	return nil, fmt.Errorf("unknown op %q", op)
}

func (s *server) create(a CreateArgs) (Thread, error) {
	if a.Engine == "" {
		a.Engine = "claude"
	}
	if a.Cwd == "" {
		return Thread{}, errors.New("no working directory")
	}
	enginesMu.Lock()
	_, ok := engines[a.Engine]
	enginesMu.Unlock()
	if !ok {
		return Thread{}, fmt.Errorf("unknown engine %q", a.Engine)
	}
	s.mu.Lock()
	used := map[string]bool{}
	for _, t := range s.threads {
		used[t.Name] = true
	}
	s.mu.Unlock()
	name := agent.Slug(a.Name)
	if name == "" || used[name] {
		base := or(name, a.Engine)
		for i := 1; ; i++ {
			if n := fmt.Sprintf("%s-%d", base, i); !used[n] {
				name = n
				break
			}
		}
	}
	now := time.Now().Unix()
	t := Thread{ID: newID(), Name: name, Engine: a.Engine, Cwd: a.Cwd, Model: a.Model, Mode: a.Mode, Status: idle, Created: now, Updated: now}
	if a.Worktree {
		dir, err := agent.Worktree(a.Cwd, name)
		if err != nil {
			return Thread{}, err
		}
		t.Cwd, t.Branch = dir, "tower/"+name
	}
	if err := saveThread(t); err != nil {
		return Thread{}, err
	}
	s.mu.Lock()
	l := s.add(t)
	s.mu.Unlock()
	s.broadcast(wire{Push: "thread", Thread: &t})
	if _, err := s.engine(l, true); err != nil {
		return t, err
	}
	if a.Prompt != "" {
		if err := s.send(l, Message{Text: a.Prompt}); err != nil {
			return t, err
		}
	}
	return s.snapshot(l), nil
}

func (s *server) snapshot(t *live) Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	return t.Thread
}

// engine returns the thread's running engine, starting (or resuming) one
// when start is set.
func (s *server) engine(t *live, start bool) (Engine, error) {
	t.op.Lock()
	defer t.op.Unlock()
	s.mu.Lock()
	eng, removed := t.eng, t.removed
	o := StartOpts{Thread: t.ID, Cwd: t.Cwd, Model: t.Model, Mode: t.Mode, Resume: t.Resume}
	s.mu.Unlock()
	if removed {
		return nil, errors.New("thread was removed")
	}
	if eng != nil {
		return eng, nil
	}
	if !start {
		return nil, errors.New("the agent is not running; send a message to start it")
	}
	s.mu.Lock()
	t.gen++
	gen := t.gen
	s.mu.Unlock()
	o.Emit = func(e Event) { t.events <- e }
	o.Exited = func(err error) { s.exited(t, gen, err) }
	eng, err := startEngine(t.Engine, o)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	t.eng = eng
	s.mu.Unlock()
	s.update(t, func(th *Thread) { th.Live = true })
	return eng, nil
}

func (s *server) send(t *live, m Message) error {
	eng, err := s.engine(t, true)
	if err != nil {
		return err
	}
	text := m.Text
	for _, img := range m.Images {
		text += "\n[image " + filepath.Base(img) + "]"
	}
	t.events <- Event{Kind: KindUser, Text: text}
	return eng.Send(m)
}

func (s *server) stop(t *live) {
	t.op.Lock()
	s.mu.Lock()
	eng := t.eng
	t.eng = nil
	t.gen++ // whatever the old engine reports on exit no longer counts
	s.mu.Unlock()
	t.op.Unlock()
	if eng != nil {
		eng.Close()
	}
	s.update(t, func(th *Thread) {
		th.Live, th.Pending = false, 0
		if th.Status == working || th.Status == waiting {
			th.Status, th.Activity = idle, "stopped"
		}
	})
}

func (s *server) exited(t *live, gen int, err error) {
	s.mu.Lock()
	if t.gen != gen {
		s.mu.Unlock()
		return
	}
	t.eng = nil
	s.mu.Unlock()
	msg := "agent exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	t.events <- Event{Kind: KindInfo, Status: "error", Text: msg}
	s.update(t, func(th *Thread) {
		th.Live, th.Pending = false, 0
		if th.Status == working || th.Status == waiting {
			th.Status = idle
		}
	})
}

// update changes a thread's metadata, saves it and tells the watchers.
func (s *server) update(t *live, f func(*Thread)) {
	s.mu.Lock()
	before := t.Thread
	f(&t.Thread)
	changed := t.Thread != before
	if changed {
		t.Updated = time.Now().Unix()
	}
	snap := t.Thread
	s.mu.Unlock()
	if changed {
		saveThread(snap)
		s.broadcast(wire{Push: "thread", Thread: &snap})
	}
}

// pump records a thread's events in order: stamps them, stores them, folds
// them into the thread's metadata and fans them out.
func (s *server) pump(t *live) {
	var seq int64
	if ev, _ := history(t.ID); len(ev) > 0 {
		seq = ev[len(ev)-1].Seq
	}
	for e := range t.events {
		s.mu.Lock()
		gone := t.removed
		s.mu.Unlock()
		if gone {
			continue
		}
		seq++
		e.Seq, e.Time = seq, time.Now().UnixMilli()
		s.mu.Lock()
		switch e.Kind {
		case KindDelta:
			p := t.partial[e.ID]
			p.Kind, p.ID, p.Role, p.Text = KindText, e.ID, e.Role, p.Text+e.Text
			p.Seq, p.Time = e.Seq, e.Time
			t.partial[e.ID] = p
		case KindText:
			delete(t.partial, e.ID)
		}
		s.mu.Unlock()
		if e.Kind != KindDelta {
			appendEvent(t.ID, e)
		}
		s.broadcast(wire{Push: "event", ThreadID: t.ID, Event: &e})
		s.update(t, func(th *Thread) { fold(th, e) })
		if e.Kind == KindTurn && e.Status == TurnFailed {
			s.retryThread(t, e.Text)
		}
	}
}

// fold applies one event to a thread's metadata.
func fold(t *Thread, e Event) {
	now := time.Now().Unix()
	switch e.Kind {
	case KindSession:
		t.Resume = e.Text
		if e.Input != "" {
			t.Model = e.Input
		}
	case KindUser:
		t.Status, t.Activity, t.Prompt, t.TurnStarted, t.RetryAt = working, "thinking", clip(e.Text, 200), now, 0
	case KindTool:
		if e.Status == ToolRunning {
			t.Activity = clip(e.Tool+" "+e.Input, 100)
		}
	case KindApproval:
		if e.Status == Pending {
			t.Pending++
			t.Status, t.Activity = waiting, clip("approve "+e.Tool+" "+e.Input, 100)
		} else if t.Pending > 0 {
			t.Pending--
			if t.Pending == 0 && t.Status == waiting {
				t.Status, t.Activity = working, "thinking"
			}
		}
	case KindTurn:
		switch e.Status {
		case TurnStarted:
			t.Status = working
		case TurnDone:
			t.Status, t.Activity, t.Pending, t.Retries, t.RetryAt = done, "finished", 0, 0, 0
		case TurnInterrupted:
			t.Status, t.Activity, t.Pending = idle, "interrupted", 0
		case TurnFailed:
			t.Status, t.Activity, t.Pending = idle, clip("failed: "+e.Text, 100), 0
		}
		if u := e.Usage; u != nil {
			t.Usage.Input += u.Input
			t.Usage.Output += u.Output
			t.Usage.CostUSD += u.CostUSD
			if u.Context > 0 {
				t.Usage.Context = u.Context
			}
		}
	case KindInfo:
		if e.Status == "error" {
			t.Activity = clip(e.Text, 100)
		}
	}
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
