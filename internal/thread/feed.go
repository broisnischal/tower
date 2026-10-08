package thread

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"tower/internal/agent"
)

// Feed turns a Claude Code transcript (the JSONL file every session writes)
// into the events a headless thread produces, so a Claude running in a tmux
// pane gets the same live chat. It reads only what was appended since the
// last call.
type Feed struct {
	pos   int64
	lines map[string]int  // assistant lines seen per message id = block index
	tools map[string]bool // tool_use ids seen, so results can find their card
}

type transcriptLine struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// Read returns the events for whatever the transcript gained.
func (f *Feed) Read(path string) []Event {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if st, err := file.Stat(); err == nil && st.Size() < f.pos {
		*f = Feed{} // rewritten
	}
	if f.lines == nil {
		f.lines, f.tools = map[string]int{}, map[string]bool{}
	}
	if _, err := file.Seek(f.pos, io.SeekStart); err != nil {
		return nil
	}
	var out []Event
	r := bufio.NewReaderSize(file, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line waits for the next call
		}
		f.pos += int64(len(line))
		out = append(out, f.parse(bytes.TrimSpace(line))...)
	}
	return out
}

func (f *Feed) parse(line []byte) []Event {
	var t transcriptLine
	if json.Unmarshal(line, &t) != nil || (t.Type != "user" && t.Type != "assistant") {
		return nil
	}
	at := int64(0)
	if ts, err := time.Parse(time.RFC3339Nano, t.Timestamp); err == nil {
		at = ts.UnixMilli()
	}
	stamp := func(e Event) Event { e.Time = at; return e }

	var blocks []block
	if s, ok := plainText(t.Message.Content); ok {
		blocks = []block{{Type: "text", Text: s}}
	} else {
		json.Unmarshal(t.Message.Content, &blocks)
	}
	var out []Event
	if t.Type == "user" {
		var said []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				said = append(said, b.Text)
			case "image":
				said = append(said, "[image]")
			case "tool_result":
				if !f.tools[b.ToolUseID] {
					continue
				}
				status := ToolOK
				if b.IsError {
					status = ToolFailed
				}
				out = append(out, stamp(Event{Kind: KindTool, ID: b.ToolUseID, Status: status, Output: clipOutput(resultText(b.Content))}))
			}
		}
		if len(said) > 0 && !t.IsMeta && !t.IsSidechain {
			out = append(out, stamp(userEvent(strings.Join(said, " "))))
		}
		return out
	}
	for _, b := range blocks {
		idx := f.lines[t.Message.ID]
		f.lines[t.Message.ID]++
		switch b.Type {
		case "text":
			if !t.IsSidechain && strings.TrimSpace(b.Text) != "" {
				out = append(out, stamp(Event{Kind: KindText, ID: t.Message.ID + ":" + itoa(idx), Role: "assistant", Text: b.Text}))
			}
		case "tool_use":
			f.tools[b.ID] = true
			in := decodeInput(b.Input)
			name := agent.ToolName(b.Name)
			if t.IsSidechain {
				name = "↳ " + name
			}
			out = append(out, stamp(Event{Kind: KindTool, ID: b.ID, Tool: name, Input: toolInput(in), Diff: editDiff(b.Name, in), Status: ToolRunning}))
		}
	}
	return out
}

// userEvent sorts what Claude Code records as a user message: my prompt, an
// interrupt, or harness text such as a command's output or a notification.
func userEvent(s string) Event {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "[Request interrupted"):
		return Event{Kind: KindTurn, Status: TurnInterrupted}
	case strings.HasPrefix(s, "<"):
		tag := strings.TrimPrefix(strings.SplitN(s, ">", 2)[0], "<")
		return Event{Kind: KindInfo, Text: strings.ReplaceAll(tag, "-", " ")}
	}
	return Event{Kind: KindUser, Text: s}
}

func plainText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
