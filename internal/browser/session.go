package browser

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// session is one Chrome and the tab on screen. Pages the tab opens (links
// with target=_blank, window.open) stack on top of it the way Chrome
// focuses a new tab, and when one closes I go back to the one below.
//
// handle runs on the DevTools reader goroutine, so it only sends messages
// to the UI and starts goroutines for anything that has to wait.
type session struct {
	chrome *chrome
	conn   *cdp
	ui     func(tea.Msg)
	frames *frames

	mu   sync.Mutex
	tabs []*tab
	lay  layout // what the current tab is sized and streamed for
}

// Messages from the session to the UI.
type (
	pageMsg    struct{ url, title string }
	loadingMsg bool
	historyMsg struct{ back, forward bool }
	dialogMsg  struct{ session, kind, text, def string }
	flashMsg   string
	closedMsg  struct{} // the last tab is gone
)

func (s *session) current() *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tabs) == 0 {
		return nil
	}
	return s.tabs[len(s.tabs)-1]
}

func (s *session) find(target string) *tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.target == target {
			return t
		}
	}
	return nil
}

// isCurrent reports whether a page event came from the tab on screen.
func (s *session) isCurrent(session string) (*tab, bool) {
	t := s.current()
	return t, t != nil && t.session == session
}

func (s *session) handle(ev event) {
	switch ev.Method {
	case "Page.screencastFrame":
		f := decode[screencastFrame](ev.Params)
		if _, ok := s.isCurrent(ev.Session); !ok {
			s.conn.send(ev.Session, "Page.screencastFrameAck", map[string]any{"sessionId": f.SessionID})
			return
		}
		s.frames.put(f, ev.Session)
	case "Target.targetInfoChanged":
		info := decode[struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}](ev.Params).TargetInfo
		s.info(info)
	case "Page.frameNavigated":
		f := decode[struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
				URL      string `json:"url"`
			} `json:"frame"`
		}](ev.Params).Frame
		if t, ok := s.isCurrent(ev.Session); ok && f.ParentID == "" {
			s.setURL(t, f.URL)
		}
	case "Page.navigatedWithinDocument":
		f := decode[struct {
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}](ev.Params)
		if t, ok := s.isCurrent(ev.Session); ok && f.FrameID == t.target {
			s.setURL(t, f.URL)
		}
	case "Page.frameStartedLoading", "Page.frameStoppedLoading":
		f := decode[struct {
			FrameID string `json:"frameId"`
		}](ev.Params)
		if t, ok := s.isCurrent(ev.Session); ok && f.FrameID == t.target {
			started := ev.Method == "Page.frameStartedLoading"
			s.ui(loadingMsg(started))
			if !started {
				go s.refresh(t)
			}
		}
	case "Page.domContentEventFired":
		if t, ok := s.isCurrent(ev.Session); ok {
			go s.refresh(t) // the <title> is in by now
		}
	case "Page.javascriptDialogOpening":
		d := decode[struct {
			Type, Message, DefaultPrompt string
		}](ev.Params)
		s.ui(dialogMsg{ev.Session, d.Type, d.Message, d.DefaultPrompt})
	case "Inspector.targetCrashed":
		if _, ok := s.isCurrent(ev.Session); ok {
			s.ui(flashMsg("the page crashed; ctrl+r reloads it"))
		}
	case "Target.targetCreated":
		info := decode[struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}](ev.Params).TargetInfo
		if t := s.current(); t != nil && info.Type == "page" && info.OpenerID == t.target {
			go s.open(info.TargetID)
		}
	case "Target.targetDestroyed", "Target.detachedFromTarget":
		id := decode[struct {
			TargetID string `json:"targetId"`
		}](ev.Params).TargetID
		if id != "" {
			go s.close(id)
		}
	}
}

// info updates a tab's title and URL from Target.targetInfoChanged.
func (s *session) info(i targetInfo) {
	t := s.find(i.TargetID)
	if t == nil {
		return
	}
	s.mu.Lock()
	t.url, t.title = i.URL, i.Title
	s.mu.Unlock()
	if s.current() == t {
		s.ui(pageMsg{i.URL, i.Title})
	}
}

func (s *session) setURL(t *tab, u string) {
	s.mu.Lock()
	t.url = u
	title := t.title
	s.mu.Unlock()
	s.ui(pageMsg{u, title})
	go s.refresh(t)
}

