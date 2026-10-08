package ui

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tower/internal/agent"
)

// composer is the message box shared by the input bar and the message popup:
// several lines, history, clipboard images, videos (sent as still frames),
// dropped files, and voice typing through voxtype.
type composer struct {
	ta      textarea.Model
	attach  []attachment
	seq     int    // attachments numbered per message, like Claude Code's [Image #1]
	voice   string // voxtype state while it is not idle
	busy    string
	err     string
	history []string
	hpos    int // -1 while editing a fresh message
	draft   string
}

// attachment is a file going along with a message.
type attachment struct {
	kind   string // image | video | file
	token  string // placeholder in the text where it was pasted
	path   string
	frames []string // a video's stills, which is what the agent gets to see
	stamps []string
}

type attachMsg struct {
	list []attachment
	err  error
}
type voiceMsg string

func newComposer(placeholder string) composer {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = dim
	ta.BlurredStyle.Placeholder = dim
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return accent.Render("❯ ")
		}
		return "  "
	})
	ta.Focus()
	return composer{ta: ta, history: loadHistory(), hpos: -1}
}

// Update handles one message. It reports true when I pressed enter to send.
func (c *composer) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case attachMsg:
		c.busy = ""
		if msg.err != nil {
			c.err = msg.err.Error()
		}
		for _, a := range msg.list {
			c.seq++
			a.token = fmt.Sprintf("[%s #%d]", strings.ToUpper(a.kind[:1])+a.kind[1:], c.seq)
			c.attach = append(c.attach, a)
			c.ta.InsertString(a.token + " ")
		}
		return false, nil
	case voiceMsg:
		c.voice = string(msg)
		if c.voice != "" {
			return false, pollVoice()
		}
		return false, nil
	case tea.KeyMsg:
		c.err = ""
		if msg.Paste {
			if cmd := c.dropFiles(string(msg.Runes)); cmd != nil {
				return false, cmd
			}
			break // plain text: the textarea takes it
		}
		switch msg.String() {
		case "enter":
			return true, nil
		case "ctrl+v":
			return false, c.clipboard()
		case "ctrl+r":
			return false, toggleVoice()
		case "up":
			if c.ta.Line() == 0 && len(c.history) > 0 && (c.ta.Value() == "" || c.hpos >= 0) {
				c.recall(1)
				return false, nil
			}
		case "down":
			if c.hpos >= 0 && c.ta.Line() == c.ta.LineCount()-1 {
				c.recall(-1)
				return false, nil
			}
		}
	}
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return false, cmd
}

func (c *composer) recall(d int) {
	if c.hpos < 0 {
		c.draft = c.ta.Value()
	}
	c.hpos = clamp(c.hpos+d, -1, len(c.history)-1)
	if c.hpos < 0 {
		c.ta.SetValue(c.draft)
		return
	}
	c.ta.SetValue(c.history[len(c.history)-1-c.hpos])
}

// Empty reports whether there is nothing to send.
func (c *composer) Empty() bool { return strings.TrimSpace(c.ta.Value()) == "" }

// Message is what to send. Every attachment still referenced by its
// placeholder is put where I pasted it: as file paths when inline (a Claude
// Code session in tmux reads files from paths), or left as the placeholder
// with the image files returned in order (a headless thread sends them as
// image blocks). Deleting a placeholder drops its attachment.
func (c *composer) Message(inline bool) (string, []string) {
	text := c.ta.Value()
	var images []string
	for _, a := range c.attach {
		if !strings.Contains(text, a.token) {
			continue
		}
		repl := a.token
		switch a.kind {
		case "image":
			images = append(images, a.path)
			if inline {
				repl = a.path
			}
		case "video":
			images = append(images, a.frames...)
			note := fmt.Sprintf("video %s, %d frames at %s", filepath.Base(a.path), len(a.frames), strings.Join(a.stamps, ", "))
			if inline {
				repl = "[" + note + ": " + strings.Join(a.frames, " ") + "]"
			} else {
				repl = strings.TrimSuffix(a.token, "]") + ": " + note + ", attached as images in order]"
			}
		default:
			repl = a.path
		}
		text = strings.Replace(text, a.token, repl, 1)
	}
	return strings.TrimSpace(text), images
}

