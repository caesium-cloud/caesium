import { parseAllDocuments } from "yaml";

export interface Job {
  id: string;
  alias: string;
  trigger_id: string;
  labels: Record<string, unknown>;
  annotations: Record<string, unknown>;
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
  next_id?: string;
  node_selector: Record<string, unknown>;
  retries: number;
  retry_delay: number;
  retry_backoff: boolean;
  trigger_rule: string;
  output_schema?: Record<string, unknown>;
  input_schema?: Record<string, Record<string, unknown>>;
  created_at: string;
  updated_at: string;
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

export interface SlowestJob {
  job_id: string;
  alias: string;
  avg_duration_seconds: number;
}

export interface StatsResponse {
  jobs: JobStats;
  top_failing: FailingJob[];
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
  };
}

export interface ApplyJobDefResponse {
  applied: number;
}

export interface TriggerRunRequest {
  params?: Record<string, string>;
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
  const response = await fetch(url, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      ...options?.headers,
    },
  });

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
  triggerJob: (jobId: string, body?: TriggerRunRequest) =>
    request<JobRun>(`/jobs/${jobId}/run`, {
      method: "POST",
      body: body ? JSON.stringify(body) : undefined,
    }),
  pauseJob: (jobId: string) => request<Job>(`/jobs/${jobId}/pause`, { method: "PUT" }),
  unpauseJob: (jobId: string) => request<Job>(`/jobs/${jobId}/unpause`, { method: "PUT" }),
  getTriggers: () => request<Trigger[]>("/triggers"),
  getTrigger: (id: string) => request<Trigger>(`/triggers/${id}`),
  fireTrigger: (id: string) => request<void>(`/triggers/${id}`, { method: "PUT" }),
  getAtoms: () => request<Atom[]>("/atoms"),
  getAtom: (id: string) => request<Atom>(`/atoms/${id}`),
  deleteAtom: (id: string) => request<void>(`/atoms/${id}`, { method: "DELETE" }),
  getStats: () => request<StatsResponse>("/stats"),
  getHealth: () => requestURL<HealthResponse>("/health"),
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
