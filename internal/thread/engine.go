package thread

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Engine drives one agent process for a thread. Methods may be called from
// any goroutine. Engines report everything through StartOpts.Emit and must
// not call it synchronously from their constructor.
type Engine interface {
	Send(m Message) error
	Interrupt() error
	Answer(approvalID string, d Decision) error
	Close() error
}

// Message is one user turn: text plus image files to attach.
type Message struct {
	Text   string   `json:"text"`
	Images []string `json:"images,omitempty"`
}

// StartOpts is what an engine gets to start or resume a session.
type StartOpts struct {
	Thread string // tower thread id, exported to the agent as TOWER_THREAD
	Cwd    string
	Model  string
	Mode   string // permission mode; engines map it to their own approval settings
	Resume string // engine session id from an earlier run
	Emit   func(Event)
	Exited func(error) // the agent process ended
}

var (
	enginesMu sync.Mutex
	engines   = map[string]func(StartOpts) (Engine, error){}
)

func register(name string, start func(StartOpts) (Engine, error)) {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	engines[name] = start
}

// Engines lists the engines a thread can use.
func Engines() []string {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	var out []string
	for n := range engines {
		if n != "fake" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func startEngine(name string, o StartOpts) (Engine, error) {
	enginesMu.Lock()
	start := engines[name]
	enginesMu.Unlock()
	if start == nil {
		return nil, fmt.Errorf("unknown engine %q (have: %s)", name, strings.Join(Engines(), ", "))
	}
	return start(o)
}

// agentEnv is the environment for an agent process. It drops the tmux pane
// so the tmux hook does not mistake a headless session for a pane's agent,
// and marks the process with its thread.
func agentEnv(thread string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX_PANE=") || strings.HasPrefix(kv, "TOWER_THREAD=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TOWER_THREAD="+thread)
}