// Reset clears the box after a send and remembers the text.
func (c *composer) Reset() {
	if v := strings.TrimSpace(c.ta.Value()); v != "" {
		c.history = append(c.history, v)
		saveHistory(v)
	}
	c.ta.Reset()
	c.attach, c.seq, c.hpos, c.draft, c.err = nil, 0, -1, "", ""
}

// View draws the text area at width w and height h, with @mentions and
// attachment placeholders highlighted.
func (c *composer) View(w, h int) string {
	c.ta.SetWidth(max(4, w))
	c.ta.SetHeight(max(1, h))
	return highlight(c.ta.View())
}

var tokenRE = regexp.MustCompile(`@[A-Za-z0-9._-]+|\[(?:Image|Video|File) #\d+\]`)

// highlight colours tokens inside already rendered text. The pattern holds
// no escape characters, so it only matches unbroken runs of plain text (not
// a token the cursor sits in), and it resets only bold and colour so the
// line keeps the rest of its style.
func highlight(s string) string {
	return tokenRE.ReplaceAllStringFunc(s, func(t string) string {
		return "\x1b[1;34m" + t + "\x1b[22;39m"
	})
}

// Status is the one-line summary under the box: attachments, voice, errors.
func (c *composer) Status() string {
	var parts []string
	text := c.ta.Value()
	for _, a := range c.attach {
		if !strings.Contains(text, a.token) {
			continue
		}
		label := a.token + " " + filepath.Base(a.path)
		if a.kind == "video" {
			label += fmt.Sprintf(" · %d frames", len(a.frames))
		}
		parts = append(parts, accent.Render(label))
	}
	switch c.voice {
	case "":
	case "recording":
		parts = append(parts, tone[agent.Waiting].Render("● recording, ctrl+r to stop"))
	default:
		parts = append(parts, tone[agent.Working].Render("◌ "+c.voice))
	}
	if c.busy != "" {
		parts = append(parts, tone[agent.Working].Render(c.busy))
	}
	if c.err != "" {
		parts = append(parts, tone[agent.Waiting].Render(c.err))
	}
	return strings.Join(parts, " ")
}

// clipboard attaches what is on the Wayland clipboard: an image, a video,
// copied files, or else plain text.
func (c *composer) clipboard() tea.Cmd {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		c.err = "clipboard is empty or wl-paste is missing"
		return nil
	}
	types := strings.Fields(string(out))
	pick := func(prefix string) string {
		for _, t := range types {
			if strings.HasPrefix(t, prefix) {
				return t
			}
		}
		return ""
	}
	switch {
	case pick("text/uri-list") != "":
		uris, _ := exec.Command("wl-paste", "--type", "text/uri-list").Output()
		if cmd := c.dropFiles(string(uris)); cmd != nil {
			return cmd
		}
	case pick("image/png") != "" || pick("image/") != "":
		t := pick("image/png")
		if t == "" {
			t = pick("image/")
		}
		c.busy = "saving image"
		return func() tea.Msg {
			p, err := saveClipboard(t)
			if err != nil {
				return attachMsg{err: err}
			}
			return attachMsg{list: []attachment{{kind: "image", path: p}}}
		}
	case pick("video/") != "":
		t := pick("video/")
		c.busy = "extracting frames"
		return func() tea.Msg {
			p, err := saveClipboard(t)
			if err != nil {
				return attachMsg{err: err}
			}
			a, err := videoAttachment(p)
			return attachMsg{list: []attachment{a}, err: err}
		}
	}
	text, _ := exec.Command("wl-paste", "--no-newline").Output()
	c.ta.InsertString(string(text))
	return nil
}

func saveClipboard(mime string) (string, error) {
	data, err := exec.Command("wl-paste", "--type", mime).Output()
	if err != nil || len(data) == 0 {
		return "", errors.New("could not read the clipboard")
	}
	ext := strings.TrimPrefix(mime, strings.SplitN(mime, "/", 2)[0]+"/")
	ext = strings.TrimPrefix(strings.SplitN(ext, ";", 2)[0], "x-")
	path := filepath.Join(mediaDir(), fmt.Sprintf("paste-%d.%s", time.Now().UnixNano(), ext))
	return path, os.WriteFile(path, data, 0o600)
}

func mediaDir() string {
	d := filepath.Join(agent.Dir(), "media")
	os.MkdirAll(d, 0o700)
	return d
}

