package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A peer takes part in tower's messages without a tmux pane: a voice
// assistant, say. Tower reaches it by running its command with the message
// as the last argument, and it answers with tower send --from <name>.
// Peers live in the runtime dir, so each registers itself when it starts.
type Peer struct {
	Name  string   `json:"name"`
	Cmd   []string `json:"cmd"`
	Added int64    `json:"added"`
}

func peerPath(name string) string { return filepath.Join(Dir(), "peers", filepath.Base(name)) }

// AddPeer registers a peer, replacing one of the same name.
func AddPeer(name string, cmd []string) error {
	if name == "" || name == "me" || name == "all" || strings.ContainsAny(name, "/ \t") || len(cmd) == 0 {
		return fmt.Errorf("usage: tower peer add <name> <command...> (name without spaces, not me or all)")
	}
	os.MkdirAll(filepath.Dir(peerPath(name)), 0o700)
	writeAtomic(peerPath(name), Peer{Name: name, Cmd: cmd, Added: time.Now().Unix()})
	return nil
}

// RemovePeer forgets a peer.
func RemovePeer(name string) error { return os.Remove(peerPath(name)) }

// FindPeer looks a peer up by name.
func FindPeer(name string) (Peer, bool) {
	var p Peer
	b, err := os.ReadFile(peerPath(name))
	return p, err == nil && json.Unmarshal(b, &p) == nil && len(p.Cmd) > 0
}

// Peers lists the registered peers by name.
func Peers() []Peer {
	files, _ := filepath.Glob(filepath.Join(Dir(), "peers", "*"))
	var out []Peer
	for _, f := range files {
		if p, ok := FindPeer(filepath.Base(f)); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Send hands the peer a message.
func (p Peer) Send(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.Cmd[0], append(p.Cmd[1:], text)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("peer %s: %v %s", p.Name, err, clip(string(out), 120))
	}
	return nil
}
