#!/usr/bin/env bash
# tmux plugin entry for tower. Load it from tmux.conf with
#   run-shell ~/tower/tower.tmux
# or through TPM. Options (set them before this line):
#   @tower_focus_key  (a)   prefix key: focus the sidebar, again to go back
#   @tower_toggle_key (A)   prefix key: show or hide the sidebar everywhere
#   @tower_popup_key  (t)   prefix key: dashboard popup
#   @tower_grid_key   (g)   prefix key: grid of every agent
#   @tower_input_key  (i)   prefix key: focus the input bar, again to go back
#   @tower_input_toggle_key (I) prefix key: show or hide the input bar
#   @tower_width      (40)  sidebar width in columns
#   @tower_input_height (5) input bar height in lines
#   @tower_claude  (claude) command new agents start with
#   @tower_notify     (on)  flash a message when a hidden agent needs me
#   @tower_refine   (auto)  input bar rewrites my message into a full prompt:
#                           auto (enter rewrites and sends), review (enter
#                           rewrites, enter again sends), off
#   @tower_refine_model (haiku)  model for the rewrite
# It defines two formats to drop into my own status line:
#   #{E:@tower_icon}    agent state icon for a window-status format
#   #{E:@tower_status}  agent counts for status-right
dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
bin="$dir/bin/tower"
[ -x "$bin" ] || bin=$(command -v tower) || {
  tmux display-message "tower: no binary, run make in $dir"
  exit 0
}

opt() { local v; v=$(tmux show -gqv "$1"); echo "${v:-$2}"; }

tmux bind-key -N "tower: focus the agents sidebar" "$(opt @tower_focus_key a)" \
  run-shell -b "'$bin' focus sidebar '#{pane_id}'"
tmux bind-key -N "tower: show or hide the agents sidebar" "$(opt @tower_toggle_key A)" \
  run-shell -b "'$bin' toggle sidebar '#{pane_id}'"
tmux bind-key -N "tower: focus the input bar" "$(opt @tower_input_key i)" \
  run-shell -b "'$bin' focus input '#{pane_id}'"
tmux bind-key -N "tower: show or hide the input bar" "$(opt @tower_input_toggle_key I)" \
  run-shell -b "'$bin' toggle input '#{pane_id}'"
tmux bind-key -N "tower: agents dashboard" "$(opt @tower_popup_key t)" \
  display-popup -E -w 92% -h 88% -T " tower " "'$bin' ui"
tmux bind-key -N "tower: grid of every agent" "$(opt @tower_grid_key g)" \
  display-popup -E -w 95% -h 90% -T " agents " "'$bin' grid"

# prefix j, then 1-9: go to that agent (the numbers the sidebar shows).
tmux bind-key -N "tower: go to agent N (then press 1-9)" "$(opt @tower_jump_key j)" switch-client -T tower
for n in 1 2 3 4 5 6 7 8 9; do
  tmux bind-key -T tower "$n" run-shell -b "'$bin' jump $n"
done

# The sidebar follows me between windows and sessions; the same call marks
# agents in the window I land on as seen.
follow="run-shell -b \"'$bin' follow '#{?hook_session,#{hook_session},#{session_id}}'\""
tmux set-hook -g 'session-window-changed[77]' "$follow"
tmux set-hook -g 'client-session-changed[77]' "$follow"
tmux set-hook -g 'after-new-window[77]' "$follow"
# tmux shares every resize among all panes; put the docked ones back.
tmux set-hook -g 'client-resized[77]' "run-shell -b \"'$bin' fit all\""
tmux set-hook -g 'pane-exited[77]' "run-shell -b \"'$bin' fit all\""

tmux set -gq @tower_status "#($bin status)"
tmux set -gq @tower_icon "#{?#{==:#{@tower_state},waiting},#[fg=red]✻,#{?#{==:#{@tower_state},working},#[fg=#D97757]✻,#{?#{==:#{@tower_state},done},#[fg=green]✻,#{?#{==:#{@tower_state},idle},#[fg=brightblack]✻,}}}}"