// dropFiles turns pasted text that is only file paths or file:// URIs (what
// a terminal pastes for a drag and drop) into attachments. It returns nil
// when the text is ordinary text.
func (c *composer) dropFiles(text string) tea.Cmd {
	var paths []string
	for _, f := range splitPaths(text) {
		if u, err := url.Parse(f); err == nil && u.Scheme == "file" {
			f = u.Path
		}
		f = strings.Trim(f, `'"`)
		if strings.HasPrefix(f, "~/") {
			home, _ := os.UserHomeDir()
			f = filepath.Join(home, f[2:])
		}
		if st, err := os.Stat(f); err != nil || st.IsDir() {
			return nil
		}
		paths = append(paths, f)
	}
	if len(paths) == 0 {
		return nil
	}
	c.busy = "attaching"
	return func() tea.Msg {
		var list []attachment
		var errs []string
		for _, p := range paths {
			switch kindOf(p) {
			case "video":
				a, err := videoAttachment(p)
				if err != nil {
					errs = append(errs, err.Error())
					continue
				}
				list = append(list, a)
			default:
				list = append(list, attachment{kind: kindOf(p), path: p})
			}
		}
		var err error
		if len(errs) > 0 {
			err = errors.New(strings.Join(errs, "; "))
		}
		return attachMsg{list: list, err: err}
	}
}

// splitPaths splits pasted text into candidate paths: one per line, or
// separated by spaces when no line has an unescaped space.
func splitPaths(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, err := os.Stat(strings.Trim(line, `'"`)); err == nil || strings.HasPrefix(line, "file://") {
			out = append(out, line)
			continue
		}
		out = append(out, strings.Fields(strings.ReplaceAll(line, `\ `, "\x00"))...)
	}
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], "\x00", " ")
	}
	return out
}

func kindOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return "image"
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v":
		return "video"
	}
	return "file"
}

// videoAttachment pulls up to six evenly spaced frames out of a video, since
// agents read images, not video.
func videoAttachment(path string) (attachment, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return attachment{}, fmt.Errorf("ffprobe %s: %v", filepath.Base(path), err)
	}
	dur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	n := clamp(int(dur)+1, 1, 6)
	a := attachment{kind: "video", path: path}
	base := filepath.Join(mediaDir(), fmt.Sprintf("frames-%d", time.Now().UnixNano()))
	for i := range n {
		at := dur * (float64(i) + 0.5) / float64(n)
		frame := fmt.Sprintf("%s-%02d.png", base, i+1)
		err := exec.Command("ffmpeg", "-v", "error", "-ss", fmt.Sprintf("%.2f", at), "-i", path,
			"-frames:v", "1", "-vf", "scale='min(1280,iw)':-2", "-y", frame).Run()
		if err != nil {
			continue
		}
		a.frames = append(a.frames, frame)
		a.stamps = append(a.stamps, fmt.Sprintf("%.1fs", at))
	}
	if len(a.frames) == 0 {
		return attachment{}, fmt.Errorf("no frames from %s", filepath.Base(path))
	}
	return a, nil
}

// toggleVoice starts or stops voxtype. It types the transcript into the
// focused pane, which is this box.
func toggleVoice() tea.Cmd {
	return func() tea.Msg {
		if _, err := exec.LookPath("voxtype"); err != nil {
			return attachMsg{err: errors.New("voxtype is not installed")}
		}
		if err := exec.Command("voxtype", "record", "toggle").Run(); err != nil {
			return attachMsg{err: fmt.Errorf("voxtype: %v (is its daemon running?)", err)}
		}
		time.Sleep(150 * time.Millisecond)
		return voiceMsg(voxStatus())
	}
}

func pollVoice() tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return voiceMsg(voxStatus()) })
}

func voxStatus() string {
	out, _ := exec.Command("voxtype", "status").Output()
	if s := strings.TrimSpace(string(out)); s != "idle" {
		return s
	}
	return ""
}

func historyPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tower", "history")
}

// loadHistory reads the last 200 messages I sent. Each line is one message
// with its newlines escaped.
func loadHistory() []string {
	f, err := os.Open(historyPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if s, err := strconv.Unquote(sc.Text()); err == nil {
			out = append(out, s)
		}
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out
}

func saveHistory(msg string) {
	os.MkdirAll(filepath.Dir(historyPath()), 0o700)
	f, err := os.OpenFile(historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, strconv.Quote(msg))
}
