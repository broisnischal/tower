# tower

A control plane for the coding agents I run in tmux: Claude Code sessions in
their own panes, plus headless Claude Code and Codex agents that tower runs
itself. It gives me:

- a **sidebar** in every window showing every agent: a spinner while it works,
  what it is running right now, its last prompt, context and output tokens
- a **grid** of every agent at once: one tile each with its state, current
  command and its live feed
- a **live feed** per agent, read from its transcript as it is written: my
  messages, its replies, every command with its output, file edits as diffs
- **agents assigning work to each other**: `tower ask` or `D` hands a task to
  another agent and the daemon pastes the answer back into the asker; the
  sidebar shows `⇠ task from` / `⇢ waiting on` while it runs, `m` shows the
  whole timeline, and the status bar counts open tasks (`⇄ 2`)
- a **dashboard** popup with the selected agent's live screen, activity log,
  git diff and last reply, plus send, approve, interrupt, spawn and close
- **state icons** on the window tabs and a **status-bar counter**
  (`◐ waiting on me`, `● working`, `✓ done`)
- **agents talking to each other**: `@name` / `@all` in any message, forward one
  agent's last reply to others, and a CLI the agents use themselves to spawn a
  helper in its own git worktree, message it, wait for it and read its answer
- an **input bar** along the bottom of every window that writes to the agent
  in it: several lines, history, voice typing through voxtype, pasted images,
  videos (sent as still frames) and dropped files
- **prompt rewriting** in the input bar: a quick Claude run (Haiku by
  default) turns what I typed or dictated into a clear, complete prompt for
  the agent, using its current task and last reply to work out what "it" and
  "that" mean. Enter rewrites the message and sends the rewrite; `ctrl+o`
  rewrites it into the box without sending, and `ctrl+z` brings back my own
  words. Short replies, slash commands and `!` commands go as typed.
  `set -g @tower_refine review` makes enter show the rewrite first and enter
  again send it, `off` turns it off, and `@tower_refine_model` picks the model
- **auto retry** when a turn dies on an API or network error: tower waits
  5s, 10s, 20s ... (doubling, up to 5 minutes, 8 tries) and says "continue";
  a usage limit waits until it resets; a login or billing error is shown to me
  instead, since retrying cannot fix it. When Claude Code sits in its own API
  retry wait (`Retrying in 18s · attempt 4/10`) or gets no response, tower
  presses Esc and retries 5s later: Enter if Esc put my prompt back in the
  box, "continue" otherwise. `set -g @tower_retry off` turns both off;
  `@tower_retry_max` and `@tower_retry_message` change the count and text
- a **browser in tmux** (`tower browse <url>`): my installed Chrome runs
  headless and tower draws the page in a tmux window, passing clicks, typing
  and scrolling through; agents screenshot the page they build with
  `tower browse --shot`
- **headless agents** (`N` or `tower new`): Claude Code or Codex driven over
  their own protocols, shown as a chat with live tool cards, diffs, and
  allow / deny buttons for every approval

## How it works

```
Claude Code ──hook events──> tower hook ──> $XDG_RUNTIME_DIR/tower/<session>.json + .log
                                                         │
tmux list-panes ─────────────────────────────────────────┤
                                                         ▼
                  tower ui · sidebar · status · ls · send · wait · spawn
```

1. `tower hook` is registered for every Claude Code lifecycle event. It reads
   `$TMUX_PANE`, so it knows which pane the session lives in, and writes the
   session's state (working, waiting, done, idle), current tool call and
   prompt to a JSON file plus an activity log line.
2. Everything else joins those files with `tmux list-panes`. Sessions whose
   pane is gone get cleaned up, an interrupted turn is detected from the
   transcript (Claude Code fires no Stop hook on Esc), and token counts come
   from tailing the transcript JSONL.
3. Messages go in as a bracketed paste through `tmux paste-buffer`, so Claude
   Code treats them like typed input and queues them if the agent is busy.
   Images go in as file paths, which Claude Code reads.

Headless agents run under a daemon, `tower serve`, that the UI starts on
demand. It owns the agent processes, so they keep working when I close the
dashboard:

```
claude --input-format stream-json ... ──┐
                                        ├─> tower serve ── events ──> $XDG_DATA_HOME/tower/threads/<id>/
codex app-server (JSON-RPC) ────────────┘        │
                                                 └── unix socket ──> dashboard, sidebar, grid, CLI
```

- **Claude Code** runs the way the Agent SDK runs it: stream-json both ways and
  `--permission-prompt-tool stdio`, so every permission prompt comes to tower
  as a control request I answer with allow, allow for the session, or deny.
  Threads run with `CLAUDE_SUDO_AUTOARM=off`, since claude-sudo's gate would
  otherwise approve everything before tower sees it.
- **Codex** runs as `codex app-server`; command and file-change approvals come
  in as JSON-RPC requests. Codex marks each new working directory as trusted in
  `~/.codex/config.toml` the first time a thread starts there.
- Both are folded into one event stream (my message, streamed text, tool
  calls, approvals, turn results with tokens and cost) that is stored per
  thread, so a thread resumes after a restart with `--resume` or
  `thread/resume`.

## Requirements

- Linux, tmux (3.7+ for the synchronized output below) and Claude Code
  (`claude` on `$PATH`): its hooks feed tower, and headless threads and prompt
  rewriting run it
- Go 1.26 to build, and `jq` for `make hooks`
- optional, each for one feature: `codex` (headless Codex threads), `git`
  (worktrees), `wl-paste` from wl-clipboard (pasting images and files),
  `ffmpeg` and `ffprobe` (video frames), `voxtype` (voice typing), and Chrome,
  Chromium or Brave (`tower browse`)

