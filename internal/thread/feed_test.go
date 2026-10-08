package thread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedReadsATranscriptIncrementally(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	write(
		`{"type":"user","timestamp":"2026-10-08T05:00:00Z","message":{"content":"run the tests"}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Running them."}]}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
	)
	var f Feed
	var chat Chat
	for _, e := range f.Read(p) {
		chat.Apply(e)
	}
	if len(chat.Items) != 3 || chat.Items[2].Status != ToolRunning || chat.Items[2].Input != "go test ./..." {
		t.Fatalf("first read: %+v", chat.Items)
	}
	write(
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok  tower 0.2s","is_error":false}]}}`,
		`{"type":"user","isMeta":true,"message":{"content":"<system-reminder>x</system-reminder>"}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
	)
	for _, e := range f.Read(p) {
		chat.Apply(e)
	}
	if chat.Items[2].Status != ToolOK || chat.Items[2].Output != "ok  tower 0.2s" {
		t.Fatalf("tool result not folded in: %+v", chat.Items[2])
	}
	if last := chat.Items[len(chat.Items)-1]; last.Kind != KindTurn || last.Status != TurnInterrupted {
		t.Fatalf("interrupt: %+v", last)
	}
}
