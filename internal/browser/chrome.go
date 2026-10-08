package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Chrome I run is my installed one, headless, with a profile of its own
// under $XDG_RUNTIME_DIR/tower/browser, so it never meets my real profiles
// or windows.

// chromeNames are tried in order on $PATH.
var chromeNames = []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "brave", "brave-browser"}

type chrome struct {
	cmd  *exec.Cmd
	ws   string // browser DevTools endpoint
	dir  string // --user-data-dir
	temp bool   // remove dir when Chrome exits
	exit chan struct{}
}

func findChrome() (string, error) {
	for _, n := range chromeNames {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome found (tried %s)", strings.Join(chromeNames, ", "))
}

// profileRoot holds the profiles: default, plus throwaway ones.
func profileRoot() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "tower", "browser")
}

// profile picks the shared default profile, or a throwaway one when
// another Chrome holds it (Chrome would hand my launch to that process).
func profile(throwaway bool) (dir string, temp bool, err error) {
	root := profileRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", false, err
	}
	def := filepath.Join(root, "default")
	if !throwaway && !profileLocked(def) {
		return def, false, nil
	}
	dir, err = os.MkdirTemp(root, "tab-")
	return dir, true, err
}

// profileLocked reports whether a live Chrome owns dir. Chrome keeps a
// SingletonLock symlink pointing at "<host>-<pid>".
func profileLocked(dir string) bool {
	link, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(link, '-')
	pid, err := strconv.Atoi(link[i+1:])
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

var devtoolsLine = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

// devtoolsURL finds the browser endpoint in a line of Chrome's stderr.
func devtoolsURL(line string) (string, bool) {
	m := devtoolsLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// launchChrome starts Chrome headless and waits for its DevTools endpoint.
// scale is the screen's device scale factor: screencast frames come at
// that many pixels per CSS pixel, whatever the page emulates.
func launchChrome(dir string, temp bool, scale float64) (*chrome, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--mute-audio",
	}
	if scale > 1 {
		args = append(args, "--force-device-scale-factor="+strconv.FormatFloat(scale, 'f', -1, 64))
	}
	cmd := exec.Command(bin, append(args, "about:blank")...)
	// Its own process group, so I can take down the renderers with it, and
	// SIGKILL if I die without cleaning up.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &chrome{cmd: cmd, dir: dir, temp: temp, exit: make(chan struct{})}
	found := make(chan string, 1)
	go c.drain(stderr, found)
	go func() { cmd.Wait(); close(c.exit) }()
	select {
	case c.ws = <-found:
		return c, nil
	case <-c.exit:
		c.cleanup()
		return nil, fmt.Errorf("%s exited before DevTools came up", filepath.Base(bin))
	case <-time.After(20 * time.Second):
		c.kill()
		return nil, fmt.Errorf("%s: no DevTools endpoint after 20s", filepath.Base(bin))
	}
}

// drain reads Chrome's stderr: it reports the endpoint once, then throws
// the rest away so Chrome never blocks on a full pipe.
func (c *chrome) drain(r io.Reader, found chan<- string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if u, ok := devtoolsURL(sc.Text()); ok {
			found <- u
			break
		}
	}
	io.Copy(io.Discard, r)
}

// close asks Chrome to quit through DevTools, so the profile is flushed,
// then makes sure every process in its group is gone.
func (c *chrome) close(conn *cdp) {
	if conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn.call(ctx, "", "Browser.close", nil, nil)
		cancel()
		conn.close()
	}
	select {
	case <-c.exit:
	case <-time.After(3 * time.Second):
	}
	c.kill()
}

// kill signals the whole process group, then cleans up.
func (c *chrome) kill() {
	pid := c.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-c.exit:
	case <-time.After(2 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-c.exit
	}
	// Helpers that outlive the main process still sit in its group.
	syscall.Kill(-pid, syscall.SIGKILL)
	c.cleanup()
}

func (c *chrome) cleanup() {
	if c.temp {
		os.RemoveAll(c.dir)
	}
}