## Install

```sh
git clone https://github.com/broisnischal/tower ~/tower
cd ~/tower
make install   # builds bin/tower, links it to ~/.local/bin/tower
make hooks     # registers the hook in ~/.claude/settings.json (backup kept)
make skill     # links skill/ to ~/.claude/skills/tower so agents know the CLI
```

Then in `tmux.conf`:

```tmux
run-shell ~/tower/tower.tmux
set -g status-right "#{E:@tower_status}..."                 # counter
set -g window-status-format " #I#{?@tower_state, #{E:@tower_icon},} "   # ✻ per agent window
set -g window-status-current-format " #I#{?@tower_state, #{E:@tower_icon},} "
```

For less flicker, tmux needs synchronized output and Claude Code its
fullscreen renderer (documented by Claude Code; needs tmux 3.7+):

```tmux
set -as terminal-features ",*:sync"
set-environment -g CLAUDE_CODE_NO_FLICKER 1
```

Each window gets its own sidebar instead of one that moves, because moving a
pane resizes the window and a resize makes Claude Code repaint everything.

## Run

Reload tmux (`tmux source-file` on my config) and start `claude` in any pane:
the hook reports the session to tower from its first event. `prefix a` opens
the sidebar, `prefix t` the dashboard and `prefix i` the input bar; `tower`
on its own opens the dashboard and `tower ls` lists the agents. After a
rebuild, `tower respawn` restarts the docked panes on the new binary and
`tower serve stop` stops the daemon, which starts again when needed.

## Keys

| tmux | |
|---|---|
| `prefix a` | focus the sidebar (opens it); again to go back |
| `prefix A` | show or hide the sidebar |
| `prefix t` | dashboard popup |
| `prefix g` | grid popup |
| `prefix j`, `1`-`9` | go to agent 1-9 (the numbers in the sidebar) |
| `prefix i` | focus the input bar (opens it); again to go back |
| `prefix I` | show or hide the input bar |

| sidebar / dashboard | |
|---|---|
| `j` `k`, wheel | move |
| click, `enter` | go to the agent (a headless thread opens its chat) |
| `1`-`9` | go to agent 1-9 |
| `D` | assign a task from this agent to another: `@agent the task` |
| `m` | agents talking: every hand-off, open tasks marked |
| `a` | next agent that is waiting on me or done |
| `v` | grid / list |
| `s` | send (start with `@name` or `@all` to redirect; sidebar opens the editor) |
| `f` | forward this agent's last reply to other agents |
| `b` | send to every agent |
| `ctrl+v` | while typing: attach the clipboard image, video or copied files |
| `ctrl+r` | while typing: start or stop voice typing (voxtype) |
| `ctrl+o` | input bar: rewrite the message into a full prompt |
| `ctrl+z` | input bar: back to my own words after a rewrite |
| `y` | approve the permission prompt the agent is waiting on |
| `x` / `X` | interrupt (Esc) / close the pane |
| `n` | new agent in a tmux window: name, task, optional worktree |
| `N` | new headless agent: Claude Code or Codex, name, task, optional worktree |
| `i` | headless thread: type to it (`esc` back to the list) |
| `y` / `A` / `d` | headless thread: allow / allow for the session / deny |
| `r` | rename |
| `tab` | dashboard view: live, screen, diff, log |
| `pgup` `pgdn` | scroll the view |
| `l` / `c` | sidebar log panel / compact rows |
| `o` | sidebar: open the dashboard |
| `?` | every key |
| `esc` | back to my pane (sidebar), close (dashboard) |
| `q` | hide the sidebar everywhere / close the dashboard |

## CLI

```sh
tower ls                                   # who is running, where, doing what
tower spawn -n tests -w "make the flaky auth test deterministic"
tower spawn -n scout --wait "list every caller of refreshToken with file:line"
tower send --wait tests "status? paste the failing assertion"
tower wait tests && tower last tests
tower peek -n 20 tests

tower ask api "add retries to the token refresh"  # from an agent: answer comes back to it
tower comms                                # who handed what to whom
tower new -e codex -w -n api "add retries to the token refresh"   # headless
tower threads
tower approve api allow
tower browse -d http://localhost:3000      # a browser tab in a new tmux window
tower browse --shot http://localhost:3000 page.png --viewport
tower serve stop                           # after rebuilding, to load the new code
```

`tower help` has the rest. An agent can be named by its name, pane id, index
from `tower ls`, or any tmux target.

## Layout

| Path | What |
|---|---|
| `main.go` | CLI commands |
| `internal/agent/hook.go` | hook event to state transitions |
| `internal/agent/state.go` | state files, `Load`, cleanup, window icons |
| `internal/agent/transcript.go` | token usage, last reply, interrupt detection |
| `internal/agent/control.go` | resolve, send, spawn, wait, worktrees |
| `internal/agent/refine.go` | rewrites input bar messages into full prompts |
| `internal/sidebar` | docked panes per window (sidebar, input bar), shown and hidden together |
| `internal/thread` | daemon, socket protocol, event model, Claude Code and Codex engines |
| `internal/ui` | Bubble Tea: dashboard, sidebar, grid, chat, input bar, composer |
| `internal/browser`, `browse.go` | Chrome over the DevTools protocol, drawn as half blocks or kitty images |
| `tower.tmux` | key bindings, tmux hooks, status formats |
| `skill/SKILL.md` | tells agents how to use tower on each other |

Adding a column or a view means adding a field in `hook.go` (or reading it
from the transcript in `transcript.go`), then drawing it in `internal/ui/view.go`.

## License

MIT, see [LICENSE](LICENSE).
