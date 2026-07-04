import { type ReactNode, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  ArrowLeft,
  Bot,
  CheckCircle2,
  ClipboardList,
  ExternalLink,
  FileSearch,
  GitCompare,
  ListChecks,
  PlayCircle,
  RotateCcw,
  ScrollText,
  ShieldAlert,
  TerminalSquare,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/ui/status-badge";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { LineageGraph } from "@/features/jobs/LineageGraph";
import { LogViewer } from "@/features/jobs/LogViewer";
import { ReplayDialog } from "@/features/jobs/ReplayDialog";
import { RunDiffView } from "@/features/jobs/RunDiffView";
import { TaskWhyView } from "@/features/jobs/TaskWhyView";
import { api, type AgentAction, type AgentSession, type ApprovalRequest, type Incident } from "@/lib/api";
import { events } from "@/lib/events";
import { cn, shortId } from "@/lib/utils";
import { AgentActivity } from "./AgentActivity";
import { ApprovalCard } from "./ApprovalCard";
import {
  actionSummary,
  formatActionType,
  formatDateTime,
  formatIncidentClass,
  formatJson,
  incidentAge,
  incidentSummary,
  isAwaitingApproval,
  jobLabel,
  recordFromUnknown,
  stringField,
  buildJobAliasMap,
  INCIDENT_EVENT_TYPES,
} from "./incident-utils";

type TimelineItem = {
  id: string;
  timestamp: string;
  tone: "danger" | "warning" | "info" | "success";
  title: string;
  subtitle?: string;
  icon: ReactNode;
  content: ReactNode;
};

export function IncidentDetailPage() {
  const { incidentId } = useParams({ strict: false }) as { incidentId: string };
  const queryClient = useQueryClient();
  const [replayOpen, setReplayOpen] = useState(false);

  const { data: features, isLoading: isLoadingFeatures } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });
  const incidentsEnabled = features?.agent_remediation_enabled === true;

  const detailQuery = useQuery({
    queryKey: ["incident", incidentId],
    queryFn: () => api.getIncident(incidentId),
    enabled: incidentsEnabled && Boolean(incidentId),
    refetchInterval: 10_000,
  });

  const jobsQuery = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    enabled: incidentsEnabled,
    staleTime: 30_000,
  });

  useEffect(() => {
    if (!incidentsEnabled) return;
    const onIncidentEvent = () => {
      queryClient.invalidateQueries({ queryKey: ["incident", incidentId] });
      queryClient.invalidateQueries({ queryKey: ["incidents"] });
    };
    INCIDENT_EVENT_TYPES.forEach((type) => events.subscribe(type, onIncidentEvent));
    return () => {
      INCIDENT_EVENT_TYPES.forEach((type) => events.unsubscribe(type, onIncidentEvent));
    };
  }, [incidentId, incidentsEnabled, queryClient]);

  const detail = detailQuery.data;
  const incident = detail?.incident;
  const actions = useMemo(() => detail?.actions ?? [], [detail?.actions]);
  const approvals = useMemo(() => detail?.approvals ?? [], [detail?.approvals]);
  const sessions = useMemo(() => detail?.sessions ?? [], [detail?.sessions]);
  const jobAliases = useMemo(() => buildJobAliasMap(jobsQuery.data), [jobsQuery.data]);
  const actionById = useMemo(() => {
    const map = new Map<string, AgentAction>();
    actions.forEach((action) => map.set(action.id, action));
    return map;
  }, [actions]);
  const pendingApprovals = useMemo(
    () => approvals.filter((approval) => approval.decision === "pending"),
    [approvals],
  );
  const timeline = useMemo(
    () => (incident ? buildTimeline(incident, sessions, actions, approvals, actionById) : []),
    [actionById, actions, approvals, incident, sessions],
  );

  if (isLoadingFeatures || detailQuery.isLoading) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-96 w-full" />
      </div>
    );
  }

  if (!incidentsEnabled) {
    return (
      <EmptyState
        title="Incidents disabled"
        subtitle="Agent remediation is not enabled on this Caesium server."
        icon={<ShieldAlert className="h-12 w-12 text-text-3" />}
        className="py-16"
      />
    );
  }

  if (detailQuery.error) {
    return (
      <EmptyState
        title="Incident unavailable"
        subtitle={detailQuery.error.message}
        icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        className="py-16"
      />
    );
  }

  if (!incident) {
    return (
      <EmptyState
        title="Incident not found"
        subtitle="The incident API returned no detail for this id."
        icon={<ShieldAlert className="h-12 w-12 text-text-3" />}
        className="py-16"
      />
    );
  }

  const currentJobLabel = jobLabel(incident, jobAliases);

  return (
    <div className="space-y-6" data-testid="incident-detail-page">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <Link
            to="/incidents"
            search={{}}
            className="mb-2 inline-flex items-center gap-1 text-xs text-text-3 transition hover:text-text-1"
          >
            <ArrowLeft className="h-3.5 w-3.5" />
            Incidents
          </Link>
          <p className="mb-1 text-xs font-medium uppercase tracking-widest text-text-3">
            {currentJobLabel}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-2xl font-bold tracking-tight">Incident {shortId(incident.id)}</h1>
            <StatusBadge status={incident.status} size="sm" />
            <Badge variant="outline">{formatIncidentClass(incident.class)}</Badge>
            {isAwaitingApproval(incident) ? (
              <Badge variant="outline" className="border-gold/30 bg-gold/10 text-gold">
                approval
              </Badge>
            ) : null}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <span>opened {formatDateTime(incident.opened_at)}</span>
            <span className="text-text-4">·</span>
            <span>{incidentAge(incident)}</span>
            {incident.task_name ? (
              <>
                <span className="text-text-4">·</span>
                <span className="font-mono">{incident.task_name}</span>
              </>
            ) : null}
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button asChild variant="outline" size="sm">
            <Link to="/jobs/$jobId" params={{ jobId: incident.job_id }}>
              Job
              <ExternalLink className="h-3.5 w-3.5" />
            </Link>
          </Button>
          {incident.run_id ? (
            <Button asChild variant="outline" size="sm">
              <Link to="/jobs/$jobId/runs/$runId" params={{ jobId: incident.job_id, runId: incident.run_id }}>
                Run
                <ExternalLink className="h-3.5 w-3.5" />
              </Link>
            </Button>
          ) : null}
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_360px]">
        <div className="space-y-4">
          <Card className="border-warning/30 bg-warning/5">
            <CardHeader className="border-b border-warning/20 pb-3">
              <CardTitle className="flex items-center gap-2 text-sm">
                <AlertTriangle className="h-4 w-4 text-warning" />
                Current summary
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3 p-4">
              <p className="text-sm text-text-2">{incidentSummary(incident)}</p>
              <div className="grid gap-2 text-xs sm:grid-cols-3">
                <Metadata label="Occurrences" value={String(incident.occurrence_count)} />
                <Metadata label="Attempts" value={String(incident.attempt)} />
                <Metadata label="Dedupe" value={incident.dedupe_key} mono />
              </div>
            </CardContent>
          </Card>

          {pendingApprovals.length > 0 ? (
            <div className="space-y-3" data-testid="incident-pending-approval-cards">
              {pendingApprovals.map((approval) => (
                <ApprovalCard
                  key={approval.id}
                  incidentId={incident.id}
                  jobId={incident.job_id}
                  incidentTaskName={incident.task_name}
                  approval={approval}
                  action={actionById.get(approval.action_id)}
                />
              ))}
            </div>
          ) : null}

          <AgentActivity incident={incident} sessions={sessions} actions={actions} />

          <EvidencePanel incident={incident} replayOpen={replayOpen} setReplayOpen={setReplayOpen} />

          <Card data-testid="incident-timeline" className="overflow-hidden border-graphite/40 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="flex items-center gap-2 text-sm">
                <ClipboardList className="h-4 w-4 text-cyan-glow" />
                Incident timeline
              </CardTitle>
            </CardHeader>
            <CardContent className="p-0">
              <div className="divide-y divide-border/50">
                {timeline.map((item) => (
                  <TimelineRow key={item.id} item={item} />
                ))}
              </div>
            </CardContent>
          </Card>
        </div>

        <aside className="space-y-4">
          <ApprovalHistory approvals={approvals} actionById={actionById} incident={incident} />
          <Card className="border-graphite/40 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="flex items-center gap-2 text-sm">
                <ListChecks className="h-4 w-4 text-cyan-glow" />
                Actions
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 p-4">
              {actions.length === 0 ? (
                <div className="text-sm text-text-3">No actions recorded.</div>
              ) : (
                actions.map((action) => (
                  <div key={action.id} className="rounded-md border border-border/50 bg-background/50 p-3">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge variant="outline" className="text-[10px]">
                        tier {action.tier}
                      </Badge>
                      <StatusBadge status={action.status} size="sm" />
                      <span className="text-xs text-text-2">{actionSummary(action)}</span>
                    </div>
                  </div>
                ))
              )}
            </CardContent>
          </Card>
        </aside>
      </div>
    </div>
  );
}

