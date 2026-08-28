/**
 * Proposal rendering is a convention over ordinary task outputs, not a schema
 * (spec §5.6): a propose step that emits three reserved `##caesium::output`
 * keys — `proposal_kind`, `proposal_summary`, `proposal_artifact` — is
 * rendered by the Console as a proposal. This module holds the pure
 * data-shaping logic (parsing the reserved keys, decoding the referenced
 * artifact, and interpreting `proposal_summary` per kind); `ProposalPanel.tsx`
 * does the actual rendering.
 *
 * Every step here is defensive by construction: `TaskRun.output` values are
 * always plain strings (they crossed a container boundary as
 * `##caesium::output` text), so any of them can be missing, truncated, or
 * simply not what a given kind's renderer expects. Nothing in this file
 * throws on malformed input — an unrecognised or broken shape degrades to
 * the generic key/value fallback rather than crashing the panel.
 */

export const PROPOSAL_KIND_KEY = "proposal_kind";
export const PROPOSAL_SUMMARY_KEY = "proposal_summary";
export const PROPOSAL_ARTIFACT_KEY = "proposal_artifact";

/** The sentinel field name `pkg/task/output.go`'s `OutputRef.Encode` writes first. */
const OUTPUT_REF_SENTINEL = "caesiumOutputRef";

/** Cap on rendered resource rows so a huge plan cannot hang the panel. */
const MAX_RESOURCES_RENDERED = 200;

/** Known Terraform plan action buckets, in the order they should render. */
const TERRAFORM_ACTION_ORDER = ["add", "change", "destroy", "import", "replace"];

/**
 * Every numeric or boolean top-level key `reagents/internal/tf.Summary` encodes
 * that is NOT an action bucket: `changes` (Terraform's own boolean verdict),
 * `outputs` (root-module output-change count), `resources` (the address
 * list itself, not a count), and `resources_truncated` (how many addresses
 * were dropped by the server-side cap). Forward-compat treats "numeric" as
 * "action" by default, which without this explicit list renders `outputs`
 * and `resources_truncated` as bogus action badges (an "Outputs 0" badge on
 * every proposal, or a "Resources_truncated 25" badge that reads as if 25
 * resources had that action) on every real `terraform.plan.v1` summary.
 */
const SUMMARY_NON_ACTION_KEYS = new Set(["resources", "resources_truncated", "changes", "outputs"]);

export interface ActionCount {
  action: string;
  count: number;
}

export interface ResourceChange {
  address: string;
  action: string;
}

/**
 * A normalized view of `proposal_summary`, produced by a kind-specific
 * renderer (or the generic fallback). `ProposalPanel` switches on `kind` to
 * decide how to lay it out; it never inspects `proposal_kind` itself.
 */
export type ProposalSection =
  | {
      kind: "structured";
      counts: ActionCount[];
      resources: ResourceChange[];
      resourcesTruncated: boolean;
    }
  | {
      kind: "generic";
      entries: Array<{ key: string; value: string }>;
      /** Set when `proposal_summary` was present but not valid JSON. */
      parseError: boolean;
    };

/** The artifact `proposal_artifact` points at, decoded from an `##caesium::output-ref` value. */
export interface ProposalArtifact {
  /** The output key `proposal_artifact` named (e.g. `"plan"`). */
  key: string;
  path?: string;
  digest?: string;
  size?: number;
  /**
   * True when `proposal_artifact` names a key that is missing from `output`,
   * or whose value is not a well-formed encoded output-ref. The panel must
   * still render something (never a download link, never a crash) — this
   * flag is how it tells "no usable reference" from "here it is".
   */
  malformed: boolean;
}

export interface Proposal {
  kind: string;
  section: ProposalSection;
  /** Undefined only when the propose step omitted `proposal_artifact` entirely. */
  artifact?: ProposalArtifact;
}

type ProposalRenderer = (summary: unknown, summaryRaw: string, parseError: boolean) => ProposalSection;

const PROPOSAL_RENDERERS: Record<string, ProposalRenderer> = {
  "terraform.plan.v1": renderTerraformPlanV1,
};

/**
 * Parses the three reserved keys out of a task's output map into a
 * `Proposal`, or returns `undefined` when the task is not a propose step
 * (no `proposal_kind`). This is the single entry point `ProposalPanel` calls.
 */
export function parseProposal(output: Record<string, string> | undefined): Proposal | undefined {
  const kind = output?.[PROPOSAL_KIND_KEY];
  if (!kind) {
    return undefined;
  }

  const summaryRaw = output[PROPOSAL_SUMMARY_KEY] ?? "";
  const { value: summary, parseError } = safeParseJSON(output[PROPOSAL_SUMMARY_KEY]);
  const renderer = PROPOSAL_RENDERERS[kind] ?? renderGenericFallback;
  const section = renderer(summary, summaryRaw, parseError);

  const artifactKey = output[PROPOSAL_ARTIFACT_KEY];
  const artifact = artifactKey ? parseArtifact(artifactKey, output[artifactKey]) : undefined;

  return { kind, section, artifact };
}

/**
 * The `terraform.plan.v1` renderer (spec §6.4 / plan C2): `proposal_summary`
 * is assumed to be a JSON object carrying counts by action
 * (`add`/`change`/`destroy`, optionally `import`/`replace`) plus a capped
 * `resources` array of `{address, action}`. Any deviation from that shape —
 * missing counts, a non-array `resources`, malformed entries within it, or
 * `proposal_summary` failing to parse at all — falls back to the generic
 * key/value renderer rather than rendering a broken or empty table.
 */
