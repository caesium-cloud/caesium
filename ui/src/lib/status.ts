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
    fg: "hsl(var(--text-2))",
    bg: "hsl(var(--text-3) / 0.12)",
    border: "hsl(var(--text-3) / 0.25)",
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

/**
 * Resolve a status string (or unknown enum) to its visual treatment.
 * Falls back to a neutral grey for anything we don't recognize.
 */
export function statusMeta(status: string | null | undefined): StatusMeta {
  if (!status) return UNKNOWN;
  const key = String(status).trim().toLowerCase();
  if (key in META) return META[key as RunStatus];
  const aliased = ALIASES[key];
  if (aliased) return META[aliased];
  return UNKNOWN;
}

export const ALL_RUN_STATUSES: RunStatus[] = [
  "running",
  "succeeded",
  "failed",
  "queued",
  "paused",
  "cached",
  "skipped",
];
