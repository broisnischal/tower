package browser

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// tab is one page target I am attached to.
type tab struct {
	c       *cdp
	target  string
	session string
	url     string
	title   string
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	OpenerID string `json:"openerId"`
}

func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// firstPage finds the page Chrome opened at start, or makes one.
func firstPage(c *cdp) (string, error) {
	ctx, cancel := timeout(10 * time.Second)
	defer cancel()
	var r struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.call(ctx, "", "Target.getTargets", nil, &r); err != nil {
		return "", err
	}
	for _, t := range r.TargetInfos {
		if t.Type == "page" {
			return t.TargetID, nil
		}
	}
	var n struct {
		TargetID string `json:"targetId"`
	}
	err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &n)
	return n.TargetID, err
}

// attach opens a flat session on a page target and turns on what I use.
func attach(c *cdp, target string) (*tab, error) {
	ctx, cancel := timeout(10 * time.Second)
	defer cancel()
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, &r); err != nil {
		return nil, err
	}
	t := &tab{c: c, target: target, session: r.SessionID}
	for _, cmd := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		// Headless pages are never focused, so carets, :focus and focus
		// events would not work without this.
		{"Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}},
	} {
		if err := c.call(ctx, t.session, cmd.method, cmd.params, nil); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *tab) call(d time.Duration, method string, params, result any) error {
	ctx, cancel := timeout(d)
	defer cancel()
	return t.c.call(ctx, t.session, method, params, result)
}

func (t *tab) send(method string, params any) { t.c.send(t.session, method, params) }

// navigate loads url and reports Chrome's network error, if any.
func (t *tab) navigate(url string) error {
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if err := t.call(30*time.Second, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return err
	}
	if r.ErrorText != "" {
		return errors.New(r.ErrorText)
	}
	return nil
}

type history struct {
	CurrentIndex int `json:"currentIndex"`
	Entries      []struct {
		ID  int    `json:"id"`
		URL string `json:"url"`
	} `json:"entries"`
}

func (t *tab) history() (history, error) {
	var h history
	err := t.call(5*time.Second, "Page.getNavigationHistory", nil, &h)
	return h, err
}

// step moves d steps through the history, if there is somewhere to go.
func (t *tab) step(d int) error {
	h, err := t.history()
	if err != nil {
		return err
	}
	i := h.CurrentIndex + d
	if i < 0 || i >= len(h.Entries) {
		return nil
	}
	return t.call(10*time.Second, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
}

// viewport sets the page's CSS size and device scale factor.
func (t *tab) viewport(w, h int, scale float64) error {
	return t.call(5*time.Second, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": scale, "mobile": false,
	}, nil)
}

// window resizes the headless window that holds the tab.
func (t *tab) window(w, h int) {
	var r struct {
		WindowID int `json:"windowId"`
	}
	ctx, cancel := timeout(5 * time.Second)
	defer cancel()
	if t.c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": t.target}, &r) != nil {
		return
	}
	t.c.send("", "Browser.setWindowBounds", map[string]any{
		"windowId": r.WindowID, "bounds": map[string]any{"width": w, "height": h, "windowState": "normal"},
	})
}

// screencast (re)starts the frame stream, scaled to fit maxW x maxH.
func (t *tab) screencast(format string, maxW, maxH int) error {
	t.call(5*time.Second, "Page.stopScreencast", nil, nil)
	p := map[string]any{"format": format, "maxWidth": maxW, "maxHeight": maxH, "everyNthFrame": 1}
	if format == "jpeg" {
		p["quality"] = 85
	}
	return t.call(5*time.Second, "Page.startScreencast", p, nil)
}

type screencastFrame struct {
	Data      string `json:"data"`
	SessionID int    `json:"sessionId"`
	Metadata  struct {
		DeviceWidth  float64 `json:"deviceWidth"`
		DeviceHeight float64 `json:"deviceHeight"`
	} `json:"metadata"`
}

// mouse dispatches a press, release or move at CSS pixel x, y.
func (t *tab) mouse(kind string, x, y float64, button string, buttons, clicks, mods int) {
	t.send("Input.dispatchMouseEvent", map[string]any{
		"type": kind, "x": x, "y": y, "modifiers": mods,
		"button": button, "buttons": buttons, "clickCount": clicks,
	})
}

// wheel scrolls by dx, dy CSS pixels at x, y.
func (t *tab) wheel(x, y, dx, dy float64, mods int) {
	t.send("Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy, "modifiers": mods,
	})
}

// key sends a key press and release; text, when set, is what it types.
func (t *tab) key(k keyDef, mods int) {
	down := map[string]any{
		"type": "rawKeyDown", "key": k.key, "code": k.code,
		"windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk, "modifiers": mods,
	}
	if k.text != "" && mods&^modShift == 0 {
		down["type"], down["text"], down["unmodifiedText"] = "keyDown", k.text, k.text
	}
	t.send("Input.dispatchKeyEvent", down)
	t.send("Input.dispatchKeyEvent", map[string]any{
		"type": "keyUp", "key": k.key, "code": k.code,
		"windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk, "modifiers": mods,
	})
}

func (t *tab) insertText(s string) { t.send("Input.insertText", map[string]any{"text": s}) }

// decode unmarshals event params, ignoring what does not fit.
func decode[T any](raw json.RawMessage) T {
	var v T
	json.Unmarshal(raw, &v)
	return v
}
