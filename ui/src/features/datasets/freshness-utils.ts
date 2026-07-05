import type { DatasetDecision, DatasetState, DatasetStatus } from "@/lib/api";

export type DatasetStatusFilter =
  | "all"
  | "unknown"
  | "fresh"
  | "stale"
  | "stale-upstream"
  | "violated"
  | "quarantined";

export const DATASET_STATUS_FILTERS: Array<{ key: DatasetStatusFilter; label: string }> = [
  { key: "all", label: "All" },
  { key: "fresh", label: "Fresh" },
  { key: "stale", label: "Stale" },
  { key: "stale-upstream", label: "Stale upstream" },
  { key: "violated", label: "Violated" },
  { key: "unknown", label: "Unknown" },
  { key: "quarantined", label: "Quarantined" },
];

export interface FreshnessTone {
  label: string;
  badgeClass: string;
  dotClass: string;
  textClass: string;
  borderClass: string;
  surfaceClass: string;
  barClass: string;
  edgeColor: string;
}

const NEUTRAL_TONE: FreshnessTone = {
  label: "unknown",
  badgeClass: "border-text-4/30 bg-text-4/10 text-text-3",
  dotClass: "bg-text-4",
  textClass: "text-text-3",
  borderClass: "border-text-4/35",
  surfaceClass: "bg-text-4/10",
  barClass: "bg-text-4",
  edgeColor: "hsl(var(--text-4))",
};

const FRESH_TONE: FreshnessTone = {
  label: "fresh",
  badgeClass: "border-success/30 bg-success/10 text-success",
  dotClass: "bg-success",
  textClass: "text-success",
  borderClass: "border-success/45",
  surfaceClass: "bg-success/10",
  barClass: "bg-success",
  edgeColor: "hsl(var(--success))",
};

const WARNING_TONE: FreshnessTone = {
  label: "stale",
  badgeClass: "border-warning/35 bg-warning/10 text-warning",
  dotClass: "bg-warning animate-gold-pulse",
  textClass: "text-warning",
  borderClass: "border-warning/55",
  surfaceClass: "bg-warning/10",
  barClass: "bg-warning",
  edgeColor: "hsl(var(--warning))",
};

const DANGER_TONE: FreshnessTone = {
  label: "violated",
  badgeClass: "border-danger/35 bg-danger/10 text-danger",
  dotClass: "bg-danger",
  textClass: "text-danger",
  borderClass: "border-danger/55",
  surfaceClass: "bg-danger/10",
  barClass: "bg-danger",
  edgeColor: "hsl(var(--danger))",
};

export function datasetKey(namespace: string | undefined, name: string): string {
  return `${namespace?.trim() ?? ""}\u0000${name.trim()}`;
}

export function datasetNamespace(state: Pick<DatasetState, "namespace">): string {
  return state.namespace?.trim() ?? "";
}

export function displayNamespace(namespace: string | undefined): string {
  return namespace?.trim() || "_";
}

/**
 * Resolve a `consumed_watermarks` map key to a routable dataset target.
 *
 * The backend keys that map with `datasetParamName` (internal/freshness):
 * `"<namespace>.<name>"` when the consumed dataset carries a namespace, or the
 * bare `"<name>"` when it does not. Dataset names themselves contain dots
 * (e.g. `raw.vendor_x`, `staging.orders`), and declarations parsed from job
 * definitions never populate a namespace today — so in practice every key is a
 * bare, possibly-dotted name, and a bare-dot `namespace.name` key is not
 * unambiguously splittable back into its parts.
 *
 * We therefore resolve the consumed dataset by NAME in the default namespace
 * (`namespace: undefined`) rather than guessing a split point or reusing the
 * *selected* dataset's namespace — a consumed source can live in a different
 * namespace than the dataset consuming it, so reusing the selected namespace
 * navigates to the wrong dataset / a 404.
 */
export function consumedDatasetTarget(key: string): {
  namespace: string | undefined;
  name: string;
} {
  return { namespace: undefined, name: key.trim() };
}

export function normalizeStatusFilter(value: unknown): DatasetStatusFilter {
  return DATASET_STATUS_FILTERS.some((entry) => entry.key === value)
    ? (value as DatasetStatusFilter)
    : "all";
}

export function cleanDatasetParam(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

export function freshnessTone(status: DatasetStatus | undefined, inheritedStale = false): FreshnessTone {
  if (inheritedStale) {
    return { ...WARNING_TONE, label: "downstream stale" };
  }
  switch (status) {
    case "fresh":
      return FRESH_TONE;
    case "stale":
      return WARNING_TONE;
    case "stale-upstream":
      return { ...WARNING_TONE, label: "stale upstream" };
    case "violated":
      return DANGER_TONE;
    case "quarantined":
      return { ...DANGER_TONE, label: "quarantined" };
    case "unknown":
    default:
      return NEUTRAL_TONE;
  }
}

export function isStaleLike(status: DatasetStatus | undefined): boolean {
  return status === "stale" || status === "stale-upstream" || status === "violated" || status === "quarantined";
}

export function isDeclaredBeforeRun(status: DatasetStatus | undefined): boolean {
  return status === "unknown";
}

export function effectiveObservedAt(state: DatasetState): string | undefined {
  const candidates = [state.advanced_at, state.verified_at, state.watermark_run_at].filter(
    (value): value is string => Boolean(value),
  );
  if (candidates.length === 0) {
    return undefined;
  }
  return candidates.reduce((latest, value) => (
    new Date(value).getTime() > new Date(latest).getTime() ? value : latest
  ));
}

export function parseGoDurationMs(raw: string | undefined): number | null {
  if (!raw) {
    return null;
  }
  const trimmed = raw.trim();
  if (!trimmed) {
    return null;
  }

  const unitMs: Record<string, number> = {
    ns: 0.000001,
    us: 0.001,
    "\u00b5s": 0.001,
    ms: 1,
    s: 1000,
    m: 60_000,
    h: 3_600_000,
  };
  const pattern = /([0-9]+(?:\.[0-9]+)?)(ns|us|\u00b5s|ms|s|m|h)/g;
  let total = 0;
  let matched = "";
  for (const match of trimmed.matchAll(pattern)) {
    const amount = Number(match[1]);
    const multiplier = unitMs[match[2] ?? ""];
    if (!Number.isFinite(amount) || multiplier === undefined) {
      return null;
    }
    matched += match[0];
    total += amount * multiplier;
  }
  return matched === trimmed && total > 0 ? total : null;
}

export function formatDurationShort(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) {
    return "unknown";
  }
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) {
    return `${seconds}s`;
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 48) {
    return `${hours}h`;
  }
  return `${Math.floor(hours / 24)}d`;
}

export function stalenessPercent(state: DatasetState, slo: string | undefined, now = Date.now()): number | null {
  const observedAt = effectiveObservedAt(state);
  const sloMs = parseGoDurationMs(slo);
  if (!observedAt || !sloMs) {
    return null;
  }
  const observedMs = new Date(observedAt).getTime();
  if (!Number.isFinite(observedMs)) {
    return null;
  }
  return Math.max(0, (now - observedMs) / sloMs) * 100;
}

export function decisionLabel(decision: DatasetDecision): string {
  switch (decision) {
    case "derived":
      return "derived";
    case "skipped_fresh":
      return "skipped fresh";
    case "skipped_upstream":
      return "skipped upstream";
    case "skipped_admission":
      return "admission skip";
    case "skipped_active_run":
      return "active run";
    default:
      return decision.replaceAll("_", " ");
  }
}
