export type ConnectorCapability = "discovery" | "inspection" | "history" | "relationships" | "status_query" | "action_submission" | "receipt_lookup";

export interface ConnectorConnection {
  id: string;
  provider: string;
  scope: string;
  enabled: boolean;
  capabilities: ConnectorCapability[];
  ui_url?: string;
  console_refresh_ms?: number;
}

export interface ConnectorConnections {
  connections: ConnectorConnection[];
  ready: boolean;
  fingerprint: string;
}

export interface ExternalExecution {
  id: string;
  reference: { connection_id: string; coordinates: Record<string, string> };
  availability: string;
  completeness: string;
  observed_at: string;
  snapshot_observed_at?: string;
  binding?: ConnectorBinding;
  metadata: {
    workflow_type: string;
    workflow_id: string;
    run_id: string;
    scope: string;
    source_event_id: number;
    native_status: string;
    display_status: string;
    terminal: boolean;
    failure: boolean;
    incomplete?: boolean;
    pending_activities?: { activity_id: string; activity_type: string; attempt: number; maximum_attempts?: number; state: string }[];
  };
}

export interface ExternalExecutionPage {
  executions: ExternalExecution[];
  cursor?: string;
  incomplete?: boolean;
}

export interface ExternalHistoryPage {
  workflow_id: string;
  run_id: string;
  scope: string;
  observed_at: string;
  cursor?: string;
  incomplete?: boolean;
  events?: { event_id: number; event_type: string }[];
  activities?: { event_id: number; scheduled_event_id: number; activity_id: string; activity_type: string; attempt: number }[];
  problems?: { event_id: number; cause: string }[];
}

export interface ConnectorExecutionReference {
  connection_id: string;
  coordinates: Record<string, string>;
}

export interface ExternalRelationship {
  type: "parent" | "child" | "continuation" | (string & {});
  id: string;
  reference: ConnectorExecutionReference;
  event_id?: number;
}

/** Verified local admission identity, rather than inferred data provenance. */
export interface ExternalDelegation {
  id: string;
  opaque_id: string;
  execution_id?: string;
  reference?: ConnectorExecutionReference;
  connection_id: string;
  activity_id: string;
  activity_type: string;
  relation_type: string;
  binding_version: string;
  admission_id: string;
  job_id: string;
  idempotency_key: string;
  outcome: string;
  queue_id?: string;
  run_id?: string;
  source_event_id: number;
  verified_at: string;
}

export interface ExternalRelationshipsPage {
  relationships: ExternalRelationship[];
  correlations: ExternalDelegation[];
  incomplete: boolean;
  cursor?: string;
}

export interface RunExternalExecutions {
  executions: ExternalDelegation[];
}

export interface ConnectorPageQuery {
  cursor?: string;
  page_size?: number;
}

export interface ConnectorListQuery extends ConnectorPageQuery {
  workflow_type?: string;
}

export interface ConnectorSearch extends ConnectorListQuery {
  connection?: string;
}

export function connectorRefreshMs(connection?: ConnectorConnection): number {
  const value = connection?.console_refresh_ms;
  return typeof value === "number" && Number.isFinite(value) ? Math.max(10_000, value) : 10_000;
}

// Browser timer delays are signed 32-bit milliseconds. Legal configuration
// durations can be much larger; passing one to setInterval can turn years
// into a 1ms polling loop. Keep the configured delay and disable that timer.
export function connectorRefreshSchedule(connection?: ConnectorConnection): { interval: number | false; description: string } {
  const refreshMs = connectorRefreshMs(connection);
  if (refreshMs > 2_147_483_647) {
    return { interval: false, description: `Automatic refresh disabled: configured interval ${refreshMs / 1000}s exceeds the browser timer range.` };
  }
  return { interval: refreshMs, description: `Refresh every ${refreshMs / 1000}s.` };
}

export function observationTimestamp(observedAt: string | null | undefined): number | undefined {
  const observed = typeof observedAt === "string" ? Date.parse(observedAt) : NaN;
  // Missing and zero-value provider timestamps are unknown, rather than a
  // centuries-old observation. Connector executions postdate the Unix epoch.
  return Number.isFinite(observed) && observed > 0 ? observed : undefined;
}

export function observationAge(observedAt: string | null | undefined, now = Date.now()): string {
  const observed = observationTimestamp(observedAt);
  if (observed === undefined) return "Observation time unavailable";
  const seconds = Math.max(0, Math.floor((now - observed) / 1000));
  return seconds < 60 ? `${seconds}s ago` : seconds < 3600 ? `${Math.floor(seconds / 60)}m ago` : `${Math.floor(seconds / 3600)}h ago`;
}

// A UI link is optional configuration. Never infer a remote UI from the RPC
// endpoint, and never let credentials or a non-web scheme reach an anchor.
export function temporalExecutionURL(connection: ConnectorConnection | undefined, execution: ExternalExecution): string | undefined {
  if (connection?.provider !== "temporal" || !connection.ui_url) return;
  try {
    const url = new URL(connection.ui_url);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) return;
    const { scope, workflow_id, run_id } = execution.metadata;
    if (!scope || !workflow_id || !run_id) return;
    url.pathname = `${url.pathname.replace(/\/$/, "")}/namespaces/${encodeURIComponent(scope)}/workflows/${encodeURIComponent(workflow_id)}/${encodeURIComponent(run_id)}/history`;
    return url.href;
  } catch {
    return;
  }
}

/** The configured, closed JSON schema exposed by the authenticated server. */
export interface ConnectorSchema {
  type?: string | string[];
  title?: string;
  description?: string;
  properties?: Record<string, ConnectorSchema>;
  required?: string[];
  enum?: unknown[];
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  [keyword: string]: unknown;
}

export interface ConnectorAction {
  name: string;
  input_schema: ConnectorSchema;
  result_schema: ConnectorSchema;
  minimum_role: string;
  available: boolean;
}

export interface ConnectorBinding {
  name: string;
  version: string;
  display_name: string;
  available: boolean;
  actions: ConnectorAction[];
  status_query?: string;
  status_query_result_schema?: ConnectorSchema;
}

export interface ConnectorOperation {
  id: string;
  connection_id: string;
  execution_id: string;
  execution?: ConnectorExecutionReference;
  action: string;
  binding_name: string;
  binding_version: string;
  state: "submitted" | "accepted" | "completed" | "rejected" | "not_dispatched" | "unknown" | (string & {});
  revision: number;
  attempt: number;
  actor: { principal_kind: string; stable_id: string; subject?: string; role?: string };
  external_update_id: string;
  configuration_epoch?: string;
  metadata_omitted?: boolean;
  evidence?: {
    state: string;
    native_stage?: string;
    preflight_rpc_code?: string;
    rpc_code?: string;
    result?: unknown;
    failure_kind?: string;
    rejection_proof?: string;
    result_omitted?: boolean;
  };
  created_at?: string;
  updated_at?: string;
}

export interface ConnectorOperationResult {
  operation: ConnectorOperation;
  created: boolean;
  conflict: boolean;
  pending: boolean;
}

export interface ConnectorOperationPage {
  operations: ConnectorOperation[];
  cursor?: string;
}
