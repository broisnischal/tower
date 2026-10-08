package browser

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAcceptKey(t *testing.T) {
	// The example from RFC 6455, section 1.3.
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %q", got)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	for _, n := range []int{0, 5, 125, 126, 127, 0xFFFF, 0x10000, 300000} {
		payload := bytes.Repeat([]byte("abcdefg"), n/7+1)[:n]
		b := appendFrame(nil, opText, true, payload, mask)
		if b[1]&0x80 == 0 {
			t.Fatalf("n=%d: client frame not masked", n)
		}
		if n > 0 && bytes.Contains(b, payload) {
			t.Fatalf("n=%d: payload sent in the clear", n)
		}
		f, err := readFrame(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !f.fin || f.op != opText || !bytes.Equal(f.payload, payload) {
			t.Fatalf("n=%d: got fin=%v op=%d len=%d", n, f.fin, f.op, len(f.payload))
		}
	}
}

func TestFrameLengthEncoding(t *testing.T) {
	var mask [4]byte
	cases := []struct {
		n      int
		header []byte
	}{
		{125, []byte{0x81, 0x80 | 125}},
		{126, []byte{0x81, 0x80 | 126, 0, 126}},
		{0x10000, []byte{0x81, 0x80 | 127, 0, 0, 0, 0, 0, 1, 0, 0}},
	}
	for _, c := range cases {
		b := appendFrame(nil, opText, true, make([]byte, c.n), mask)
		if !bytes.HasPrefix(b, c.header) {
			t.Errorf("n=%d: header % x, want % x", c.n, b[:len(c.header)], c.header)
		}
	}
}

// serverFrame is an unmasked frame as a server sends it.
func serverFrame(op byte, fin bool, p []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	switch {
	case len(p) < 126:
		out = append(out, byte(len(p)))
	case len(p) <= 0xFFFF:
		out = append(out, 126)
		out = binary.BigEndian.AppendUint16(out, uint16(len(p)))
	default:
		out = append(out, 127)
		out = binary.BigEndian.AppendUint64(out, uint64(len(p)))
	}
	return append(out, p...)
}

// pipeConn is a wsConn over net.Pipe; the other end plays the server.
func pipeConn() (*wsConn, net.Conn) {
	c, s := net.Pipe()
	return &wsConn{conn: c, r: bufio.NewReader(c)}, s
}

func TestReadMessageFragmentsAndPing(t *testing.T) {
	w, srv := pipeConn()
	defer srv.Close()
	go func() {
		srv.Write(serverFrame(opText, false, []byte(`{"id":1,`)))
		srv.Write(serverFrame(opPing, true, []byte("hi")))
		srv.Write(serverFrame(opCont, false, []byte(`"result"`)))
		srv.Write(serverFrame(opCont, true, []byte(`:{}}`)))
		srv.Write(serverFrame(opClose, true, []byte{0x03, 0xE8}))
	}()
	// The pong and the close echo come back on the server side.
	replies := make(chan frame, 2)
	go func() {
		for range 2 {
			f, err := readFrame(srv)
			if err != nil {
				return
			}
			replies <- f
		}
	}()
	msg, err := w.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != `{"id":1,"result":{}}` {
		t.Fatalf("message = %q", msg)
	}
	if pong := <-replies; pong.op != opPong || string(pong.payload) != "hi" {
		t.Fatalf("pong = op %d %q", pong.op, pong.payload)
	}
	if _, err := w.ReadMessage(); err != io.EOF {
		t.Fatalf("after close: %v, want EOF", err)
	}
	if c := <-replies; c.op != opClose || !bytes.Equal(c.payload, []byte{0x03, 0xE8}) {
		t.Fatalf("close echo = op %d % x", c.op, c.payload)
	}
}

func TestReadMessageRejectsStrayContinuation(t *testing.T) {
	w, srv := pipeConn()
	defer srv.Close()
	go srv.Write(serverFrame(opCont, true, []byte("x")))
	if _, err := w.ReadMessage(); err == nil {
		t.Fatal("continuation without a message was accepted")
	}
}

func TestDialHandshakeAndEcho(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		req, err := http.ReadRequest(r)
		if err != nil || req.URL.Path != "/devtools/browser/x" || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			return
		}
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + acceptKey(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
		f, err := readFrame(r)
		if err != nil {
			return
		}
		got <- string(f.payload)
		big := bytes.Repeat([]byte("z"), 70000)
		c.Write(serverFrame(opText, true, big))
	}()
	w, err := dialWS("ws://"+ln.Addr().String()+"/devtools/browser/x", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.WriteText([]byte(`{"id":1,"method":"Browser.getVersion"}`)); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != `{"id":1,"method":"Browser.getVersion"}` {
		t.Fatalf("server read %q", s)
	}
	msg, err := w.ReadMessage()
	if err != nil || len(msg) != 70000 {
		t.Fatalf("read %d bytes, %v", len(msg), err)
	}
}

func TestDialRejectsBadAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		http.ReadRequest(bufio.NewReader(c))
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: nope\r\n\r\n"))
	}()
	if _, err := dialWS("ws://"+ln.Addr().String()+"/", 2*time.Second); err == nil {
		t.Fatal("bad accept key was accepted")
	}
}
