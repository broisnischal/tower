package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// entry is the slice of a transcript line tower cares about.
type entry struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input       int `json:"input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			Output      int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// texts returns the text of a message's content, which is either a plain
// string or a list of typed blocks.
func texts(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var blocks []struct{ Type, Text string }
	json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// Usage follows one transcript incrementally and keeps token totals.
type Usage struct {
	pos   int64
	out   map[string]int // output tokens per message id; one reply spans several lines
	Ctx   int            // tokens in context as of the last reply
	Out   int            // output tokens over the whole session
	Model string
}

// Update reads whatever the transcript gained since the last call.
func (u *Usage) Update(path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < u.pos {
		*u = Usage{} // transcript was rewritten
	}
	if _, err := f.Seek(u.pos, io.SeekStart); err != nil {
		return
	}
	if u.out == nil {
		u.out = map[string]int{}
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line waits for the next call
		}
		u.pos += int64(len(line))
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.IsSidechain || e.Message.Usage == nil {
			continue
		}
		us := e.Message.Usage
		id := e.Message.ID
		if id == "" {
			id = strconv.Itoa(len(u.out))
		}
		u.out[id] = us.Output
		u.Ctx = us.Input + us.CacheCreate + us.CacheRead
		if e.Message.Model != "" {
			u.Model = e.Message.Model
		}
	}
	u.Out = 0
	for _, n := range u.out {
		u.Out += n
	}
}

// LastReply returns the text of the agent's latest turn: every assistant text
// block after the last prompt.
func LastReply(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var reply []string
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e entry
			if json.Unmarshal(line, &e) == nil && !e.IsSidechain && !e.IsMeta {
				switch e.Type {
				case "user":
					if t := texts(e.Message.Content); len(t) > 0 && !strings.HasPrefix(t[0], "[Request interrupted") {
						reply = nil
					}
				case "assistant":
					reply = append(reply, texts(e.Message.Content)...)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(strings.Join(reply, "\n\n"))
}

// FinalReply is LastReply once it is there: the Stop hook can fire a moment
// before Claude Code writes the turn's last message to the transcript.
func FinalReply(path string) string {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if r := LastReply(path); r != "" || time.Now().After(deadline) {
			return r
		}
	}
}

// interrupted reports whether the session's last word is my Esc. Claude Code
// fires no Stop hook for an interrupted turn, but it writes this marker.
func interrupted(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	off := max(0, st.Size()-32<<10)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return false
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if json.Unmarshal(lines[i], &e) != nil || e.IsSidechain || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		if e.Type == "assistant" {
			return false
		}
		if e.IsMeta {
			continue
		}
		t := texts(e.Message.Content)
		if len(t) == 0 {
			return false // a tool result: the turn is still going
		}
		return strings.HasPrefix(t[0], "[Request interrupted by user")
	}
	return false
}