function renderTerraformPlanV1(summary: unknown, summaryRaw: string, parseError: boolean): ProposalSection {
  if (parseError || !isPlainObject(summary)) {
    return renderGenericFallback(summary, summaryRaw, parseError);
  }

  const counts: ActionCount[] = [];
  const seenActions = new Set<string>();
  for (const action of TERRAFORM_ACTION_ORDER) {
    const value = summary[action];
    if (typeof value === "number" && Number.isFinite(value)) {
      counts.push({ action, count: value });
      seenActions.add(action);
    }
  }
  // Forward-compat: an action bucket this renderer doesn't know about yet
  // still shows up rather than silently vanishing. Known non-action fields
  // (SUMMARY_NON_ACTION_KEYS) are skipped explicitly rather than inferred
  // from "is it numeric" — `outputs` and `resources_truncated` are both
  // numbers and neither is an action.
  for (const [key, value] of Object.entries(summary)) {
    if (SUMMARY_NON_ACTION_KEYS.has(key) || seenActions.has(key)) {
      continue;
    }
    if (typeof value === "number" && Number.isFinite(value)) {
      counts.push({ action: key, count: value });
    }
  }

  const resources = normalizeResources(summary.resources);

  if (counts.length === 0 && resources.length === 0) {
    // Nothing in the shape this renderer recognizes — an empty "0 add / 0
    // change / 0 destroy" table would look like a real (if boring) plan
    // rather than "this data didn't match", so fall back instead.
    return renderGenericFallback(summary, summaryRaw, false);
  }

  return {
    kind: "structured",
    counts,
    resources: resources.slice(0, MAX_RESOURCES_RENDERED),
    resourcesTruncated: resources.length > MAX_RESOURCES_RENDERED,
  };
}

function normalizeResources(value: unknown): ResourceChange[] {
  if (!Array.isArray(value)) {
    return [];
  }

  const resources: ResourceChange[] = [];
  for (const item of value) {
    if (isPlainObject(item) && typeof item.address === "string" && typeof item.action === "string") {
      resources.push({ address: item.address, action: item.action });
    }
    // A malformed entry is skipped, not fatal to the rest of the table.
  }
  return resources;
}

/**
 * The generic key/value fallback (spec §5.6): any `proposal_kind` without a
 * registered renderer — or a registered renderer that could not make sense
 * of the payload — still renders something useful. When `proposal_summary`
 * parses as a JSON object its top-level entries are shown directly;
 * otherwise (parse failure, or JSON that isn't an object — a bare array,
 * string, number, or null) the raw text is shown as a single entry.
 */
function renderGenericFallback(summary: unknown, summaryRaw: string, parseError: boolean): ProposalSection {
  if (!parseError && isPlainObject(summary)) {
    const entries = Object.entries(summary).map(([key, value]) => ({
      key,
      value: typeof value === "string" ? value : JSON.stringify(value),
    }));
    return { kind: "generic", entries, parseError: false };
  }

  return {
    kind: "generic",
    entries: [{ key: PROPOSAL_SUMMARY_KEY, value: summaryRaw || "(empty)" }],
    parseError,
  };
}

/**
 * Decodes the artifact reference `proposal_artifact` points at. Mirrors the
 * shape `pkg/task/output.go`'s `OutputRef.Encode` writes into the output map
 * (`{"caesiumOutputRef":1,"path":...,"digest":"sha256:...","size":...}`),
 * re-implemented here rather than imported since the UI never imports Go
 * output-handling code — only `digest`/`size`/`path` are surfaced, and only
 * as text: the Console is not in the data path and never offers a download.
 */
function parseArtifact(key: string, rawValue: string | undefined): ProposalArtifact {
  if (rawValue === undefined) {
    return { key, malformed: true };
  }

  const { value: parsed, parseError } = safeParseJSON(rawValue);
  if (!parseError && isPlainObject(parsed) && OUTPUT_REF_SENTINEL in parsed) {
    const path = typeof parsed.path === "string" ? parsed.path : undefined;
    const digest = typeof parsed.digest === "string" ? parsed.digest : undefined;
    const size = typeof parsed.size === "number" && Number.isFinite(parsed.size) ? parsed.size : undefined;
    // The digest is the load-bearing field (it is what enters the identity
    // hash); a reference without one is not usable and is reported malformed
    // even though the JSON itself parsed.
    return { key, path, digest, size, malformed: !digest };
  }

  return { key, malformed: true };
}

function safeParseJSON(raw: string | undefined): { value: unknown; parseError: boolean } {
  if (raw === undefined) {
    return { value: undefined, parseError: true };
  }
  try {
    return { value: JSON.parse(raw), parseError: false };
  } catch {
    return { value: undefined, parseError: true };
  }
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Human-readable byte size for the artifact reference; `undefined` renders as an em dash. */
export function formatByteSize(size: number | undefined): string {
  if (size === undefined || !Number.isFinite(size) || size < 0) {
    return "—";
  }
  if (size < 1024) {
    return `${size} B`;
  }

  const units = ["KB", "MB", "GB", "TB"];
  let value = size;
  let unitIndex = -1;
  do {
    value /= 1024;
    unitIndex++;
  } while (value >= 1024 && unitIndex < units.length - 1);

  return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[unitIndex]}`;
}
