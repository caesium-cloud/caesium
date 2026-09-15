import type {
  KnownAgentActionStatus,
  KnownAgentSessionState,
  KnownIncidentStatus,
} from "./api";
import { AGENT_ACTION_STATUSES, AGENT_SESSION_STATES, INCIDENT_STATUSES } from "./api";

/**
 * Canonical status semantics for the operator console.
 *
 * Every status-tinted surface (badges, dots, sparkline tints, log-level chips,
 * DAG edge colors, table row borders) reads from `statusMeta(status)`.
 * Per-component color literals are a refactoring target; new code must call
 * this helper rather than hard-coding a color.
 */

export type RunStatus =
  | "running"
  | "succeeded"
  | "failed"
  | "queued"
  | "paused"
  | "cached"
  | "skipped";

/** Status sources use distinct lifecycles even where their strings overlap. */
export type StatusDomain = "run" | "incident" | "agent-action" | "agent-session";

export interface StatusMeta {
  /** Lowercase display label, e.g. "running". */
  label: string;
  /** Foreground (text) color as a CSS `hsl(...)` expression. */
  fg: string;
  /** Background tint as a CSS `hsl(...)` expression (low alpha). */
  bg: string;
  /** Border / outline color as a CSS `hsl(...)` expression. */
  border: string;
  /**
   * Animation class name (or empty string).
   * Pulse animations are scoped to active states (`running`, `paused`).
   */
  dotClass: string;
}

const META: Record<RunStatus, StatusMeta> = {
  running: {
    label: "running",
    fg: "hsl(var(--cyan-glow))",
    bg: "hsl(var(--running) / 0.14)",
    border: "hsl(var(--running) / 0.4)",
    dotClass: "animate-cyan-pulse",
  },
  succeeded: {
    label: "succeeded",
    fg: "hsl(var(--success))",
    bg: "hsl(var(--success) / 0.12)",
    border: "hsl(var(--success) / 0.3)",
    dotClass: "",
  },
  failed: {
    label: "failed",
    fg: "hsl(var(--danger))",
    bg: "hsl(var(--danger) / 0.12)",
    border: "hsl(var(--danger) / 0.35)",
    dotClass: "",
  },
  queued: {
    label: "queued",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.32)",
    dotClass: "",
  },
  paused: {
    label: "paused",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.35)",
    dotClass: "animate-gold-pulse",
  },
  cached: {
    label: "cached",
    fg: "hsl(var(--cached))",
    bg: "hsl(var(--cached) / 0.12)",
    border: "hsl(var(--cached) / 0.3)",
    dotClass: "",
  },
  skipped: {
    label: "skipped",
    fg: "hsl(var(--text-3))",
    bg: "hsl(var(--text-4) / 0.18)",
    border: "hsl(var(--text-4) / 0.3)",
    dotClass: "",
  },
};

const INCIDENT_META: Record<KnownIncidentStatus, StatusMeta> = {
  open: {
    label: "open",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.35)",
    dotClass: "",
  },
  triaging: {
    label: "triaging",
    fg: "hsl(var(--cyan-glow))",
    bg: "hsl(var(--running) / 0.14)",
    border: "hsl(var(--running) / 0.4)",
    dotClass: "",
  },
  awaiting_approval: {
    label: "awaiting approval",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.35)",
    dotClass: "",
  },
  remediated: {
    label: "remediated",
    fg: "hsl(var(--success))",
    bg: "hsl(var(--success) / 0.12)",
    border: "hsl(var(--success) / 0.3)",
    dotClass: "",
  },
  escalated: {
    label: "escalated",
    fg: "hsl(var(--danger))",
    bg: "hsl(var(--danger) / 0.12)",
    border: "hsl(var(--danger) / 0.35)",
    dotClass: "",
  },
  closed: {
    label: "closed",
    fg: "hsl(var(--text-3))",
    bg: "hsl(var(--text-4) / 0.18)",
    border: "hsl(var(--text-4) / 0.3)",
    dotClass: "",
  },
  suppressed: {
    label: "suppressed",
    fg: "hsl(var(--text-3))",
    bg: "hsl(var(--text-4) / 0.18)",
    border: "hsl(var(--text-4) / 0.3)",
    dotClass: "",
  },
  abandoned: {
    label: "abandoned",
    fg: "hsl(var(--danger))",
    bg: "hsl(var(--danger) / 0.12)",
    border: "hsl(var(--danger) / 0.35)",
    dotClass: "",
  },
};

const AGENT_ACTION_META: Record<KnownAgentActionStatus, StatusMeta> = {
  proposed: {
    label: "proposed",
    fg: "hsl(var(--cyan-glow))",
    bg: "hsl(var(--running) / 0.14)",
    border: "hsl(var(--running) / 0.4)",
    dotClass: "",
  },
  approved: {
    label: "approved",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.35)",
    dotClass: "",
  },
  rejected: {
    label: "rejected",
    fg: "hsl(var(--text-3))",
    bg: "hsl(var(--text-4) / 0.18)",
    border: "hsl(var(--text-4) / 0.3)",
    dotClass: "",
  },
  executing: {
    label: "executing",
    fg: "hsl(var(--cyan-glow))",
    bg: "hsl(var(--running) / 0.14)",
    border: "hsl(var(--running) / 0.4)",
    dotClass: "",
  },
  executed: {
    label: "executed",
    fg: "hsl(var(--success))",
    bg: "hsl(var(--success) / 0.12)",
    border: "hsl(var(--success) / 0.3)",
    dotClass: "",
  },
  failed: {
    label: "failed",
    fg: "hsl(var(--danger))",
    bg: "hsl(var(--danger) / 0.12)",
    border: "hsl(var(--danger) / 0.35)",
    dotClass: "",
  },
};