function buildTimeline(
  incident: Incident,
  sessions: AgentSession[],
  actions: AgentAction[],
  approvals: ApprovalRequest[],
  actionById: Map<string, AgentAction>,
): TimelineItem[] {
  const items: TimelineItem[] = [
    {
      id: "failure",
      timestamp: incident.opened_at,
      tone: "danger",
      title: "Failure captured",
      subtitle: incident.task_name || "run-level failure",
      icon: <AlertTriangle className="h-4 w-4" />,
      content: (
        <div className="space-y-2">
          <p>{incident.last_error || "The incident manager recorded a failing run event."}</p>
          <JsonDetails value={incident.evidence} />
        </div>
      ),
    },
    {
      id: "classification",
      timestamp: incident.created_at,
      tone: "info",
      title: "Classified",
      subtitle: formatIncidentClass(incident.class),
      icon: <FileSearch className="h-4 w-4" />,
      content: <p>Failure class {formatIncidentClass(incident.class)} assigned from recorded evidence.</p>,
    },
  ];

  sessions.forEach((session) => {
    items.push({
      id: `session-${session.id}`,
      timestamp: session.started_at ?? session.created_at,
      tone: session.state === "failed" || session.state === "timed_out" ? "danger" : "info",
      title: "Agent session",
      subtitle: session.state,
      icon: <Bot className="h-4 w-4" />,
      content: (
        <div className="space-y-3">
          <div className="grid gap-2 text-xs sm:grid-cols-3">
            <Metadata label="Session" value={shortId(session.id)} mono />
            <Metadata label="Engine" value={session.engine || "unknown"} />
            <Metadata label="Tools" value={String(session.actions_used)} />
          </div>
          {session.session_log ? (
            <JsonDetails title="Session log" value={{ log: session.session_log }} />
          ) : null}
        </div>
      ),
    });
  });

  actions.forEach((action) => {
    const noteText = action.type === "note" ? stringField(action.params, "text", "message", "summary") : undefined;
    items.push({
      id: `action-${action.id}`,
      timestamp: action.created_at,
      tone: action.status === "failed" || action.status === "rejected" ? "danger" : "info",
      title: action.type === "note" ? "Agent observation" : "Action recorded",
      subtitle: action.type === "note" ? noteText : actionSummary(action),
      icon: action.type === "note" ? <Bot className="h-4 w-4" /> : <ListChecks className="h-4 w-4" />,
      content: (
        <div className="space-y-3">
          {noteText ? <p className="text-sm text-text-2">{noteText}</p> : null}
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="outline">actor {action.actor}</Badge>
            <Badge variant="outline">tier {action.tier}</Badge>
            <StatusBadge status={action.status} size="sm" />
          </div>
          {action.type !== "note" ? <JsonDetails title="Params" value={action.params} /> : null}
          {action.result ? <JsonDetails title="Result" value={action.result} /> : null}
        </div>
      ),
    });
  });

  approvals.forEach((approval) => {
    const action = actionById.get(approval.action_id);
    items.push({
      id: `approval-${approval.id}`,
      timestamp: approval.created_at,
      tone:
        approval.decision === "approved"
          ? "success"
          : approval.decision === "rejected"
            ? "danger"
            : "warning",
      title: "Approval requested",
      subtitle: action ? formatActionType(action.type) : approval.decision,
      icon: <CheckCircle2 className="h-4 w-4" />,
      content: (
        <ApprovalCard
          incidentId={incident.id}
          jobId={incident.job_id}
          incidentTaskName={incident.task_name}
          approval={approval}
          action={action}
          compact
        />
      ),
    });
  });

  if (incident.closed_at || incident.resolution_summary) {
    items.push({
      id: "resolution",
      timestamp: incident.closed_at ?? incident.updated_at,
      tone: "success",
      title: "Resolution",
      subtitle: incident.status,
      icon: <CheckCircle2 className="h-4 w-4" />,
      content: <p>{incident.resolution_summary || `Incident is ${incident.status}.`}</p>,
    });
  }

  return items.sort((a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime());
}

function EvidencePanel({
  incident,
  replayOpen,
  setReplayOpen,
}: {
  incident: Incident;
  replayOpen: boolean;
  setReplayOpen: (open: boolean) => void;
}) {
  const lineageNamespace =
    stringField(incident.evidence, "dataset_namespace", "namespace") ?? incident.namespace ?? "";
  const lineageName = stringField(incident.evidence, "dataset_name", "name") ?? "";
  const rightRunId =
    incident.remediation_target_run_id && incident.remediation_target_run_id !== incident.run_id
      ? incident.remediation_target_run_id
      : undefined;

  return (
    <Card className="overflow-hidden border-graphite/40 bg-midnight/30">
      <CardHeader className="border-b border-border/50 pb-3">
        <CardTitle className="flex items-center gap-2 text-sm">
          <FileSearch className="h-4 w-4 text-cyan-glow" />
          Primary evidence
        </CardTitle>
      </CardHeader>
      <CardContent className="p-4">
        <Tabs defaultValue="why">
          <TabsList className="grid h-auto w-full grid-cols-2 gap-1 bg-midnight p-1 md:grid-cols-5">
            <TabsTrigger value="why" className="gap-1 text-xs">
              <FileSearch className="h-3.5 w-3.5" />
              Why
            </TabsTrigger>
            <TabsTrigger value="logs" className="gap-1 text-xs">
              <TerminalSquare className="h-3.5 w-3.5" />
              Logs
            </TabsTrigger>
            <TabsTrigger value="lineage" className="gap-1 text-xs">
              <ScrollText className="h-3.5 w-3.5" />
              Lineage
            </TabsTrigger>
            <TabsTrigger value="diff" className="gap-1 text-xs">
              <GitCompare className="h-3.5 w-3.5" />
              Diff
            </TabsTrigger>
            <TabsTrigger value="replay" className="gap-1 text-xs">
              <RotateCcw className="h-3.5 w-3.5" />
              Replay
            </TabsTrigger>
          </TabsList>

          <TabsContent value="why">
            {incident.run_id ? (
              <TaskWhyView jobId={incident.job_id} runId={incident.run_id} taskName={incident.task_name} />
            ) : (
              <EvidenceMissing label="Run id unavailable" />
            )}
          </TabsContent>
          <TabsContent value="logs">
            {incident.run_id && incident.task_id ? (
              <div className="h-96 overflow-hidden rounded-md border border-border/60" data-testid="incident-log-viewer">
                <LogViewer
                  jobId={incident.job_id}
                  runId={incident.run_id}
                  taskId={incident.task_id}
                  error={incident.last_error}
                  status={incident.status}
                />
              </div>
            ) : (
              <EvidenceMissing label="Task run id unavailable" />
            )}
          </TabsContent>
          <TabsContent value="lineage">
            <div className="h-[520px] overflow-hidden rounded-md border border-border/60" data-testid="incident-lineage-graph">
              <LineageGraph initialNamespace={lineageNamespace} initialName={lineageName} />
            </div>
          </TabsContent>
          <TabsContent value="diff">
            {incident.run_id ? (
              <RunDiffView jobId={incident.job_id} leftRunId={incident.run_id} rightRunId={rightRunId} />
            ) : (
              <EvidenceMissing label="Run id unavailable" />
            )}
          </TabsContent>
          <TabsContent value="replay">
            <div className="rounded-md border border-border/60 bg-background/50 p-4">
              <Button
                type="button"
                variant="outline"
                onClick={() => setReplayOpen(true)}
                disabled={!incident.run_id}
                data-testid="incident-replay-trigger"
              >
                <PlayCircle className="h-4 w-4" />
                Replay baseline
              </Button>
              {incident.run_id ? (
                <ReplayDialog
                  jobId={incident.job_id}
                  baselineRunId={incident.run_id}
                  open={replayOpen}
                  onOpenChange={setReplayOpen}
                />
              ) : null}
            </div>
          </TabsContent>
        </Tabs>
      </CardContent>
    </Card>
  );
}

function TimelineRow({ item }: { item: TimelineItem }) {
  return (
    <div data-testid="incident-timeline-event" className="grid gap-3 px-4 py-4 md:grid-cols-[160px_28px_minmax(0,1fr)]">
      <div className="text-xs text-text-3">
        <div>{formatDateTime(item.timestamp)}</div>
        <div className="mt-1 font-mono text-[10px] text-text-4">{item.timestamp}</div>
      </div>
      <div className="flex justify-center">
        <span
          className={cn(
            "flex h-7 w-7 items-center justify-center rounded-full border",
            item.tone === "danger" && "border-danger/40 bg-danger/10 text-danger",
            item.tone === "warning" && "border-warning/40 bg-warning/10 text-warning",
            item.tone === "success" && "border-success/40 bg-success/10 text-success",
            item.tone === "info" && "border-cyan-glow/40 bg-cyan-glow/10 text-cyan-glow",
          )}
        >
          {item.icon}
        </span>
      </div>
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="text-sm font-semibold text-text-1">{item.title}</h2>
          {item.subtitle ? (
            <Badge variant="outline" className="max-w-full truncate text-[10px]">
              {item.subtitle}
            </Badge>
          ) : null}
        </div>
        <div className="mt-3 text-sm text-text-2">{item.content}</div>
      </div>
    </div>
  );
}

function ApprovalHistory({
  approvals,
  actionById,
  incident,
}: {
  approvals: ApprovalRequest[];
  actionById: Map<string, AgentAction>;
  incident: Incident;
}) {
  return (
    <Card className="border-gold/30 bg-gold/5">
      <CardHeader className="border-b border-gold/20 pb-3">
        <CardTitle className="flex items-center gap-2 text-sm">
          <CheckCircle2 className="h-4 w-4 text-gold" />
          Approval cards
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 p-4">
        {approvals.length === 0 ? (
          <div className="text-sm text-text-3">No approvals requested.</div>
        ) : (
          approvals.map((approval) => (
            <ApprovalCard
              key={approval.id}
              incidentId={incident.id}
              jobId={incident.job_id}
              incidentTaskName={incident.task_name}
              approval={approval}
              action={actionById.get(approval.action_id)}
              compact
            />
          ))
        )}
      </CardContent>
    </Card>
  );
}

function JsonDetails({ value, title = "Evidence" }: { value: unknown; title?: string }) {
  const record = recordFromUnknown(value);
  if (Object.keys(record).length === 0) return null;
  return (
    <details className="rounded-md border border-border/50 bg-background/50 p-3">
      <summary className="cursor-pointer text-[10px] font-semibold uppercase tracking-wide text-text-3">
        {title}
      </summary>
      <pre className="mt-2 overflow-auto font-mono text-xs text-text-3">{formatJson(value)}</pre>
    </details>
  );
}

function Metadata({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="rounded-md border border-border/50 bg-background/50 p-3">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-text-3">{label}</div>
      <div className={cn("mt-1 truncate text-sm text-text-1", mono && "font-mono text-xs")}>{value}</div>
    </div>
  );
}

function EvidenceMissing({ label }: { label: string }) {
  return (
    <div className="rounded-md border border-border/60 bg-background/50 px-3 py-6 text-center text-sm text-text-3">
      {label}
    </div>
  );
}
