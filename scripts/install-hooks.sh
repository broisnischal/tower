#!/usr/bin/env bash
# Register `tower hook` for every Claude Code lifecycle event in my global
# settings. Safe to re-run: it skips events that already have it, and keeps
# a backup of the previous file next to it.
set -euo pipefail
bin=${1:-$HOME/.local/bin/tower}
settings=${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}
[ -s "$settings" ] || echo '{}' >"$settings"
[ -e "$settings.bak.tower" ] || cp "$settings" "$settings.bak.tower" # keep the first, pre-tower copy
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
jq --arg cmd "\"$bin\" hook" '
  reduce ("SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "StopFailure",
          "Notification", "PermissionRequest", "PreCompact", "Stop", "SessionEnd") as $e (.;
    if any(.hooks[$e][]?.hooks[]?; .command == $cmd) then .
    else .hooks[$e] += [{matcher: "*", hooks: [{type: "command", command: $cmd, timeout: 5}]}]
    end)
' "$settings" >"$tmp"
cat "$tmp" >"$settings" # write through, in case settings.json is a symlink
echo "tower hook registered in $settings (backup: $settings.bak.tower)"
