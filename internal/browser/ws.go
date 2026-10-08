package browser

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal WebSocket client (RFC 6455), enough for the Chrome DevTools
// Protocol: one connection to localhost, JSON in text messages. I write
// masked text frames and read whole messages, joining fragments and
// answering pings on the way.

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA

	// maxMessage caps one message. A full page PNG of a long page arrives
	// as one base64 string, so this is generous.
	maxMessage = 256 << 20

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex // one frame at a time on the wire
}

// dialWS opens a WebSocket to a ws:// URL.
func dialWS(raw string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := handshake(conn, r, u); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, r: r}, nil
}

// handshake sends the upgrade request and checks the server's answer.
func handshake(conn net.Conn, r *bufio.Reader, u *url.URL) error {
	var nonce [16]byte
	rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil {
		return fmt.Errorf("websocket handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("websocket handshake: %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return errors.New("websocket handshake: no upgrade")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return errors.New("websocket handshake: bad accept key")
	}
	return nil
}

// acceptKey is what the server must answer for a Sec-WebSocket-Key.
func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// WriteText sends one text message as a single masked frame.
func (w *wsConn) WriteText(p []byte) error { return w.write(opText, p) }

func (w *wsConn) write(op byte, p []byte) error {
	var mask [4]byte
	rand.Read(mask[:])
	frame := appendFrame(nil, op, true, p, mask)
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_, err := w.conn.Write(frame)
	return err
}

// appendFrame appends a client frame: clients always mask their payload.
func appendFrame(dst []byte, op byte, fin bool, payload []byte, mask [4]byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	dst = append(dst, b0)
	n := len(payload)
	switch {
	case n < 126:
		dst = append(dst, 0x80|byte(n))
	case n <= 0xFFFF:
		dst = append(dst, 0x80|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, 0x80|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(n))
	}
	dst = append(dst, mask[:]...)
	start := len(dst)
	dst = append(dst, payload...)
	for i := range n {
		dst[start+i] ^= mask[i&3]
	}
	return dst
}

type frame struct {
	fin     bool
	op      byte
	payload []byte
}

// readFrame reads one frame. Servers do not mask, but I unmask anyway if
// the bit is set.
func readFrame(r io.Reader) (frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	f := frame{fin: hdr[0]&0x80 != 0, op: hdr[0] & 0x0F}
	masked := hdr[1]&0x80 != 0
	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxMessage {
		return frame{}, fmt.Errorf("websocket: frame of %d bytes is too large", n)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return frame{}, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, err
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= mask[i&3]
		}
	}
	return f, nil
}

// ReadMessage returns the next text or binary message, joined from its
// fragments. Pings get a pong, pongs are dropped, and a close frame is
// echoed and ends the stream with io.EOF.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		f, err := readFrame(w.r)
		if err != nil {
			return nil, err
		}
		switch f.op {
		case opPing:
			if err := w.write(opPong, f.payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			w.write(opClose, closeCode(f.payload))
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("websocket: new message inside a fragmented one")
			}
			started, msg = true, f.payload
		case opCont:
			if !started {
				return nil, errors.New("websocket: continuation without a message")
			}
			if len(msg)+len(f.payload) > maxMessage {
				return nil, errors.New("websocket: message too large")
			}
			msg = append(msg, f.payload...)
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %#x", f.op)
		}
		if f.fin {
			return msg, nil
		}
	}
}

// closeCode keeps just the status code of a close payload for the echo.
func closeCode(p []byte) []byte {
	if len(p) >= 2 {
		return p[:2]
	}
	return nil
}

// Close sends a normal close frame and drops the connection.
func (w *wsConn) Close() error {
	w.conn.SetWriteDeadline(time.Now().Add(time.Second))
	w.write(opClose, []byte{0x03, 0xE8}) // 1000: normal closure
	return w.conn.Close()
}
