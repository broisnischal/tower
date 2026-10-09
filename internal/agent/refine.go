package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// refineSystem turns a rough message into the prompt I would have written
// with time to think. It may only restate what I said or what the context
// holds: a confident rewrite that invents a cause or a file sends the agent
// the wrong way, which is worse than a vague message.
const refineSystem = `I type or dictate quick, rough messages to a Claude Code agent working in one of my tmux panes. Rewrite my message into the prompt I would have written with time to think: clear, specific and complete, so the agent gets it right the first time.

Rules:
- Keep my intent, scope and every concrete detail (names, paths, numbers, error text, URLs, code). Never add requirements, files, causes, libraries or other technical facts that are not in my message or the context. Where something is unknown, tell the agent to find out (read the code, check, ask me) instead of guessing.
- Fix speech to text mistakes, typos and grammar, using the context to work out what I meant. Write in English.
- Write as me, to the agent: first person, imperative ("Fix", "Check", "Don't"). Plain ASCII punctuation, no em dashes.
- Size it to the task. A reply or follow-up in an ongoing conversation ("yes do that", "try again", an answer to its question) keeps its length: fix the wording and resolve references, add nothing, since the agent already has the whole conversation. "yes go ahead and commit it" becomes "Yes, go ahead and commit it." A real task gets the goal, the context that matters, what to do and what to leave alone, and how to tell it is done (what to run or check, what to report back). Use a numbered list only for real steps.
- Use the agent's current task and last reply to resolve "it", "that" and "the thing". Don't repeat what it already knows beyond what the message needs.
- Keep placeholders like [Image #1], [Video #2] or [File #3] exactly as written, where they fit in the text.
- Don't do the task, answer it, or ask me anything. Output only the rewritten prompt, with no preamble, quotes or code fences around it.`

var (
	mentionsRE = regexp.MustCompile(`^(?:@\S+\s+)+`)
	attachRE   = regexp.MustCompile(`\[(?:Image|Video|File) #\d+\]`)
)

// NeedsRefine reports whether a message is worth rewriting: not a slash
// command or a shell escape, and more than a short reply like "yes do it".
func NeedsRefine(text string) bool {
	_, body := splitMentions(text)
	if body == "" || strings.HasPrefix(body, "/") || strings.HasPrefix(body, "!") {
		return false
	}
	return strings.Contains(body, "\n") || len(strings.Fields(body)) > 4
}

// Refine rewrites a rough message to a into a clear prompt with one quick
// claude -p run: no tools, no settings, so none of my hooks fire and it
// doesn't show up in tower as an agent. Leading @mentions and attachment
// placeholders come through untouched. a may be nil.
func Refine(text string, a *Agent, model string) (string, error) {
	mentions, body := splitMentions(text)
	if body == "" {
		return text, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", model, "--tools", "",
		"--strict-mcp-config", "--setting-sources", "", "--no-session-persistence",
		"--output-format", "stream-json", "--verbose", "--system-prompt", refineSystem)
	cmd.Stdin = strings.NewReader(refineInput(body, a))
	cmd.Env = refineEnv()
	if a != nil && a.Cwd != "" {
		cmd.Dir = a.Cwd
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("rewrite failed: %v", err)
	}
	// The result event comes about 1.3s before claude exits, so read it off
	// the stream and stop the process instead of waiting for its shutdown.
	res, found := streamResult(out)
	cmd.Process.Kill()
	cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return "", errors.New("rewrite timed out")
	case !found:
		return "", fmt.Errorf("rewrite failed: %s", clip(or(strings.TrimSpace(stderr.String()), "claude printed no result"), 120))
	case res.IsError:
		return "", fmt.Errorf("rewrite failed: %s", clip(res.Result, 120))
	}
	refined := cleanRefined(res.Result)
	if refined == "" {
		return "", errors.New("rewrite came back empty")
	}
	return mentions + keepAttachments(body, refined), nil
}

type streamEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// streamResult reads claude's stream-json output up to its result event.
func streamResult(r io.Reader) (streamEvent, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var e streamEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type == "result" {
			return e, true
		}
	}
	return streamEvent{}, false
}

// refineInput is the message plus what the agent is in the middle of.
func refineInput(body string, a *Agent) string {
	var b strings.Builder
	if a != nil {
		b.WriteString("<agent>\n")
		fmt.Fprintf(&b, "name: %s\nfolder: %s\n", a.Name, a.Cwd)
		if a.Prompt != "" {
			fmt.Fprintf(&b, "current task: %s\n", clip(a.Prompt, 1200))
		}
		if r := LastReply(a.Transcript); r != "" {
			fmt.Fprintf(&b, "its last reply (end):\n%s\n", tail(r, 3000))
		}
		b.WriteString("</agent>\n\n")
	}
	fmt.Fprintf(&b, "<message>\n%s\n</message>", body)
	return b.String()
}

// refineEnv keeps the run out of tmux and out of any Claude Code session the
// input bar was started from.
func refineEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "TMUX") || (strings.HasPrefix(k, "CLAUDE") && k != "CLAUDE_CONFIG_DIR") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func splitMentions(text string) (mentions, body string) {
	text = strings.TrimSpace(text)
	mentions = mentionsRE.FindString(text)
	return mentions, strings.TrimSpace(text[len(mentions):])
}

// cleanRefined drops a code fence or quotes the model wrapped the prompt in.
func cleanRefined(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) > 6 {
		s = strings.TrimSpace(s[3 : len(s)-3])
		if first, rest, ok := strings.Cut(s, "\n"); ok && !strings.Contains(first, " ") {
			s = strings.TrimSpace(rest) // the fence's language tag
		}
	}
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `"`) {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

// keepAttachments puts back any placeholder the rewrite dropped, since a
// missing placeholder drops its attachment.
func keepAttachments(body, refined string) string {
	var missing []string
	for _, t := range attachRE.FindAllString(body, -1) {
		if !strings.Contains(refined, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return refined
	}
	return refined + "\n\n" + strings.Join(missing, " ")
}

// tail is the last n bytes of s, cut at a line start when one is close.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
		s = s[i+1:]
	}
	return s
}
