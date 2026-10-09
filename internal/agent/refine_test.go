package agent

import "testing"

func TestNeedsRefine(t *testing.T) {
	for text, want := range map[string]bool{
		"yes":                                   false,
		"go ahead do it":                        false,
		"@api yes do it":                        false,
		"/compact":                              false,
		"!git status":                           false,
		"@api /clear":                           false,
		"fix the flicker when i resize the bar": true,
		"@api @docs check how the token refresh retries": true,
		"line one\nline two":                             true,
	} {
		if got := NeedsRefine(text); got != want {
			t.Errorf("NeedsRefine(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestSplitMentions(t *testing.T) {
	m, body := splitMentions("  @api @docs  fix [Image #1] here ")
	if m != "@api @docs  " || body != "fix [Image #1] here" {
		t.Errorf("got %q %q", m, body)
	}
	if m, body := splitMentions("email me@x.com"); m != "" || body != "email me@x.com" {
		t.Errorf("got %q %q", m, body)
	}
}

func TestCleanRefined(t *testing.T) {
	for in, want := range map[string]string{
		"```\nFix it.\n```":           "Fix it.",
		"```text\nFix it.\n```":       "Fix it.",
		`"Fix it."`:                   "Fix it.",
		`Run "make" then "make x".`:   `Run "make" then "make x".`,
		"  Fix it.\n":                 "Fix it.",
		"```go\nx := 1\n``` and more": "```go\nx := 1\n``` and more",
	} {
		if got := cleanRefined(in); got != want {
			t.Errorf("cleanRefined(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeepAttachments(t *testing.T) {
	got := keepAttachments("see [Image #1] and [File #2]", "Look at [Image #1].")
	if want := "Look at [Image #1].\n\n[File #2]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
