import type { AgentAction, AgentSession, Incident, Job } from "@/lib/api";
import { formatUTCTimestamp, shortId } from "@/lib/utils";

export const INCIDENT_EVENT_TYPES = [
  "incident_opened",
  "incident_status_changed",
  "agent_action_recorded",
  "approval_requested",
] as const;

const ACTIVE_STATUSES = new Set(["open", "triaging", "awaiting_approval"]);
const RESOLVED_STATUSES = new Set(["remediated", "escalated", "closed", "suppressed", "abandoned"]);

export function isActiveIncident(incident: Incident): boolean {
  return ACTIVE_STATUSES.has(incident.status);
}

export function isResolvedIncident(incident: Incident): boolean {
  return RESOLVED_STATUSES.has(incident.status);
}

export function isAwaitingApproval(incident: Incident): boolean {
  return incident.status === "awaiting_approval";
}

export function buildJobAliasMap(jobs?: Job[]): Record<string, string> {
  const aliases: Record<string, string> = {};
  jobs?.forEach((job) => {
    aliases[job.id] = job.alias || shortId(job.id);
  });
  return aliases;
}

export function jobLabel(incident: Incident, aliases: Record<string, string>): string {
  return aliases[incident.job_id] ?? shortId(incident.job_id);
}

export function formatIncidentClass(value: string): string {
  return value.replaceAll("_", " ");
}

export function incidentSummary(incident: Incident): string {
  if (incident.resolution_summary) {
    return incident.resolution_summary;
  }
  if (incident.last_error) {
    return incident.last_error;
  }
  if (incident.status === "awaiting_approval") {
    return "Human approval is pending for a tier-3 remediation.";
  }
  if (incident.status === "triaging") {
    return "Agent triage is collecting evidence.";
  }
  return `${formatIncidentClass(incident.class)} incident opened for ${incident.task_name || "the run"}.`;
}

export function formatDateTime(value?: string): string {
  if (!value) return "unknown";
  return formatUTCTimestamp(value, value);
}

export function formatDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "unknown";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.round(hours / 24)}d`;
}

export function incidentAge(incident: Incident): string {
  const start = new Date(incident.opened_at).getTime();
  const end = incident.closed_at ? new Date(incident.closed_at).getTime() : Date.now();
  return formatDurationMs(end - start);
}

export function recordFromUnknown(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

export function stringField(value: unknown, ...keys: string[]): string | undefined {
  const record = recordFromUnknown(value);
  for (const key of keys) {
    const entry = record[key];
    if (typeof entry === "string" && entry.trim() !== "") {
      return entry;
    }
  }
  return undefined;
}

export function numberField(value: unknown, ...keys: string[]): number | undefined {
  const record = recordFromUnknown(value);
  for (const key of keys) {
    const entry = record[key];
    if (typeof entry === "number" && Number.isFinite(entry)) {
      return entry;
    }
    if (typeof entry === "string") {
      const parsed = Number(entry);
      if (Number.isFinite(parsed)) {
        return parsed;
      }
    }
  }
  return undefined;
}

export function formatJson(value: unknown): string {
  if (value === undefined || value === null || value === "") return "None";
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

export function actionSummary(action: AgentAction): string {
  const target =
    stringField(action.params, "task", "task_name", "taskName") ??
    stringField(action.params, "run_id", "runId") ??
    stringField(action.params, "job_alias", "jobAlias");
  return target ? `${formatActionType(action.type)} -> ${target}` : formatActionType(action.type);
}

export function formatActionType(value: string): string {
  return value.replaceAll("_", " ");
}

export function sessionElapsed(session: AgentSession): string {
  const start = new Date(session.started_at ?? session.created_at).getTime();
  const end = session.completed_at ? new Date(session.completed_at).getTime() : Date.now();
  return formatDurationMs(end - start);
}

export function sessionProfileLabel(session: AgentSession): string {
  return session.profile_id ? shortId(session.profile_id) : "default";
}

export function resolutionMs(incident: Incident): number | null {
  if (!incident.closed_at) return null;
  const opened = new Date(incident.opened_at).getTime();
  const closed = new Date(incident.closed_at).getTime();
  return Number.isFinite(opened) && Number.isFinite(closed) && closed >= opened ? closed - opened : null;
}

export function actionHasHuman(actions: AgentAction[]): boolean {
  return actions.some((action) => action.actor === "human");
}

export function actionHasAgent(actions: AgentAction[], sessions: AgentSession[]): boolean {
  return actions.some((action) => action.actor === "agent") || sessions.length > 0;
}

export function costFromSession(session: AgentSession): number {
  const extra = recordFromUnknown(session.extra);
  return (
    numberField(extra, "cost_usd", "usd", "cost") ??
    numberField(recordFromUnknown(extra.usage), "cost_usd", "usd", "cost") ??
    0
  );
}

export function formatMoney(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "$0.00";
  return `$${value.toFixed(value < 1 ? 4 : 2)}`;
}
