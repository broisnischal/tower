---
name: tower
description: Coordinate with the other Claude Code agents running in my tmux panes through the tower CLI. Use when asked to spawn or start a parallel agent, delegate or hand off a task to another agent, ask or message another agent, check what another agent is doing, wait for one to finish, or read its last reply.
---

# tower: working with the other agents

Every Claude Code session in my tmux is tracked by tower. `tower ls` lists them;
the row marked `(you)` is this session.

| Goal | Command |
|---|---|
| See who is running and what they are doing | `tower ls` (`--json` for all fields) |
| Start a helper on a task and get its answer | `tower spawn -n NAME --wait "TASK"` |
| Start a helper that edits code alongside me | `tower spawn -n NAME -w "TASK"` (own worktree, branch `tower/NAME`) |
| Assign a task and keep working; the answer comes back to me as a message | `tower ask NAME "TASK"` |
| Ask another agent and block until it replies | `tower send --wait NAME "QUESTION"` |
| See who handed what to whom | `tower comms` |
| Look at the page I am building | `tower browse --shot http://localhost:3000 /tmp/page.png --viewport`, then read the PNG |
| Open a page for me in tmux | `tower browse -d http://localhost:3000` |
| Look at the page I am building | `tower browse --shot http://localhost:3000 /tmp/page.png --viewport`, then read the PNG |
| Open a page for me to use in tmux | `tower browse -d http://localhost:3000` |
| Hand something off without waiting | `tower send NAME "MESSAGE"` |
| Block until an agent stops working | `tower wait NAME` |
| Read its latest reply, or its screen | `tower last NAME`, `tower peek NAME` |
| Close a helper I started | `tower kill NAME` |
| Start a helper with no terminal window | `tower new -e claude\|codex -n NAME --wait "TASK"` |
| See who takes part without a pane (a voice assistant, say) | `tower peer`; message one with `tower send NAME "..."` |
| See every branch: its worktree, who works there, vs main, pushed or not, uncommitted | `tower branches` (`--fetch` to check the remote first, `--json`) |

`tower new` starts a headless thread: tower runs Claude Code or Codex itself.
Its permission prompts go to me in tower, so with `--wait` it can stop at
"waiting for approval". Tell me then; do not approve on my behalf. `send`,
`wait`, `last`, `stop` and `kill` work on threads too.

## Rules

- Flags go before the agent name: `tower send --wait api "..."`.
- Agent turns take minutes. Run `--wait` commands with a long Bash timeout
  (up to 600000 ms) or in the background, and add `-t SECONDS` to cap them.
- Two agents editing one checkout overwrite each other. Give every agent that
  edits code its own worktree (`-w`) or a disjoint set of files, and say which
  in the task.
- The other agent has none of my context. Write the task as a complete brief:
  goal, files, constraints, and what to report back.
- `send` refuses an agent that sits on a permission prompt, because the Enter
  would approve it. Tell me about it instead of passing `--force`.
- Prefer `tower ask` for anything that takes more than a minute: I keep
  working, and the answer is pasted into my prompt as
  `[reply from agent "X" to the task you gave it]` when it is done.
- A message that starts with `[task from agent "X" via tower; ...]` is a task
  from X: do it and finish with the answer; tower carries it back.
- A message that starts with `[message from agent "X"; ...]` came from agent X.
  Do what it asks, and answer with `tower send X "..."` only when the header
  says to; with `--wait` my final answer reaches it on its own.
- Close the helpers I spawned once their work is merged or reported.
