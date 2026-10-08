package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"tower/internal/browser"
)

const browseHelp = `usage:
  tower browse [-d] [--render kitty|blocks] [url]   open a browser tab in a new tmux window
  tower browse --here [--render kitty|blocks] [url] the same, in this pane
  tower browse --shot <url> <out.png> [--width W --height H] [--viewport] [-t secs]
                                                    save a PNG of the page and print its path

The url defaults to http://localhost:3000 when something listens there,
else about:blank. Words that are not an address become a Google search.

keys: ctrl+l address bar, alt+left / alt+right back and forward, ctrl+r
reload, alt+= / alt+- / alt+0 zoom, ctrl+q quit. Everything else, mouse
included, goes to the page.
`

// browse runs tower browse: my installed Chrome, headless, drawn in a pane.
func browse(args []string) error {
	fs := flag.NewFlagSet("tower browse", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), browseHelp) }
	here := fs.Bool("here", false, "run the viewer in this pane")
	view := fs.Bool("view", false, "run the viewer in this pane (what the new window runs)")
	detach := fs.Bool("d", false, "open the window without switching to it")
	render := fs.String("render", "", "kitty or blocks (default: what the terminal supports)")
	shot := fs.Bool("shot", false, "save a PNG: --shot <url> <out.png>")
	width := fs.Int("width", 1280, "with --shot, viewport width in CSS pixels")
	height := fs.Int("height", 800, "with --shot, viewport height in CSS pixels")
	viewport := fs.Bool("viewport", false, "with --shot, capture the viewport instead of the whole page")
	timeout := fs.Int("t", 30, "with --shot, give up after this many seconds")
	// Flags may follow the positional arguments: --shot <url> <out> --width W.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	switch *render {
	case "", "auto", "kitty", "blocks":
	default:
		return fmt.Errorf("--render %q: want kitty or blocks", *render)
	}

	if *shot {
		if len(pos) != 2 {
			return errors.New("usage: tower browse --shot <url> <out.png> [--width W --height H]")
		}
		path, err := browser.Shot(browser.ResolveArg(pos[0]), pos[1], browser.ShotOptions{
			Width: *width, Height: *height, Viewport: *viewport, Timeout: time.Duration(*timeout) * time.Second,
		})
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}

	if len(pos) > 1 {
		return errors.New("usage: tower browse [--here] [url]")
	}
	url := ""
	if len(pos) == 1 {
		url = browser.ResolveArg(pos[0])
	}
	if url == "" {
		url = browser.Default()
	}
	if *here || *view || os.Getenv("TMUX") == "" {
		return browser.Run(url, browser.Options{Render: *render})
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := []string{exe, "browse", "--view"}
	if *render != "" {
		cmd = append(cmd, "--render", *render)
	}
	_, err = browser.Open(url, append(cmd, url), *detach)
	return err
}
