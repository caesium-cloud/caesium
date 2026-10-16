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
