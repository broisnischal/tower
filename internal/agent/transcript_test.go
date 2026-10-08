package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	prompt     = `{"type":"user","message":{"role":"user","content":"fix the bug"}}`
	toolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
	interrupt  = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
)

func reply(id, text string, out int) string {
	return `{"type":"assistant","message":{"id":"` + id + `","model":"claude-x","content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":10,"cache_read_input_tokens":100,"output_tokens":` + itoa(out) + `}}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestLastReplyKeepsOnlyTheLatestTurn(t *testing.T) {
	p := writeTranscript(t, prompt, reply("a", "old answer", 1), prompt, reply("b", "first", 1), toolResult, reply("c", "second", 1))
	if got := LastReply(p); got != "first\n\nsecond" {
		t.Fatalf("LastReply = %q", got)
	}
}

func TestUsageCountsEachMessageOnce(t *testing.T) {
	// One reply is written as several lines that repeat the same usage.
	p := writeTranscript(t, prompt, reply("a", "x", 5), reply("a", "y", 5), reply("b", "z", 3))
	var u Usage
	u.Update(p)
	if u.Out != 8 || u.Ctx != 112 || u.Model != "claude-x" {
		t.Fatalf("usage = out %d ctx %d model %q", u.Out, u.Ctx, u.Model)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(reply("c", "w", 2) + "\n")
	f.Close()
	u.Update(p)
	if u.Out != 10 {
		t.Fatalf("after append out = %d", u.Out)
	}
}

func TestInterrupted(t *testing.T) {
	cases := map[string][]string{
		"esc after prompt":  {prompt, interrupt},
		"esc during a tool": {prompt, reply("a", "x", 1), toolResult, interrupt},
	}
	for name, lines := range cases {
		if !interrupted(writeTranscript(t, lines...)) {
			t.Errorf("%s: want interrupted", name)
		}
	}
	for name, lines := range map[string][]string{
		"thinking":     {prompt},
		"running tool": {prompt, reply("a", "x", 1)},
		"tool result":  {prompt, reply("a", "x", 1), toolResult},
		"next turn":    {interrupt, prompt},
	} {
		if interrupted(writeTranscript(t, lines...)) {
			t.Errorf("%s: want not interrupted", name)
		}
	}
}

func TestToolLabel(t *testing.T) {
	got := toolLabel("Edit", map[string]any{"file_path": "/a/b/refresh.ts"})
	if got != "Edit refresh.ts" {
		t.Fatalf("got %q", got)
	}
	got = toolLabel("Bash", map[string]any{"command": "go   test\n ./..."})
	if got != "Bash go test ./..." {
		t.Fatalf("got %q", got)
	}
}
