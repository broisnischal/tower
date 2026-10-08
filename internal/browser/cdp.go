package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// cdp is a Chrome DevTools Protocol connection to the browser endpoint.
// Page commands carry the session id of the target I attached to with
// flatten on, so one socket serves every tab.
//
// Events reach onEvent on the reader goroutine. A handler must never wait
// for a call there: the answer would have to come through the goroutine it
// is blocking. Handlers that need calls start a goroutine.
type cdp struct {
	ws      *wsConn
	onEvent func(event)
	next    atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan message

	done chan struct{}
	err  error // why done closed
}

type event struct {
	Method  string
	Params  json.RawMessage
	Session string
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

var errClosed = errors.New("devtools connection closed")

func newCDP(ws *wsConn, onEvent func(event)) *cdp {
	c := &cdp{ws: ws, onEvent: onEvent, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.read()
	return c
}

func (c *cdp) read() {
	for {
		b, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		if m.Method != "" && c.onEvent != nil {
			c.onEvent(event{m.Method, m.Params, m.SessionID})
		}
	}
}

func (c *cdp) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.err = errClosed
	if err != nil {
		c.err = fmt.Errorf("%w: %v", errClosed, err)
	}
	close(c.done)
}

func (c *cdp) write(id int64, session, method string, params any) error {
	type out struct {
		ID        int64  `json:"id"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
		SessionID string `json:"sessionId,omitempty"`
	}
	b, err := json.Marshal(out{id, method, params, session})
	if err != nil {
		return err
	}
	return c.ws.WriteText(b)
}

// call sends a command and waits for its answer, decoded into result when
// that is not nil.
func (c *cdp) call(ctx context.Context, session, method string, params, result any) error {
	id := c.next.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(id, session, method, params); err != nil {
		c.forget(id)
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %w", method, m.Error)
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return c.err
	}
}

func (c *cdp) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// send fires a command without waiting; its answer is dropped. Safe from
// the UI goroutine and from event handlers.
func (c *cdp) send(session, method string, params any) {
	c.write(c.next.Add(1), session, method, params)
}

func (c *cdp) close() error {
	c.fail(nil)
	return c.ws.Close()
}
