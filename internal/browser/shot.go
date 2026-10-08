package browser

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// ShotOptions size the page for Shot.
type ShotOptions struct {
	Width, Height int           // viewport in CSS pixels
	Viewport      bool          // only what fits in the viewport, not the whole page
	Timeout       time.Duration // for the whole run
}

// maxShotHeight is about where Chrome stops drawing tall captures.
const maxShotHeight = 16384

// Shot loads url in a throwaway headless Chrome and saves a PNG of it to
// out. It returns the absolute path it wrote.
func Shot(url, out string, o ShotOptions) (string, error) {
	if o.Width <= 0 || o.Height <= 0 {
		return "", errors.New("width and height must be positive")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	deadline := time.Now().Add(o.Timeout)
	dir, temp, err := profile(true)
	if err != nil {
		return "", err
	}
	ch, err := launchChrome(dir, temp, 1)
	if err != nil {
		return "", err
	}
	var conn *cdp
	defer func() { ch.close(conn) }()
	ws, err := dialWS(ch.ws, 10*time.Second)
	if err != nil {
		return "", err
	}
	events := make(chan event, 256)
	conn = newCDP(ws, func(ev event) {
		select {
		case events <- ev:
		default: // nothing I wait for comes in floods
		}
	})
	target, err := firstPage(conn)
	if err != nil {
		return "", err
	}
	t, err := attach(conn, target)
	if err != nil {
		return "", err
	}
	if err := t.viewport(o.Width, o.Height, 1); err != nil {
		return "", err
	}
	loader, err := t.load(url)
	if err != nil {
		return "", err
	}
	waitLoaded(events, t, loader, deadline)
	png, err := t.capture(o, time.Until(deadline))
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	return abs, os.WriteFile(abs, png, 0o644)
}

// load navigates and returns the loader id that its lifecycle events carry.
func (t *tab) load(url string) (string, error) {
	var r struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := t.call(30*time.Second, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return "", err
	}
	if r.ErrorText != "" {
		return "", fmt.Errorf("%s: %s", url, r.ErrorText)
	}
	return r.LoaderID, nil
}

// waitLoaded waits for the page's load event, then up to 3 more seconds
// for the network to go quiet, so late fetches and fonts make it in.
func waitLoaded(events <-chan event, t *tab, loader string, deadline time.Time) {
	if loader == "" { // same document navigation: nothing to wait for
		return
	}
	loaded := false
	var quiet <-chan time.Time
	stop := time.NewTimer(time.Until(deadline))
	defer stop.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Method != "Page.lifecycleEvent" || ev.Session != t.session {
				continue
			}
			e := decode[struct {
				FrameID  string `json:"frameId"`
				LoaderID string `json:"loaderId"`
				Name     string `json:"name"`
			}](ev.Params)
			if e.FrameID != t.target || e.LoaderID != loader {
				continue
			}
			switch {
			case e.Name == "load" && !loaded:
				loaded = true
				quiet = time.After(3 * time.Second)
			case e.Name == "networkIdle" && loaded:
				return
			}
		case <-quiet:
			return
		case <-stop.C:
			return
		}
	}
}

// capture takes the PNG, of the whole page unless o.Viewport.
func (t *tab) capture(o ShotOptions, left time.Duration) ([]byte, error) {
	// Two animation frames, so what the load started is painted.
	t.call(5*time.Second, "Runtime.evaluate", map[string]any{
		"expression":   "new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))",
		"awaitPromise": true,
	}, nil)
	p := map[string]any{"format": "png"}
	if !o.Viewport {
		var m struct {
			CSSContentSize struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"cssContentSize"`
		}
		if err := t.call(5*time.Second, "Page.getLayoutMetrics", nil, &m); err != nil {
			return nil, err
		}
		w := math.Max(float64(o.Width), math.Ceil(m.CSSContentSize.Width))
		h := math.Min(maxShotHeight, math.Max(float64(o.Height), math.Ceil(m.CSSContentSize.Height)))
		p["captureBeyondViewport"] = true
		p["clip"] = map[string]any{"x": 0, "y": 0, "width": w, "height": h, "scale": 1}
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := t.call(max(left, 10*time.Second), "Page.captureScreenshot", p, &r); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(r.Data)
}