// refresh reads the tab's title and whether back and forward lead
// anywhere. Chrome sends no event when a page sets its title, and right
// after a navigation that swapped renderers the page is briefly not
// there to ask, so I try a few times.
func (s *session) refresh(t *tab) {
	for try := range 3 {
		if try > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		ctx, cancel := timeout(5 * time.Second)
		var info struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if s.conn.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": t.target}, &info) == nil {
			s.info(info.TargetInfo)
		}
		cancel()
		h, err := t.history()
		if err != nil {
			continue
		}
		if s.current() == t {
			s.ui(historyMsg{h.CurrentIndex > 0, h.CurrentIndex < len(h.Entries)-1})
		}
		return
	}
}

// open attaches to a page the current tab opened and puts it on screen.
func (s *session) open(target string) {
	t, err := attach(s.conn, target)
	if err != nil {
		s.ui(flashMsg("new tab: " + err.Error()))
		return
	}
	prev := s.current()
	s.mu.Lock()
	s.tabs = append(s.tabs, t)
	s.mu.Unlock()
	if prev != nil {
		prev.call(5*time.Second, "Page.stopScreencast", nil, nil)
	}
	s.show(t)
}

// close drops a tab whose target went away; if it was on screen, the one
// below takes its place.
func (s *session) close(target string) {
	s.mu.Lock()
	i := -1
	for j, t := range s.tabs {
		if t.target == target {
			i = j
		}
	}
	if i < 0 {
		s.mu.Unlock()
		return
	}
	top := i == len(s.tabs)-1
	s.tabs = append(s.tabs[:i], s.tabs[i+1:]...)
	n := len(s.tabs)
	s.mu.Unlock()
	switch {
	case n == 0:
		s.ui(closedMsg{})
	case top:
		s.show(s.current())
	}
}

// show sizes and streams t, the new current tab, and tells the UI.
func (s *session) show(t *tab) {
	s.mu.Lock()
	l, u, title := s.lay, t.url, t.title
	s.mu.Unlock()
	s.ui(pageMsg{u, title})
	if l.ok() {
		s.stream(t, l)
	}
	s.refresh(t)
}

// resize applies a new layout to the tab on screen.
func (s *session) resize(l layout) error {
	s.mu.Lock()
	s.lay = l
	s.mu.Unlock()
	t := s.current()
	if t == nil || !l.ok() {
		return nil
	}
	return s.stream(t, l)
}

// stream sets the viewport and (re)starts the screencast for l. The
// headless window is sized to match: Chrome paints nothing outside it, and
// a tab I switch to is otherwise left partly blank.
func (s *session) stream(t *tab, l layout) error {
	w, h := l.css()
	s.conn.send("", "Target.activateTarget", map[string]any{"targetId": t.target})
	t.window(w, h)
	if err := t.viewport(w, h, l.dsf()); err != nil {
		return err
	}
	mw, mh := l.frameMax()
	return t.screencast(l.format(), mw, mh)
}

// pause stops the frames while nobody can see them.
func (s *session) pause() {
	if t := s.current(); t != nil {
		t.call(5*time.Second, "Page.stopScreencast", nil, nil)
	}
}

// answer closes a JavaScript dialog.
func (s *session) answer(d dialogMsg, accept bool) {
	p := map[string]any{"accept": accept}
	if d.kind == "prompt" {
		p["promptText"] = d.def
	}
	s.conn.send(d.session, "Page.handleJavaScriptDialog", p)
}

// frames hands the newest screencast frame to the renderer. Frames that
// were never drawn are acked along with the drawn one, so Chrome keeps
// sending at the pace I draw and never more.
type frames struct {
	mu     sync.Mutex
	latest *screencastFrame
	last   *screencastFrame // drawn last, for redraws after a resize
	acks   []frameAck
	ready  chan struct{}
}

type frameAck struct {
	session string
	id      int
}

func newFrames() *frames { return &frames{ready: make(chan struct{}, 1)} }

func (f *frames) put(fr screencastFrame, session string) {
	f.mu.Lock()
	f.latest = &fr
	f.acks = append(f.acks, frameAck{session, fr.SessionID})
	f.mu.Unlock()
	f.wake()
}

// redraw queues the last frame again, for a new pane size.
func (f *frames) redraw() {
	f.mu.Lock()
	if f.latest == nil {
		f.latest = f.last
	}
	f.mu.Unlock()
	f.wake()
}

func (f *frames) wake() {
	select {
	case f.ready <- struct{}{}:
	default:
	}
}

func (f *frames) take() (*screencastFrame, []frameAck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr, acks := f.latest, f.acks
	f.latest, f.acks = nil, nil
	if fr != nil {
		f.last = fr
	}
	return fr, acks
}
