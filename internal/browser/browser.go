// Package browser runs my installed Chrome headless and shows a tab of it
// in a tmux pane. Chrome draws the page and streams it to me over the
// DevTools protocol (Page.startScreencast); I draw each frame with kitty
// graphics where the terminal has them and with half blocks everywhere
// else, and send my mouse and keys back as input events. It also takes
// one-off screenshots, so an agent can look at the page it is building.
package browser

import (
	"errors"
	"os"

	"tower/internal/tmux"
)

// Open starts view, the command that runs the viewer for url, in a new
// tmux window named after the URL's host. With detach the window opens
// in the background.
func Open(url string, view []string, detach bool) (pane string, err error) {
	args := []string{"new-window", "-P", "-F", "#{pane_id}", "-n", "web:" + Host(url)}
	if detach {
		args = append(args, "-d")
	}
	if cwd, err := os.Getwd(); err == nil {
		args = append(args, "-c", cwd)
	}
	// More than one word after the flags: tmux runs it without a shell,
	// so the URL needs no quoting.
	if pane = tmux.Run(append(args, view...)...); pane == "" {
		return "", errors.New("tmux new-window failed (is tmux running?)")
	}
	return pane, nil
}
