#!/usr/bin/env bash
# Resume a dead codex job from where it left off (caesium).
#
# Reads state.json for the dispatching agent, verifies the codex pid is
# actually dead, and dispatches a fresh codex `task --resume <threadId>`
# from the agent's worktree. The resumed agent picks up its thread
# context and continues — typically: finish the implementation, gofmt
# its changed files, stage, edit the plan doc, then STOP for the
# orchestrator to verify (just lint / just unit-test) and publish.
#
# When to use:
#   - Codex job died (pid not alive) with substantial in-scope work in
#     the worktree.
#   - Last phase was `implementing`.
#   - The implementation looks correct in shape; the agent just didn't
#     finish staging / the plan-doc edit.
#
# When NOT to use (fall back to re-dispatch or publish-as-is):
#   - Worktree is empty or has only 1-2 files (zero-output death) —
#     re-dispatch with a fresh prompt instead.
#   - Agent took a wrong design path (an API shape that doesn't compose
#     with its sibling streams) — resume continues the wrong design.
#   - Work is substantially complete already — go straight to Phase 4.5
#     publish (the orchestrator runs the real verify chain there anyway,
#     since codex can't run caesium's containerized tests).
#
# Usage:
#   bash .claude/skills/exec-plan-wave/codex-resume.sh <dispatching-agent-id> [--prompt "custom continue prompt"]
#
# Where <dispatching-agent-id> is the short ID from Agent({name}) —
# e.g. `a0980bb57adfa3605` (the prefix before `-<hash>` under
# ~/.claude/plugins/data/codex-openai-codex/state/agent-*).
#
# Exit codes:
#   0  resume dispatched successfully
#   2  argument error
#   3  state dir or state.json not found
#   4  state.json present but lacks resumable thread / workspace
#   5  pid still alive (refuse to resume)
#   6  codex-companion.mjs not found

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: codex-resume.sh <dispatching-agent-id> [--prompt "text"]
USAGE
  exit 2
}

agent_id="${1-}"
[ -n "${agent_id}" ] || usage
shift

prompt=$'Continue from where you left off. Finish your implementation if incomplete, then `gofmt -l -w` only the Go files you changed, stage your work via `git add -A`, edit the plan doc to tick your item and append a per-item note per the original instructions, then STOP without commit/push/PR. You are in a sandbox with no Docker and no dqlite CGO libs, so do NOT attempt `just lint` / `just unit-test` / `just integration-test` (they will fail on environment, not your code) — the orchestrator runs the real verify chain and publishes from outside the sandbox.'

while [ "${1-}" ]; do
  case "$1" in
    --prompt)
      prompt="${2:?need prompt text after --prompt}"
      shift 2
      ;;
    -h|--help)
      usage
      ;;
    *)
      echo "unknown arg: $1" >&2
      usage
      ;;
  esac
done

# Find the codex state dir for this dispatching agent. Use shell glob
# with nullglob so a no-match doesn't trip `set -e` via a failing `ls`.
shopt -s nullglob
matches=("$HOME/.claude/plugins/data/codex-openai-codex/state/agent-${agent_id}-"*)
shopt -u nullglob
state_dir="${matches[0]:-}"
if [ -z "${state_dir}" ]; then
  echo "error: no codex state dir matching agent-${agent_id}-*" >&2
  echo "       under: $HOME/.claude/plugins/data/codex-openai-codex/state/" >&2
  exit 3
fi

state_file="${state_dir}/state.json"
if [ ! -f "${state_file}" ]; then
  echo "error: no state.json at ${state_file}" >&2
  exit 3
fi

# Use the most recent job (jobs[-1]) — for an agent that's already been
# resumed once, this is the resumed job, not the original.
thread_id=$(jq -r '.jobs[-1].threadId // empty' "${state_file}")
job_pid=$(jq -r '.jobs[-1].pid // empty' "${state_file}")
job_status=$(jq -r '.jobs[-1].status // empty' "${state_file}")
job_phase=$(jq -r '.jobs[-1].phase // empty' "${state_file}")
workspace=$(jq -r '.jobs[-1].workspaceRoot // empty' "${state_file}")

if [ -z "${thread_id}" ]; then
  echo "error: no threadId in state.json — nothing to resume" >&2
  exit 4
fi

if [ -z "${workspace}" ] || [ ! -d "${workspace}" ]; then
  echo "error: workspace '${workspace}' doesn't exist on disk" >&2
  exit 4
fi

# Refuse if the pid is still alive — the agent is still working, resume
# would race the running job.
if [ -n "${job_pid}" ] && [ "${job_pid}" != "null" ] && ps -p "${job_pid}" > /dev/null 2>&1; then
  echo "error: pid ${job_pid} still alive (status=${job_status} phase=${job_phase}) — refusing to resume; let it finish or cancel first" >&2
  exit 5
fi

shopt -s nullglob
companions=("$HOME"/.claude/plugins/cache/openai-codex/codex/*/scripts/codex-companion.mjs)
shopt -u nullglob
companion=""
if [ "${#companions[@]}" -gt 0 ]; then
  # Sort by version (highest last); take the last one.
  IFS=$'\n' sorted=($(sort -V <<<"${companions[*]}"))
  unset IFS
  companion="${sorted[${#sorted[@]}-1]}"
fi
if [ -z "${companion}" ]; then
  echo "error: codex-companion.mjs not found under $HOME/.claude/plugins/cache/openai-codex/" >&2
  exit 6
fi

echo "Resuming dead codex thread:"
echo "  agent:     ${agent_id}"
echo "  thread:    ${thread_id}"
echo "  workspace: ${workspace}"
echo "  last:      status=${job_status} phase=${job_phase} pid=${job_pid:-?} (dead)"
echo "  companion: ${companion}"
echo

cd "${workspace}"
exec node "${companion}" task --background --write --resume "${thread_id}" "${prompt}"
