package thread

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

func decodeInput(raw json.RawMessage) map[string]any {
	var in map[string]any
	json.Unmarshal(raw, &in)
	return in
}

// toolInput is the one-line gist of a tool call: the file, the command, the
// pattern or the URL.
func toolInput(in map[string]any) string {
	str := func(k string) string { v, _ := in[k].(string); return v }
	if p := or(str("file_path"), str("notebook_path"), str("path")); p != "" {
		return filepath.Base(p)
	}
	return clip(or(str("command"), str("pattern"), str("url"), str("query"), str("description"), str("skill"), str("prompt")), 160)
}

// editDiff renders a file edit as a unified-style diff for the chat.
func editDiff(tool string, in map[string]any) string {
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	path := str(in, "file_path")
	var b strings.Builder
	hunk := func(old, new string) {
		for _, l := range lines(old) {
			b.WriteString("-" + l + "\n")
		}
		for _, l := range lines(new) {
			b.WriteString("+" + l + "\n")
		}
	}
	switch tool {
	case "Edit":
		hunk(str(in, "old_string"), str(in, "new_string"))
	case "MultiEdit":
		edits, _ := in["edits"].([]any)
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				hunk(str(m, "old_string"), str(m, "new_string"))
				b.WriteString("@@\n")
			}
		}
	case "Write":
		hunk("", str(in, "content"))
	default:
		return ""
	}
	return fmt.Sprintf("--- %s\n+++ %s\n%s", path, path, strings.TrimSuffix(b.String(), "@@\n"))
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// resultText flattens a tool_result's content: a string or text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		} else if b.Type == "image" {
			out = append(out, "[image]")
		}
	}
	return strings.Join(out, "\n")
}

// clipOutput keeps the first 40 lines of a tool's output.
func clipOutput(s string) string {
	ls := strings.Split(strings.TrimRight(stripANSI(s), "\n"), "\n")
	if len(ls) > 40 {
		ls = append(ls[:40], fmt.Sprintf("… %d more lines", len(ls)-40))
	}
	return strings.Join(ls, "\n")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
