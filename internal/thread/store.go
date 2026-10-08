package thread

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DataDir holds one directory per thread: thread.json and events.jsonl. It
// is persistent, unlike the tmux agent state, so threads survive a reboot.
func DataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "tower", "threads")
}

func threadDir(id string) string { return filepath.Join(DataDir(), filepath.Base(id)) }

func saveThread(t Thread) error {
	dir := threadDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("thread.json.%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "thread.json"))
}

func appendEvent(id string, e Event) error {
	f, err := os.OpenFile(filepath.Join(threadDir(id), "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func history(id string) ([]Event, error) {
	f, err := os.Open(filepath.Join(threadDir(id), "events.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func loadThreads() []Thread {
	dirs, _ := os.ReadDir(DataDir())
	var out []Thread
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(DataDir(), d.Name(), "thread.json"))
		if err != nil {
			continue
		}
		var t Thread
		if json.Unmarshal(b, &t) == nil && t.ID != "" {
			out = append(out, t)
		}
	}
	return out
}

func removeThread(id string) error { return os.RemoveAll(threadDir(id)) }
