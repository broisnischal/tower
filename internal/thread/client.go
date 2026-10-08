package thread

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"tower/internal/agent"
)

// Client talks to the daemon.
type Client struct {
	conn  net.Conn
	mu    sync.Mutex
	next  int
	waits map[int]chan wire
	push  chan wire
}

// Push is a live update from the daemon after Watch.
type Push struct {
	Kind     string // thread | event | removed
	ThreadID string
	Thread   *Thread
	Event    *Event
}

// Dial connects to the daemon. With autostart it launches one when none is
// running, detached so it outlives this process.
func Dial(autostart bool) (*Client, error) {
	c, err := net.DialTimeout("unix", SocketPath(), time.Second)
	if err != nil && autostart {
		if err = launch(); err == nil {
			for range 50 {
				time.Sleep(100 * time.Millisecond)
				if c, err = net.DialTimeout("unix", SocketPath(), time.Second); err == nil {
					break
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	cl := &Client{conn: c, waits: map[int]chan wire{}, push: make(chan wire, 4096)}
	go cl.read()
	return cl, nil
}

func launch() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	os.MkdirAll(agent.Dir(), 0o700)
	logf, err := os.OpenFile(filepath.Join(agent.Dir(), "serve.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir, _ = os.UserHomeDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (c *Client) read() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var w wire
		if json.Unmarshal(sc.Bytes(), &w) != nil {
			continue
		}
		if w.Push != "" {
			c.push <- w
			continue
		}
		c.mu.Lock()
		ch := c.waits[w.ID]
		delete(c.waits, w.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- w
		}
	}
	c.mu.Lock()
	for id, ch := range c.waits {
		close(ch)
		delete(c.waits, id)
	}
	c.mu.Unlock()
	close(c.push)
}

// Call runs one operation and decodes its result into out (may be nil).
func (c *Client) Call(op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan wire, 1)
	c.waits[id] = ch
	b, _ := json.Marshal(wire{ID: id, Op: op, Args: raw})
	_, err = c.conn.Write(append(b, '\n'))
	c.mu.Unlock()
	if err != nil {
		return err
	}
	w, ok := <-ch
	if !ok {
		return errors.New("tower daemon went away")
	}
	if w.Error != "" {
		return errors.New(w.Error)
	}
	if out != nil && len(w.Data) > 0 {
		return json.Unmarshal(w.Data, out)
	}
	return nil
}

// Watch subscribes to live updates and returns the current threads. Updates
// arrive on Pushes.
func (c *Client) Watch() ([]Thread, error) {
	var ts []Thread
	err := c.Call("watch", nil, &ts)
	return ts, err
}

// Next blocks for the next live update; ok is false once the daemon is gone.
func (c *Client) Next() (Push, bool) {
	w, ok := <-c.push
	if !ok {
		return Push{}, false
	}
	return Push{Kind: w.Push, ThreadID: w.ThreadID, Thread: w.Thread, Event: w.Event}, true
}

// Close disconnects.
func (c *Client) Close() error { return c.conn.Close() }

// Ref names a thread in a call.
func Ref(thread string) map[string]string { return map[string]string{"thread": thread} }
