import { IdChip } from "@/components/ui/id-chip";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StatusBadge } from "@/components/ui/status-badge";
import type { AgentAction, AgentSession, Incident } from "@/lib/api";
import { actionSummary, formatDateTime, sessionElapsed, sessionProfileLabel } from "./incident-utils";

interface AgentActivityProps {
  incident: Incident;
  sessions: AgentSession[];
  actions: AgentAction[];
}

export function AgentActivity({ incident, sessions, actions }: AgentActivityProps) {
  const activeSessions = sessions.filter((session) => session.state === "pending" || session.state === "running");
  const visibleSessions = activeSessions.length > 0 ? activeSessions : sessions.slice(-2);

  if (sessions.length === 0 && incident.status !== "triaging") {
    return null;
  }

  return (
    <section data-testid="agent-activity" className="relative border-t border-border rounded-none bg-transparent"><span aria-hidden="true" className="cs-now-head absolute left-0 top-0 h-[7px] w-[7px] rounded-full bg-running" />
      <div className="border-b border-cyan-glow/15 pb-3">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <h3 className="flex items-center gap-2 text-sm">

              Agent activity
            </h3>
            <div className="mt-1 text-xs text-text-3">
              {activeSessions.length > 0 ? "Live triage session" : "Last recorded session"}
            </div>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled
            title="Take over is unavailable on this server build"
            data-testid="incident-take-over"
          >

            Take over
          </Button>
        </div>
      </div>
      <div className="space-y-4 p-4">
        {visibleSessions.length > 0 ? (
          visibleSessions.map((session) => (
            <SessionPanel
              key={session.id}
              session={session}
              actions={actions.filter((action) => action.session_id === session.id)}
            />
          ))
        ) : (
          <div className="border-b border-border py-3 text-xs text-text-3">
            No agent session has been recorded yet for this triaging incident.
          </div>
        )}
      </div>
    </section>
  );
}

function SessionPanel({ session, actions }: { session: AgentSession; actions: AgentAction[] }) {
  return (
    <div className="border-b border-border py-3">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge status={session.state} domain="agent-session" size="sm" />
            <span className="text-xs text-text-2">session <IdChip value={session.id} label="session id" /></span>
            <Badge variant="outline" className="text-[11px]">
              profile {sessionProfileLabel(session)}
            </Badge>
          </div>
          <div className="mt-1 flex flex-wrap gap-2 text-[11px] text-text-3">
            <span>{session.engine || "engine unknown"}</span>
            {session.container_id ? <span>container <IdChip value={session.container_id} label="container id" /></span> : null}
            <span>started {formatDateTime(session.started_at ?? session.created_at)}</span>
          </div>
        </div>
        <div className="grid grid-cols-3 gap-2 text-right text-[11px] text-text-3">
          <Metric label="elapsed" value={sessionElapsed(session)} />
          <Metric label="tools" value={String(session.actions_used)} />
          <Metric label="tokens" value={String(session.tokens_used)} />
        </div>
      </div>

      {actions.length > 0 ? (
        <div className="mt-3 space-y-2">
          {actions.map((action) => (
            <div key={action.id} className="flex flex-wrap items-center gap-2 min-h-[42px] border-b border-border py-2 text-xs">
              <Badge variant="outline" className="text-[11px]">
                tier {action.tier}
              </Badge>
              <span className="text-text-2">{actionSummary(action)}</span>
              <StatusBadge status={action.status} domain="agent-action" size="sm" />
            </div>
          ))}
        </div>
      ) : null}

      <div className="mt-3">
        <div className="mb-1.5 flex items-center gap-2 text-[11px] font-bold lowercase text-text-3">

          Agent log
        </div>
        <pre className="max-h-60 overflow-auto rounded-md border border-border/50 bg-void p-3 text-xs leading-relaxed text-text-3">
          {session.session_log?.trim() || "No persisted agent log yet."}
        </pre>
      </div>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-border/50 bg-background/50 px-2 py-1">
      <div className="flex items-center justify-end gap-1 text-text-3">

        {label}
      </div>
      <div className="text-text-1">{value}</div>
    </div>
  );
}
