export interface Job {
  id: string;
  alias: string;
  trigger_id: string;
  labels: Record<string, unknown>;
  annotations: Record<string, unknown>;
  paused: boolean;
  created_at: string;
  updated_at: string;
  trigger?: Trigger;
  latest_run?: JobRun;
}

export interface JobRun {
  id: string;
  job_id: string;
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
  created_at: string;
  updated_at: string;
}

export interface DAGNode {
  id: string;
  atom_id: string;
  next_id?: string;
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
}

export interface TriggerRunRequest {
  params?: Record<string, string>;
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

  return response.json();
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
  getAtoms: () => request<Atom[]>("/atoms"),
  getAtom: (id: string) => request<Atom>(`/atoms/${id}`),
  getStats: () => request<StatsResponse>("/stats"),
};