const AGENT_SESSION_META: Record<KnownAgentSessionState, StatusMeta> = {
  pending: {
    label: "pending",
    fg: "hsl(var(--gold))",
    bg: "hsl(var(--gold) / 0.12)",
    border: "hsl(var(--gold) / 0.32)",
    dotClass: "",
  },
  running: META.running,
  succeeded: META.succeeded,
  failed: META.failed,
  timed_out: {
    label: "timed out",
    fg: "hsl(var(--danger))",
    bg: "hsl(var(--danger) / 0.12)",
    border: "hsl(var(--danger) / 0.35)",
    dotClass: "",
  },
  cancelled: {
    label: "cancelled",
    fg: "hsl(var(--text-3))",
    bg: "hsl(var(--text-4) / 0.18)",
    border: "hsl(var(--text-4) / 0.3)",
    dotClass: "",
  },
};

const DOMAIN_META = {
  incident: INCIDENT_META,
  "agent-action": AGENT_ACTION_META,
  "agent-session": AGENT_SESSION_META,
} as const;

const UNKNOWN: StatusMeta = {
  label: "unknown",
  fg: "hsl(var(--text-3))",
  bg: "hsl(var(--text-4) / 0.18)",
  border: "hsl(var(--text-4) / 0.3)",
  dotClass: "",
};

const ALIASES: Record<string, RunStatus> = {
  success: "succeeded",
  succeeded: "succeeded",
  ok: "succeeded",
  fail: "failed",
  failed: "failed",
  error: "failed",
  errored: "failed",
  cancelled: "failed",
  canceled: "failed",
  pending: "queued",
  waiting: "queued",
  scheduled: "queued",
  active: "running",
  in_progress: "running",
  running: "running",
  paused: "paused",
  cached: "cached",
  hit: "cached",
  skipped: "skipped",
  skip: "skipped",
  queued: "queued",
};

/** A domain-scoped status lookup with a nullable unrecognized key. */
export interface StatusResolution {
  /** Null only when the source value is not part of this lifecycle. */
  key: string | null;
  meta: StatusMeta;
}

/**
 * Resolve a status string (or unknown enum) to its visual treatment.
 * Falls back to a neutral grey for anything we don't recognize.
 */
export function statusMeta(status: string | null | undefined): StatusMeta {
  return resolveStatusForDomain(status, "run").meta;
}

/**
 * Resolve a status in its source lifecycle once. Domains intentionally prevent
 * identical strings from borrowing a different lifecycle's meaning.
 */
export function resolveStatusForDomain(
  status: string | null | undefined,
  domain: StatusDomain = "run",
): StatusResolution {
  const key = statusKeyForDomain(status, domain);
  if (key === null) return { key: null, meta: UNKNOWN };
  if (domain === "run") return { key, meta: META[key as RunStatus] };

  const domainMeta = hasOwn(DOMAIN_META, domain) ? DOMAIN_META[domain] : undefined;
  return { key, meta: domainMeta && hasOwn(domainMeta, key) ? domainMeta[key] : UNKNOWN };
}

/** Resolve a lifecycle status to its visual treatment. */
export function statusMetaForDomain(
  status: string | null | undefined,
  domain: StatusDomain = "run",
): StatusMeta {
  return resolveStatusForDomain(status, domain).meta;
}

/**
 * The canonical machine-readable key for a recognized status. Unrecognized
 * values return null so a real future enum named "unknown" cannot collide
 * with the fallback sentinel.
 */
export function statusKeyForDomain(
  status: string | null | undefined,
  domain: StatusDomain = "run",
): string | null {
  if (!status) return null;
  const key = String(status).trim().toLowerCase();
  if (domain === "run") {
    if (hasOwn(META, key)) return key;
    return hasOwn(ALIASES, key) ? ALIASES[key] : null;
  }

  const domainMeta = hasOwn(DOMAIN_META, domain) ? DOMAIN_META[domain] : undefined;
  return domainMeta && hasOwn(domainMeta, key) ? key : null;
}

function hasOwn<T extends object>(record: T, key: string): key is keyof T & string {
  return Object.prototype.hasOwnProperty.call(record, key);
}

export const ALL_RUN_STATUSES = [
  "running",
  "succeeded",
  "failed",
  "queued",
  "paused",
  "cached",
  "skipped",
] as const satisfies readonly RunStatus[];

export const ALL_INCIDENT_STATUSES = INCIDENT_STATUSES;
export const ALL_AGENT_ACTION_STATUSES = AGENT_ACTION_STATUSES;
export const ALL_AGENT_SESSION_STATUSES = AGENT_SESSION_STATES;
