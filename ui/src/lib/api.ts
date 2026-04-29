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
  tasks?: TaskRun[];
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

const API_BASE_URL = import.meta.env.VITE_API_BASE_URL || "/v1";

export class ApiError extends Error {
  public status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

async function request<T>(endpoint: string, options?: RequestInit): Promise<T> {
  const url = `${API_BASE_URL}${endpoint}`;
  return requestURL<T>(url, options);
}

async function requestURL<T>(url: string, options?: RequestInit): Promise<T> {
  const headers = withAuthHeaders({
    "Content-Type": "application/json",
    ...(options?.headers as Record<string, string>),
  });

  const response = await fetch(url, {
    ...options,
    headers,
  });

  if (response.status === 401) {
    clearApiKey();
    throw new ApiError(401, "Authentication required");
  }

  if (!response.ok) {
    throw new ApiError(response.status, await response.text());
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

export const api = {
  getJobs: () => request<Job[]>("/jobs"),
  getJob: (id: string) => request<Job>(`/jobs/${id}`),
  getJobRuns: (jobId: string) => request<JobRun[]>(`/jobs/${jobId}/runs`),
  getJobRun: (jobId: string, runId: string) => request<JobRun>(`/jobs/${jobId}/runs/${runId}`),
  getJobDAG: (jobId: string) => request<JobDAGResponse>(`/jobs/${jobId}/dag`),
  getJobTasks: (jobId: string) => request<JobTask[]>(`/jobs/${jobId}/tasks`),
  getJobCache: (jobId: string) => request<JobCacheResponse>(`/jobs/${jobId}/cache`),
  deleteJobCache: (jobId: string) => request<void>(`/jobs/${jobId}/cache`, { method: "DELETE" }),
  deleteTaskCache: (jobId: string, taskName: string) =>
    request<void>(`/jobs/${jobId}/cache/${encodeURIComponent(taskName)}`, { method: "DELETE" }),
  pruneCache: () => request<CachePruneResponse>("/cache/prune", { method: "POST" }),
  getSystemNodes: () => request<Node[]>("/system/nodes"),
  getSystemFeatures: () => request<SystemFeatures>("/system/features"),
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
    const response = await fetch("/health", { headers });
    if (response.status === 401) {
      clearApiKey();
      throw new ApiError(401, "Authentication required");
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
