#!/bin/sh
set -eu

fail() {
  echo "triage-agent: $*" >&2
  exit 1
}

api_url="${CAESIUM_API_URL:-http://127.0.0.1:8080}"
api_url="${api_url%/}"

incident_id="${CAESIUM_INCIDENT_ID:-}"
if [ -z "$incident_id" ]; then
  incident_id="${CAESIUM_AGENT_INCIDENT_ID:-}"
fi

[ -n "$incident_id" ] || fail "CAESIUM_INCIDENT_ID is required"
[ -n "${CAESIUM_AGENT_TOKEN:-}" ] || fail "CAESIUM_AGENT_TOKEN is required"

bundle_file=""
note_file=""
action_file=""
trap 'rm -f "${bundle_file:-}" "${note_file:-}" "${action_file:-}"' EXIT

bundle_file="$(mktemp)"
note_file="$(mktemp)"
action_file="$(mktemp)"

auth_header="Authorization: Bearer ${CAESIUM_AGENT_TOKEN}"

curl -fsS \
  -H "$auth_header" \
  "${api_url}/v1/agent/incidents/${incident_id}/bundle" \
  -o "$bundle_file" || fail "bundle fetch failed"

jq -e --arg incident_id "$incident_id" '
  .incident.id == $incident_id and
  (.incident.job_id | type == "string" and length > 0) and
  (.incident.status | type == "string" and length > 0) and
  (.classification.class | type == "string" and length > 0) and
  (.failure.log_tail_scrubbed | type == "boolean") and
  (.job.alias | type == "string" and length > 0) and
  (.job.tasks | type == "array" and length > 0) and
  (.run_history | type == "array") and
  (.lineage_impact.allowed_jobs | type == "array") and
  (.lineage_impact.frozen == true) and
  (.generated_at | type == "string" and length > 0)
' "$bundle_file" >/dev/null || fail "bundle shape assertion failed"

post_json() {
  url="$1"
  payload="$2"
  out="$3"
  status="$(curl -sS -o "$out" -w "%{http_code}" \
    -X POST \
    -H "$auth_header" \
    -H "Content-Type: application/json" \
    -d "$payload" \
    "$url")" || fail "POST $url failed"
  case "$status" in
    200|201|202)
      ;;
    *)
      echo "triage-agent: unexpected status ${status} from ${url}" >&2
      cat "$out" >&2 || true
      exit 1
      ;;
  esac
}

task_name="$(jq -r '.incident.task_name // (.job.tasks[0].name // "")' "$bundle_file")"
if [ -z "$task_name" ] || [ "$task_name" = "null" ]; then
  task_name="unknown"
fi

note_payload="$(jq -nc --arg task_name "$task_name" \
  '{text: ("fake triage agent validated bundle shape for task " + $task_name)}')"
post_json "${api_url}/v1/agent/incidents/${incident_id}/notes" "$note_payload" "$note_file"
jq -e '.id and .type == "note" and .status == "executed"' "$note_file" >/dev/null ||
  fail "note response shape assertion failed"

action_payload="$(jq -nc --arg task_name "$task_name" \
  '{type: "skip_task", params: {task_name: $task_name}}')"
post_json "${api_url}/v1/agent/incidents/${incident_id}/actions" "$action_payload" "$action_file"
# skip_task is a tier-3 (approval-gated) action: it is recorded and routed to the
# approval gate, never auto-executed. A correct disposition is therefore
# "proposed" (audit-spine stub, or executor pending approval) or
# "awaiting_approval" — but never "executed"/"failed"/absent, which would signal
# a contract violation or a silent remediation error slipping past the lane.
jq -e '
  .action.id and
  .action.type == "skip_task" and
  (.disposition == "proposed" or .disposition == "awaiting_approval")
' "$action_file" >/dev/null ||
  fail "action disposition assertion failed (skip_task is tier-3; expected proposed or awaiting_approval)"

echo "triage-agent: completed scripted triage for incident ${incident_id}"
