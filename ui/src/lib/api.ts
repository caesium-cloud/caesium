import { parseAllDocuments } from "yaml";
import { clearApiKey, withAuthHeaders } from "./auth";

export interface Job {
  id: string;
  alias: string;
  trigger_id: string;
  labels: Record<string, unknown>;
  annotations: Record<string, unknown>;
  cache_config?: CacheConfigValue;
  max_parallel_tasks?: number;
  task_timeout?: number;
  run_timeout?: number;
  paused: boolean;
  created_at: string;
  updated_at: string;
  trigger?: Trigger;
  latest_run?: JobRun;
}

export interface JobRun {
  id: string;
  job_id: string;
  backfill_id?: string;
  job_alias?: string;
  trigger_type?: string;
  trigger_alias?: string;
  status: string;
  params?: Record<string, string>;
  error?: string;
  started_at: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  cache_hits?: number;
  executed_tasks?: number;
  total_tasks?: number;
  quarantine?: boolean;
  tasks?: TaskRun[];
  callbacks?: CallbackRun[];
}

export interface CallbackRun {
  id: string;
  callback_id: string;
  status: string;
  error?: string;
  started_at: string;
  completed_at?: string;
}

export interface Backfill {
  id: string;
  job_id: string;
  status: "running" | "succeeded" | "failed" | "cancelled";
  start: string;
  end: string;
  max_concurrent: number;
  reprocess: "none" | "failed" | "all";
  total_runs: number;
  completed_runs: number;
  failed_runs: number;
  cancel_requested_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateBackfillRequest {
  start: string;
  end: string;
  max_concurrent?: number;
  reprocess?: "none" | "failed" | "all";
}

export interface TaskRun {
  id: string;
  job_run_id: string;
  task_id: string;
  atom_id: string;
  engine: string;
  image: string;
  command: string | string[];
  runtime_id?: string;
  status: string;
  node_selector?: Record<string, unknown>;
  claimed_by?: string;
  claim_expires_at?: string;
  claim_attempt?: number;
  rate_limit_retry_after?: string;
  attempt?: number;
  max_attempts?: number;
  result?: string;
  output?: Record<string, string>;
  schema_violations?: Array<{ key: string; message: string }>;
  branch_selections?: string[];
  cache_hit?: boolean;
  cache_origin_run_id?: string;
  cache_created_at?: string;
  cache_expires_at?: string;
  error?: string;
  outstanding_predecessors?: number;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface RunQueueItem {
  id: string;
  position: number;
  priority: number;
  params?: Record<string, string>;
  enqueued_at: string;
}

export interface Atom {
  id: string;
  engine: string;
  image: string;
  command: string;
  spec: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface Trigger {
  id: string;
  alias: string;
  type: string;
  configuration: string;
  created_at: string;
  updated_at: string;
}

export interface JobTask {
  id: string;
  job_id: string;
  atom_id: string;
  name: string;
  next_id?: string;
  node_selector: Record<string, unknown>;
  retries: number;
  retry_delay: number;
  retry_backoff: boolean;
  trigger_rule: string;
  cache_config?: CacheConfigValue;
  output_schema?: Record<string, unknown>;
  input_schema?: Record<string, Record<string, unknown>>;
  created_at: string;
  updated_at: string;
}

type RawJobTask = Partial<JobTask> & {
  ID?: string;
  JobID?: string;
  AtomID?: string;
  Name?: string;
  NextID?: string;
  NodeSelector?: Record<string, unknown>;
  Retries?: number;
  RetryDelay?: number;
  RetryBackoff?: boolean;
  TriggerRule?: string;
  CacheConfig?: CacheConfigValue;
  OutputSchema?: Record<string, unknown>;
  InputSchema?: Record<string, Record<string, unknown>>;
  CreatedAt?: string;
  UpdatedAt?: string;
};

export type CacheConfigValue =
  | boolean
  | {
      enabled?: boolean;
      ttl?: string;
      version?: number;
    }
  | null;

export interface CacheEntry {
  hash: string;
  task_name: string;
  result: string;
  run_id: string;
  task_run_id: string;
  created_at: string;
  expires_at?: string;
}

export interface JobCacheResponse {
  entries: CacheEntry[];
}

export interface CachePruneResponse {
  pruned: number;
}

export interface DAGNode {
  id: string;
  atom_id: string;
  type?: string;
  next_id?: string;
  successors?: string[];
  output_schema?: Record<string, unknown>;
  input_schema?: Record<string, Record<string, unknown>>;
}

export interface DAGEdge {
  from: string;
  to: string;
  contract_defined?: boolean;
}

export interface JobDAGResponse {
  job_id: string;
  nodes: DAGNode[];
  edges: DAGEdge[];
}

export interface JobStats {
  total: number;
  recent_runs: number;
  success_rate: number;
  avg_duration_seconds: number;
}

export interface FailingJob {
  job_id: string;
  alias: string;
  failure_count: number;
  last_failure?: string;
}

export interface FailingAtom {
  job_id: string;
  alias: string;
  atom_name: string;
  failure_count: number;
}

export interface SlowestJob {
  job_id: string;
  alias: string;
  avg_duration_seconds: number;
}

export interface StatsResponse {
  jobs: JobStats;
  top_failing: FailingJob[];
  top_failing_atoms: FailingAtom[];
  slowest_jobs: SlowestJob[];
  success_rate_trend: DailyStats[];
}

export interface DailyStats {
  date: string;
  run_count: number;
  success_rate: number;
}

export type IncidentStatus =
  | "open"
  | "triaging"
  | "awaiting_approval"
  | "remediated"
  | "escalated"
  | "closed"
  | "suppressed"
  | "abandoned"
  | string;

export interface Incident {
  id: string;
  namespace?: string;
  job_id: string;
  run_id?: string;
  task_id?: string;
  task_name?: string;
  class: string;
  status: IncidentStatus;
  dedupe_key: string;
  occurrence_count: number;
  attempt: number;
  backfill_id?: string;
  remediation_target_run_id?: string;
  allowed_jobs?: unknown;
  last_error?: string;
  resolution_summary?: string;
  evidence?: unknown;
  opened_at: string;
  closed_at?: string;
  created_at: string;
  updated_at: string;
}

export type AgentActionStatus = "proposed" | "approved" | "rejected" | "executed" | "failed" | string;
export type AgentActionActor = "policy" | "agent" | "human" | string;

export interface AgentAction {
  id: string;
  namespace?: string;
  incident_id: string;
  session_id?: string;
  type: string;
  params?: unknown;
  tier: number;
  status: AgentActionStatus;
  result?: unknown;
  actor: AgentActionActor;
  created_at: string;
  updated_at: string;
}

export type ApprovalDecision = "pending" | "approved" | "rejected" | "expired" | string;

export interface ApprovalRequest {
  id: string;
  namespace?: string;
  incident_id: string;
  action_id: string;
  approvers_hint?: string;
  decision: ApprovalDecision;
  decider?: string;
  reason?: string;
  expires_at?: string;
  decided_at?: string;
  created_at: string;
  updated_at: string;
}

export type AgentSessionState = "pending" | "running" | "succeeded" | "failed" | "timed_out" | "cancelled" | string;

export interface AgentSession {
  id: string;
  namespace?: string;
  incident_id: string;
  profile_id?: string;
  engine?: string;
  atom_id?: string;
  container_id?: string;
  token_id?: string;
  state: AgentSessionState;
  session_log?: string;
  actions_used: number;
  tokens_used: number;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  extra?: unknown;
}

export interface IncidentListParams {
  status?: string;
  class?: string;
  needs_approval?: boolean;
  job_id?: string;
  limit?: number;
  offset?: number;
}

export interface IncidentList {
  incidents: Incident[];
  total: number;
  limit: number;
  offset: number;
}

export interface IncidentDetail {
  incident: Incident;
  actions: AgentAction[];
  approvals: ApprovalRequest[];
  sessions: AgentSession[];
}

export interface HealthCheckResult {
  status?: string;
  latency_ms?: number;
  count?: number;
}

export interface HealthResponse {
  status: string;
  uptime: number;
  checks?: {
    database?: HealthCheckResult;
    active_runs?: HealthCheckResult;
    triggers?: HealthCheckResult;
    nodes?: HealthCheckResult;
  };
}

export interface Node {
  address: string;
  arch: string;
  workers_busy: number;
  workers_total: number;
}

export interface SystemFeatures {
  database_console_enabled: boolean;
  log_console_enabled: boolean;
  agent_remediation_enabled: boolean;
  freshness_enabled: boolean;
  external_url?: string;
}

export interface LintRequest {
  definitions: unknown[];
}

export interface LintMessage {
  message: string;
  line?: number;
}

export interface LintSummary {
  steps: string;
}

export interface LintResponse {
  errors: LintMessage[];
  warnings: LintMessage[];
  summary: LintSummary;
}

export interface DiffJobSpec {
  alias: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  trigger?: {
    type: string;
    configuration: Record<string, unknown>;
  };
  steps?: unknown[];
}

export interface DiffUpdate {
  alias: string;
  diff: string;
}

export interface DiffResponse {
  added: DiffJobSpec[];
  removed: DiffJobSpec[];
  modified: DiffUpdate[];
}

export interface ApplyJobDefResponse {
  applied: number;
}

export interface TriggerRunRequest {
  params?: Record<string, string>;
}

export interface TriggerCreateRequest {
  alias: string;
  type: string;
  configuration: Record<string, unknown>;
}

export interface TriggerUpdateRequest {
  alias?: string;
  configuration?: Record<string, unknown>;
}

export interface DatabaseSchemaColumn {
  name: string;
  data_type: string;
  nullable: boolean;
  primary_key: boolean;
  default_value?: string;
}

export interface DatabaseSchemaTable {
  name: string;
  row_count?: number;
  columns: DatabaseSchemaColumn[];
}

export interface DatabaseSchemaResponse {
  dialect: string;
  version?: string;
  read_only: boolean;
  tables: DatabaseSchemaTable[];
}

export interface DatabaseQueryRequest {
  sql: string;
  limit?: number;
}

export interface DatabaseQueryColumn {
  name: string;
  data_type: string;
}

export interface DatabaseQueryResponse {
  dialect: string;
  read_only: boolean;
  statement_type: string;
  query: string;
  limit: number;
  duration_ms: number;
  row_count: number;
  truncated: boolean;
  columns: DatabaseQueryColumn[];
  rows: unknown[][];
}

export type ApiErrorKind =
  | "unknown"
  | "authentication_required"
  | "insufficient_access"
  | "replay_missing_idempotency_key"
  | "replay_bad_request"
  | "replay_target_not_found"
  | "replay_requires_distributed_execution"
  | "replay_conflict"
  | "replay_request_too_large"
  | "replay_safe_refusal"
  | "replay_refused";

export interface FieldChange {
  field: string;
  kind: "scalar" | "map_entry" | "structural" | string;
  before?: string;
  after?: string;
  added?: boolean;
  removed?: boolean;
  redacted?: boolean;
}

export interface WhyTrigger {
  type?: string;
  alias?: string;
  params?: Record<string, string>;
  firedAt?: string;
}

export interface BlobDiff {
  hashEqual: boolean;
  subjectHash?: string;
  baselineHash?: string;
  changes?: FieldChange[];
  degraded?: string;
}

export type RunDiffVerdict = "WOULD_CACHE_HIT" | "RERAN" | "DEGRADED";

export interface RunDiffTask {
  taskName: string;
  leftTaskRunId: string;
  rightTaskRunId: string;
  leftTaskId: string;
  rightTaskId: string;
  leftStatus: string;
  rightStatus: string;
  leftAttempt: number;
  rightAttempt: number;
  leftHash?: string;
  rightHash?: string;
  verdict: RunDiffVerdict;
  hashEqual: boolean;
  changes?: FieldChange[];
  degraded?: string;
}

export interface RunDiff {
  jobId: string;
  leftRunId: string;
  rightRunId: string;
  leftStatus: string;
  rightStatus: string;
  leftTrigger: WhyTrigger;
  rightTrigger: WhyTrigger;
  triggerChanges?: FieldChange[];
  paramChanges?: FieldChange[];
  tasks: RunDiffTask[];
  tasksAdded?: string[];
  tasksRemoved?: string[];
  generatedAt: string;
}

export interface ReplayRequest {
  set?: Record<string, string>;
}

export interface ReplayResponse {
  run_id: string;
  status: string;
  quarantine: boolean;
}

export interface WhyBaseline {
  kind: string;
  runId?: string;
  taskRunId?: string;
  startedAt?: string;
}

export type WhyVerdict = "CACHE_HIT" | "CACHE_MISS" | "CACHE_DISABLED" | "UNKNOWN";

export interface WhyExplanation {
  runId: string;
  jobId: string;
  taskId: string;
  taskName: string;
  taskRunId: string;
  verdict: WhyVerdict;
  status: string;
  cacheEnabled: boolean;
  hash?: string;
  summary: string;
  trigger: WhyTrigger;
  baseline: WhyBaseline;
  diff?: BlobDiff;
}

export interface BlameOptions {
  from?: string;
  to?: string;
  task?: string;
}

export interface BlameTaskElement {
  name: string;
  image: string;
  command?: string[];
}

export interface BlameTaskAttribution {
  element: BlameTaskElement;
  introducing_commit: string;
  snapshot_id: string;
}

export interface BlameEdgeElement {
  from: string;
  to: string;
}

export interface BlameEdgeAttribution {
  element: BlameEdgeElement;
  introducing_commit: string;
  snapshot_id: string;
  provenance_commit?: string;
}

export interface BlameResult {
  job_id: string;
  coverage: string;
  from_commit?: string;
  to_commit?: string;
  tasks: BlameTaskAttribution[];
  edges: BlameEdgeAttribution[];
}

export interface ReceiptTaskEntry {
  task_name: string;
  identity_hash: string;
  image: string;
  resolved_image_digest?: string;
  digest_pinned: boolean;
  degraded: boolean;
  degraded_reason?: string;
}

export interface Receipt {
  receipt_version: number;
  run_id: string;
  job_id: string;
  job_alias?: string;
  git_commit?: string;
  manifest_content_hash?: string;
  tasks: ReceiptTaskEntry[];
  degraded: boolean;
  degraded_tasks?: string[];
  receipt_digest: string;
}

export type ReceiptDriftKind =
  | "receipt_digest_mismatch"
  | "image_digest_mismatch"
  | "identity_hash_mismatch"
  | "manifest_changed"
  | "git_commit_changed"
  | "task_missing"
  | "task_added"
  | "receipt_version_mismatch";

export interface ReceiptDrift {
  kind: ReceiptDriftKind;
  task?: string;
  expected?: string;
  actual?: string;
  detail: string;
}

export interface VerifyResult {
  run_id: string;
  match: boolean;
  degraded: boolean;
  degraded_tasks?: string[];
  expected_digest: string;
  actual_digest: string;
  drifts?: ReceiptDrift[];
  rederived: Receipt | null;
}

export type DatasetStatus =
  | "unknown"
  | "fresh"
  | "stale"
  | "stale-upstream"
  | "violated"
  | "quarantined"
  | string;

export type DatasetDeclarationDirection =
  | "produces"
  | "consumes"
  | "source"
  | string;

export type DatasetDecision =
  | "derived"
  | "skipped_fresh"
  | "skipped_upstream"
  | "skipped_admission"
  | "skipped_active_run"
  | string;

export interface DatasetState {
  id: string;
  namespace?: string;
  name: string;
  watermark: string;
  watermark_run_at?: string;
  advanced_at?: string;
  verified_at?: string;
  status: DatasetStatus;
  reason?: string;
  last_run_id?: string;
  consumed_watermarks?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface DatasetDeclaration {
  id: string;
  job_id: string;
  job_alias: string;
  step_name: string;
  namespace?: string;
  name: string;
  direction: DatasetDeclarationDirection;
  freshness?: string;
  max_staleness?: string;
  expected_every?: string;
  watermark_key?: string;
  external: boolean;
  arrival_binding?: unknown;
  created_at: string;
  updated_at: string;
}

export interface DatasetSLO {
  freshness?: string;
  max_staleness?: string;
  expected_every?: string;
}

export interface DatasetProducingJob {
  id: string;
  alias: string;
  step_name?: string;
}

export interface DatasetDerivation {
  id: string;
  namespace?: string;
  name: string;
  decision: DatasetDecision;
  reason?: string;
  consumed_watermarks?: Record<string, string>;
  run_id?: string;
  created_at: string;
}

export interface DatasetListParams {
  status?: string;
  limit?: number;
  offset?: number;
}

export interface DatasetListResponse {
  datasets: DatasetState[];
  total: number;
  limit: number;
  offset: number;
}

export interface DatasetDetail {
  state: DatasetState;
  declaration?: DatasetDeclaration;
  slo?: DatasetSLO;
  producing_job?: DatasetProducingJob;
  last_decision?: DatasetDerivation;
}

export interface DatasetDerivationsParams {
  limit?: number;
  offset?: number;
}

export interface DatasetDerivationsResponse {
  derivations: DatasetDerivation[];
  total: number;
  limit: number;
  offset: number;
}

export interface LineageImpactQuery {
  namespace: string;
  name: string;
  maxDepth?: number;
}

export interface ImpactNode {
  dataset_namespace: string;
  dataset_name: string;
  direction: string;
  producing_step?: string;
  job_id: string;
  job_alias: string;
  provenance_commit?: string;
  provenance_repo?: string;
  last_seen: string;
  depth: number;
}

export interface ImpactResult {
  root_namespace: string;
  root_name: string;
  downstream: ImpactNode[];
}

const API_BASE_URL = import.meta.env.VITE_API_BASE_URL || "/v1";

export class ApiError extends Error {
  public status: number;
  public kind: ApiErrorKind;
  constructor(status: number, message: string, kind: ApiErrorKind = "unknown") {
    super(message);
    this.status = status;
    this.kind = kind;
    this.name = "ApiError";
  }
}

type ErrorKindMapper = (status: number, message: string) => ApiErrorKind | undefined;

async function request<T>(
  endpoint: string,
  options?: RequestInit,
  errorKindForStatus?: ErrorKindMapper,
): Promise<T> {
  const url = `${API_BASE_URL}${endpoint}`;
  return requestURL<T>(url, options, errorKindForStatus);
}

async function requestURL<T>(
  url: string,
  options?: RequestInit,
  errorKindForStatus?: ErrorKindMapper,
): Promise<T> {
  const headers = withAuthHeaders({
    "Content-Type": "application/json",
    ...(options?.headers as Record<string, string>),
  });

  const response = await fetch(url, {
    credentials: "include",
    ...options,
    headers,
  });

  if (response.status === 401) {
    clearApiKey();
    throw new ApiError(401, "Authentication required", "authentication_required");
  }

  if (!response.ok) {
    const message = parseErrorMessage(await response.text());
    throw new ApiError(response.status, message, classifyApiError(response.status, message, errorKindForStatus));
  }

  if (response.status === 204) {
    return undefined as T;
  }

  const text = await response.text();
  if (!text) {
    return undefined as T;
  }

  return JSON.parse(text) as T;
}

function parseErrorMessage(text: string): string {
  if (!text) {
    return "";
  }

  try {
    const body = JSON.parse(text) as unknown;
    // Guard: JSON.parse can yield null or a non-object primitive (e.g. body "null");
    // only index into it when it is actually an object.
    if (body && typeof body === "object") {
      const obj = body as { message?: unknown; error?: unknown };
      if (typeof obj.message === "string" && obj.message.trim() !== "") {
        return obj.message.trim();
      }
      if (typeof obj.error === "string" && obj.error.trim() !== "") {
        return obj.error.trim();
      }
    }
  } catch {
    // Non-JSON error bodies are common in older handlers; keep the raw text.
  }

  return text.trim();
}

function classifyApiError(
  status: number,
  message: string,
  errorKindForStatus?: ErrorKindMapper,
): ApiErrorKind {
  if (status === 403) {
    return "insufficient_access";
  }
  return errorKindForStatus?.(status, message) ?? "unknown";
}

// The replay controller overloads several status codes across distinct service
// errors (api/rest/controller/replay/replay.go), emitting err.Error() as the body.
// Classify by status AND a stable substring of that body so the UI does not assert
// a single cause for an overloaded code (e.g. 409 is also returned for a quarantined
// or non-terminal baseline, not only "requires distributed execution").
function replayErrorKind(status: number, message: string): ApiErrorKind | undefined {
  const body = message.toLowerCase();
  switch (status) {
    case 400:
      // Missing key emits a literal message; malformed ids / body-decode emit "bad request".
      return body.includes("idempotency-key")
        ? "replay_missing_idempotency_key"
        : "replay_bad_request";
    case 404:
      return "replay_target_not_found";
    case 409:
      // ErrReplayRequiresDistributedMode vs unavailable-proof / quarantined / not-terminal baseline.
      return body.includes("distributed execution mode")
        ? "replay_requires_distributed_execution"
        : "replay_conflict";
    case 413:
      return "replay_request_too_large";
    case 422:
      // ErrReplayUnsafe vs missing/unsupported descriptor or secret-identity refusal.
      return body.includes("not replay safe")
        ? "replay_safe_refusal"
        : "replay_refused";
    default:
      return undefined;
  }
}

function queryString(params: Record<string, string | number | boolean | undefined>): string {
  const query = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined) {
      query.set(key, String(value));
    }
  });
  return query.toString();
}

function datasetPath(namespace: string | undefined, name: string): string {
  return `/datasets/${encodeURIComponent(namespace?.trim() || "_")}/${encodeURIComponent(name)}`;
}

function parseJobDefinitions(yaml: string) {
  const docs = parseAllDocuments(yaml);
  return docs
    .map((doc) => {
      if (doc.errors.length > 0) {
        throw new Error(doc.errors.map((err) => err.message).join("\n"));
      }
      return doc.toJS();
    })
    .filter((doc): doc is Record<string, unknown> => !!doc && typeof doc === "object");
}

export interface JobRunsQuery {
  limit?: number;
  offset?: number;
}

function pickDefined<T>(...values: Array<T | undefined>): T | undefined {
  return values.find((value) => value !== undefined);
}

function normalizeJobTask(raw: RawJobTask): JobTask {
  const task: JobTask = {
    id: pickDefined(raw.id, raw.ID) ?? "",
    job_id: pickDefined(raw.job_id, raw.JobID) ?? "",
    atom_id: pickDefined(raw.atom_id, raw.AtomID) ?? "",
    name: pickDefined(raw.name, raw.Name) ?? "",
    node_selector: pickDefined(raw.node_selector, raw.NodeSelector) ?? {},
    retries: pickDefined(raw.retries, raw.Retries) ?? 0,
    retry_delay: pickDefined(raw.retry_delay, raw.RetryDelay) ?? 0,
    retry_backoff: pickDefined(raw.retry_backoff, raw.RetryBackoff) ?? false,
    trigger_rule: pickDefined(raw.trigger_rule, raw.TriggerRule) ?? "all_success",
    created_at: pickDefined(raw.created_at, raw.CreatedAt) ?? "",
    updated_at: pickDefined(raw.updated_at, raw.UpdatedAt) ?? "",
  };

  const nextId = pickDefined(raw.next_id, raw.NextID);
  if (nextId !== undefined) {
    task.next_id = nextId;
  }

  const cacheConfig = pickDefined(raw.cache_config, raw.CacheConfig);
  if (cacheConfig !== undefined) {
    task.cache_config = cacheConfig;
  }

  const outputSchema = pickDefined(raw.output_schema, raw.OutputSchema);
  if (outputSchema !== undefined) {
    task.output_schema = outputSchema;
  }

  const inputSchema = pickDefined(raw.input_schema, raw.InputSchema);
  if (inputSchema !== undefined) {
    task.input_schema = inputSchema;
  }

  return task;
}

function normalizeJobTasks(raw: RawJobTask[]): JobTask[] {
  return raw.map(normalizeJobTask);
}

export const api = {
  getJobs: () => request<Job[]>("/jobs"),
  getJob: (id: string) => request<Job>(`/jobs/${id}`),
  getJobRuns: (jobId: string, query?: JobRunsQuery) => {
    const params = queryString({ limit: query?.limit, offset: query?.offset });
    const suffix = params ? `?${params}` : "";
    return request<JobRun[]>(`/jobs/${jobId}/runs${suffix}`);
  },
  getJobQueue: (jobId: string) => request<RunQueueItem[]>(`/jobs/${encodeURIComponent(jobId)}/queue`),
  getJobRun: (jobId: string, runId: string) => request<JobRun>(`/jobs/${jobId}/runs/${runId}`),
  getRunDiff: (jobId: string, left: string, right: string) =>
    request<RunDiff>(
      `/jobs/${encodeURIComponent(jobId)}/runs/diff?${queryString({ left, right })}`,
    ),
  postReplay: (jobId: string, runId: string, body: ReplayRequest, idempotencyKey: string) =>
    request<ReplayResponse>(
      `/jobs/${encodeURIComponent(jobId)}/runs/${encodeURIComponent(runId)}/replay`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: JSON.stringify(body),
      },
      replayErrorKind,
    ),
  getTaskWhy: (jobId: string, runId: string, taskName: string) =>
    request<WhyExplanation>(
      `/jobs/${encodeURIComponent(jobId)}/runs/${encodeURIComponent(runId)}/why?${queryString({ task: taskName })}`,
    ),
  getBlame: (jobId: string, options: BlameOptions = {}) => {
    const query = queryString({ from: options.from, to: options.to, task: options.task });
    return request<BlameResult>(`/jobs/${encodeURIComponent(jobId)}/blame${query ? `?${query}` : ""}`);
  },
  getReceipt: (jobId: string, runId: string) =>
    request<Receipt>(`/jobs/${encodeURIComponent(jobId)}/runs/${encodeURIComponent(runId)}/receipt`),
  postVerify: (committedReceiptBody: Receipt) =>
    request<VerifyResult>(
      `/jobs/${encodeURIComponent(committedReceiptBody.job_id)}/runs/${encodeURIComponent(committedReceiptBody.run_id)}/receipt/verify`,
      {
        method: "POST",
        body: JSON.stringify(committedReceiptBody),
      },
    ),
  getJobDAG: (jobId: string) => request<JobDAGResponse>(`/jobs/${jobId}/dag`),
  getJobTasks: async (jobId: string) => normalizeJobTasks(await request<RawJobTask[]>(`/jobs/${jobId}/tasks`)),
  getJobCache: (jobId: string) => request<JobCacheResponse>(`/jobs/${jobId}/cache`),
  deleteJobCache: (jobId: string) => request<void>(`/jobs/${jobId}/cache`, { method: "DELETE" }),
  deleteTaskCache: (jobId: string, taskName: string) =>
    request<void>(`/jobs/${jobId}/cache/${encodeURIComponent(taskName)}`, { method: "DELETE" }),
  pruneCache: () => request<CachePruneResponse>("/cache/prune", { method: "POST" }),
  getDatasets: (params: DatasetListParams = {}) => {
    const query = queryString({
      status: params.status,
      limit: params.limit,
      offset: params.offset,
    });
    return request<DatasetListResponse>(`/datasets${query ? `?${query}` : ""}`);
  },
  getDataset: (namespace: string | undefined, name: string) =>
    request<DatasetDetail>(datasetPath(namespace, name)),
  getDatasetDerivations: (
    namespace: string | undefined,
    name: string,
    params: DatasetDerivationsParams = {},
  ) => {
    const query = queryString({ limit: params.limit, offset: params.offset });
    return request<DatasetDerivationsResponse>(
      `${datasetPath(namespace, name)}/derivations${query ? `?${query}` : ""}`,
    );
  },
  getSystemNodes: () => request<Node[]>("/system/nodes"),
  getSystemFeatures: () => request<SystemFeatures>("/system/features"),
  getIncidents: (params: IncidentListParams = {}) => {
    const query = queryString({
      status: params.status,
      class: params.class,
      needs_approval: params.needs_approval,
      job_id: params.job_id,
      limit: params.limit,
      offset: params.offset,
    });
    return request<IncidentList>(`/incidents${query ? `?${query}` : ""}`);
  },
  getIncident: (id: string) =>
    request<IncidentDetail>(`/incidents/${encodeURIComponent(id)}`),
  approveIncident: (id: string, approvalId: string, reason?: string) =>
    request<ApprovalRequest>(
      `/incidents/${encodeURIComponent(id)}/approvals/${encodeURIComponent(approvalId)}/approve`,
      {
        method: "POST",
        body: JSON.stringify({ reason }),
      },
    ),
  rejectIncident: (id: string, approvalId: string, reason?: string) =>
    request<ApprovalRequest>(
      `/incidents/${encodeURIComponent(id)}/approvals/${encodeURIComponent(approvalId)}/reject`,
      {
        method: "POST",
        body: JSON.stringify({ reason }),
      },
    ),
  lintJobDef: (yaml: string) =>
    request<LintResponse>("/jobdefs/lint", {
      method: "POST",
      body: JSON.stringify({ definitions: parseJobDefinitions(yaml) }),
    }),
  diffJobDef: (yaml: string) =>
    request<DiffResponse>("/jobdefs/diff", {
      method: "POST",
      body: JSON.stringify({ definitions: parseJobDefinitions(yaml) }),
    }),
  triggerJob: (jobId: string, body?: TriggerRunRequest) =>
    request<JobRun>(`/jobs/${jobId}/run`, {
      method: "POST",
      body: body ? JSON.stringify(body) : undefined,
    }),
  pauseJob: (jobId: string) => request<Job>(`/jobs/${jobId}/pause`, { method: "PUT" }),
  unpauseJob: (jobId: string) => request<Job>(`/jobs/${jobId}/unpause`, { method: "PUT" }),
  getTriggers: () => request<Trigger[]>("/triggers"),
  getTrigger: (id: string) => request<Trigger>(`/triggers/${id}`),
  createTrigger: (body: TriggerCreateRequest) =>
    request<Trigger>("/triggers", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  updateTrigger: (id: string, body: TriggerUpdateRequest) =>
    request<Trigger>(`/triggers/${id}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
  fireTrigger: (id: string, body?: TriggerRunRequest) =>
    request<void>(`/triggers/${id}/fire`, {
      method: "POST",
      body: body ? JSON.stringify(body) : undefined,
    }),
  getAtoms: () => request<Atom[]>("/atoms"),
  getAtom: (id: string) => request<Atom>(`/atoms/${id}`),
  deleteAtom: (id: string) => request<void>(`/atoms/${id}`, { method: "DELETE" }),
  getStats: () => request<StatsResponse>("/stats"),
  getStatsSummary: (window: string = "7d") => request<StatsResponse>(`/stats/summary?window=${window}`),
  getLineageImpact: (query: LineageImpactQuery) =>
    request<ImpactResult>(
      `/lineage/impact?${queryString({
        namespace: query.namespace,
        name: query.name,
        max_depth: query.maxDepth,
      })}`,
    ),
  getHealth: () => requestURL<HealthResponse>("/health"),
  /**
   * Like `getHealth` but reads the response body on any non-401 status code.
   * The server returns HTTP 503 for degraded/incident states, which causes
   * `requestURL` (and therefore `getHealth`) to throw — making those states
   * unobservable. This variant is used by `useClusterHealth` so the sidebar
   * can display degraded and incident states rather than falling back to
   * `unknown` on every non-2xx response.
   */
  getHealthStatus: async (): Promise<HealthResponse> => {
    const headers = withAuthHeaders({ "Content-Type": "application/json" });
    const response = await fetch("/health", { credentials: "include", headers });
    if (response.status === 401) {
      clearApiKey();
      throw new ApiError(401, "Authentication required", "authentication_required");
    }
    const text = await response.text();
    if (!text) throw new ApiError(response.status, "Empty health response");
    return JSON.parse(text) as HealthResponse;
  },
  getDatabaseSchema: () => request<DatabaseSchemaResponse>("/database/schema"),
  queryDatabase: (body: DatabaseQueryRequest) =>
    request<DatabaseQueryResponse>("/database/query", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  getLogLevel: () => request<{ level: string }>("/logs/level"),
  setLogLevel: (body: { level: string }) =>
    request<{ level: string }>("/logs/level", {
      method: "PUT",
      body: JSON.stringify(body),
    }),
  applyJobDef: (yaml: string) =>
    request<ApplyJobDefResponse>("/jobdefs/apply", {
      method: "POST",
      body: JSON.stringify({ definitions: parseJobDefinitions(yaml) }),
    }),
  getBackfills: (jobId: string) =>
    request<Backfill[]>(`/jobs/${jobId}/backfills`),
  getBackfill: (jobId: string, backfillId: string) =>
    request<Backfill>(`/jobs/${jobId}/backfills/${backfillId}`),
  createBackfill: (jobId: string, body: CreateBackfillRequest) =>
    request<Backfill>(`/jobs/${jobId}/backfill`, {
      method: "POST",
      body: JSON.stringify(body),
    }),
  cancelBackfill: (jobId: string, backfillId: string) =>
    request<Backfill>(`/jobs/${jobId}/backfills/${backfillId}/cancel`, {
      method: "PUT",
    }),
};
