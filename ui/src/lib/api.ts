export interface Job {
  id: string;
  alias: string;
  trigger_id: string;
  labels: Record<string, unknown>;
  annotations: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  latest_run?: JobRun;
  // Provenance / GitOps fields
  source_id?: string;
  repo?: string;
  ref?: string;
  commit?: string;
  path?: string;
  max_parallel_tasks?: number;
  task_timeout?: number;
}

export interface JobRun {
  id: string;
  job_id: string;
  job_alias?: string;
  trigger_type?: string;
  trigger_alias?: string;
  status: string;
  error?: string;
  started_at: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  tasks?: TaskRun[];
}

export interface TaskRun {
  id: string;
  job_run_id: string;
  task_id: string;
  atom_id: string;
  engine: string;
  image: string;
  command: string;
  status: string;
  result?: string;
  error?: string;
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
  node_selector: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface DAGNode {
  id: string;
  atom_id: string;
  successors?: string[];
}

export interface DAGEdge {
  from: string;
  to: string;
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
  success_rate: number;
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

  // 202 Accepted / 204 No Content — body is empty, nothing to parse
  const contentType = response.headers.get("content-type");
  if (!contentType?.includes("application/json")) {
    return undefined as T;
  }

  return response.json();
}

export interface WorkerClaim {
  id: string;
  task_run_id: string;
  node_id: string;
  status: string;
  claimed_at: string;
  expires_at: string;
}

export interface WorkerStatus {
  node_address: string;
  active_claims: WorkerClaim[];
  running_count: number;
  expired_count: number;
  last_activity?: string;
}

export interface HealthCheckResult {
  status?: "healthy" | "degraded";
  latency_ms?: number;
  count?: number;
}

export interface HealthCheck {
  status: "healthy" | "degraded";
  uptime: number; // Go time.Duration serialized as int64 nanoseconds
  checks?: {
    database?: HealthCheckResult;
    active_runs?: HealthCheckResult;
    triggers?: HealthCheckResult;
  };
}

export const api = {
  getJobs: () => request<Job[]>("/jobs"),
  getJob: (id: string) => request<Job>(`/jobs/${id}`),
  getJobRuns: (jobId: string) => request<JobRun[]>(`/jobs/${jobId}/runs`),
  getJobRun: (jobId: string, runId: string) => request<JobRun>(`/jobs/${jobId}/runs/${runId}`),
  getJobDAG: (jobId: string) => request<JobDAGResponse>(`/jobs/${jobId}/dag`),
  getJobTasks: (jobId: string) => request<JobTask[]>(`/jobs/${jobId}/tasks`),
  triggerJob: (jobId: string) => request<JobRun>(`/jobs/${jobId}/run`, { method: "POST" }),
  retryCallbacks: (jobId: string, runId: string) =>
    request<void>(`/jobs/${jobId}/runs/${runId}/callbacks/retry`, { method: "POST" }),
  deleteJob: (id: string) => request<void>(`/jobs/${id}`, { method: "DELETE" }),
  getTriggers: () => request<Trigger[]>("/triggers"),
  getTrigger: (id: string) => request<Trigger>(`/triggers/${id}`),
  fireTrigger: (id: string) => request<void>(`/triggers/${id}`, { method: "PUT" }),
  getAtoms: () => request<Atom[]>("/atoms"),
  getAtom: (id: string) => request<Atom>(`/atoms/${id}`),
  deleteAtom: (id: string) => request<void>(`/atoms/${id}`, { method: "DELETE" }),
  applyJobDef: async (yamlText: string): Promise<{ applied: number }> => {
    // The backend expects {"definitions":[...]} JSON, not raw YAML.
    // Parse all YAML documents from the editor, then POST as JSON — matching
    // the same contract used by the CLI (cmd/job/apply.go).
    const { loadAll } = await import("js-yaml");
    const definitions: unknown[] = [];
    loadAll(yamlText, (doc) => { if (doc) definitions.push(doc); });
    if (definitions.length === 0) {
      throw new ApiError(400, "No definitions found in the YAML");
    }
    return request<{ applied: number }>("/jobdefs/apply", {
      method: "POST",
      body: JSON.stringify({ definitions }),
    });
  },
  getWorkers: (nodeAddress: string) => request<WorkerStatus>(`/nodes/${nodeAddress}/workers`),
  getHealth: async (): Promise<HealthCheck> => {
    // /health is mounted at the root, not under /v1
    const healthBase = API_BASE_URL.replace(/\/v1\/?$/, "");
    const response = await fetch(`${healthBase}/health`);
    if (!response.ok) {
      throw new ApiError(response.status, await response.text());
    }
    return response.json();
  },
  getStats: () => request<StatsResponse>("/stats"),
};
