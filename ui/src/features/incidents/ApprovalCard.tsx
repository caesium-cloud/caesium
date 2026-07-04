import { type FormEvent, type ReactNode, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, FileCode2, GitBranch, RotateCcw, SkipForward, XCircle } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { AgentAction, ApprovalRequest, DAGNode, JobTask } from "@/lib/api";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  formatActionType,
  formatJson,
  numberField,
  recordFromUnknown,
  stringField,
} from "./incident-utils";

interface ApprovalCardProps {
  incidentId: string;
  jobId: string;
  incidentTaskName?: string;
  approval: ApprovalRequest;
  action?: AgentAction;
  compact?: boolean;
}

export function ApprovalCard({
  incidentId,
  jobId,
  incidentTaskName,
  approval,
  action,
  compact = false,
}: ApprovalCardProps) {
  const queryClient = useQueryClient();
  const [reason, setReason] = useState("");
  const isPending = approval.decision === "pending";

  const approveMutation = useMutation({
    mutationFn: () => api.approveIncident(incidentId, approval.id, reason.trim() || undefined),
    onSuccess: () => {
      toast.success("Approval recorded");
      invalidateIncidentQueries(queryClient, incidentId);
    },
    onError: (err: Error) => toast.error(`Approval failed: ${err.message}`),
  });

  const rejectMutation = useMutation({
    mutationFn: () => api.rejectIncident(incidentId, approval.id, reason.trim() || undefined),
    onSuccess: () => {
      toast.success("Rejection recorded");
      invalidateIncidentQueries(queryClient, incidentId);
    },
    onError: (err: Error) => toast.error(`Rejection failed: ${err.message}`),
  });

  function submitDecision(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
  }

  const actionType = action?.type ?? "unknown_action";

  return (
    <Card
      data-testid="approval-card"
      className={cn(
        "overflow-hidden border-gold/30 bg-gold/5",
        compact ? "shadow-none" : "shadow-lg",
      )}
    >
      <CardHeader className="border-b border-gold/20 pb-3">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
          <div className="min-w-0">
            <CardTitle className="flex items-center gap-2 text-sm">
              <DecisionIcon type={actionType} />
              Tier-3 proposal
            </CardTitle>
            <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
              <Badge variant="outline" className="border-gold/30 bg-gold/10 text-gold">
                {formatActionType(actionType)}
              </Badge>
              {action ? <span>tier {action.tier}</span> : null}
              {approval.approvers_hint ? <span>{approval.approvers_hint}</span> : null}
            </div>
          </div>
          <Badge variant={isPending ? "outline" : approval.decision === "approved" ? "success" : "destructive"}>
            {approval.decision}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-4 p-4">
        <ActionPreview
          action={action}
          jobId={jobId}
          incidentTaskName={incidentTaskName}
        />

        {isPending ? (
          <form onSubmit={submitDecision} className="space-y-3">
            <label className="block space-y-1.5">
              <span className="text-[10px] font-semibold uppercase tracking-wide text-text-3">
                Decision reason
              </span>
              <textarea
                data-testid="approval-reason"
                value={reason}
                onChange={(event) => setReason(event.target.value)}
                rows={compact ? 2 : 3}
                className="min-h-20 w-full resize-y rounded-md border border-border bg-background/70 px-3 py-2 text-sm text-text-1 outline-none transition focus:border-cyan-glow"
                placeholder="Optional operator note"
              />
            </label>
            <div className="flex flex-wrap gap-2">
              <Button
                type="button"
                size="sm"
                data-testid="approval-approve"
                onClick={() => approveMutation.mutate()}
                disabled={approveMutation.isPending || rejectMutation.isPending}
              >
                <CheckCircle2 className="h-3.5 w-3.5" />
                Approve
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                data-testid="approval-reject"
                onClick={() => rejectMutation.mutate()}
                disabled={approveMutation.isPending || rejectMutation.isPending}
                className="border-danger/30 text-danger hover:bg-danger/10"
              >
                <XCircle className="h-3.5 w-3.5" />
                Reject
              </Button>
            </div>
          </form>
        ) : (
          <div className="rounded-md border border-border/60 bg-background/50 p-3 text-xs text-text-3">
            {approval.decider ? <div>Decider: {approval.decider}</div> : null}
            {approval.reason ? <div className="mt-1">Reason: {approval.reason}</div> : null}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function ActionPreview({
  action,
  jobId,
  incidentTaskName,
}: {
  action?: AgentAction;
  jobId: string;
  incidentTaskName?: string;
}) {
  if (!action) {
    return <JsonBlock title="Action payload unavailable" value={undefined} />;
  }

  if (action.type === "apply_jobdef_patch") {
    return <JobDefPatchPreview action={action} />;
  }
  if (action.type === "rerun_with_params") {
    return <RerunPreview action={action} />;
  }
  if (action.type === "skip_task") {
    return (
      <SkipTaskPreview
        action={action}
        jobId={jobId}
        incidentTaskName={incidentTaskName}
      />
    );
  }

  return <JsonBlock title="Action parameters" value={action.params} />;
}

function JobDefPatchPreview({ action }: { action: AgentAction }) {
  const diff = extractPatchText(action.params) ?? extractPatchText(action.result);
  return (
    <div className="space-y-2">
      <PreviewTitle icon={<FileCode2 className="h-3.5 w-3.5" />}>
        Job definition patch
      </PreviewTitle>
      {diff ? (
        <pre className="max-h-80 overflow-auto rounded-md border border-border/60 bg-void p-3 font-mono text-xs leading-relaxed">
          {diff.split("\n").map((line, index) => (
            <div
              key={`${index}-${line}`}
              className={cn(
                "min-w-max whitespace-pre-wrap",
                line.startsWith("+") ? "text-success" : line.startsWith("-") ? "text-danger" : "text-text-3",
              )}
            >
              {line || " "}
            </div>
          ))}
        </pre>
      ) : (
        <JsonBlock title="Patch parameters" value={action.params} />
      )}
    </div>
  );
}

function RerunPreview({ action }: { action: AgentAction }) {
  const params = recordFromUnknown(action.params);
  const overrides = recordFromUnknown(
    params.overrides ?? params.params ?? params.set ?? params.param_overrides,
  );
  const cost =
    stringField(params, "cost", "estimated_cost", "recompute_cost") ??
    stringField(action.result, "cost", "estimated_cost", "recompute_cost");
  const tasks =
    numberField(params, "tasks", "estimated_tasks", "recompute_tasks") ??
    numberField(action.result, "tasks", "estimated_tasks", "recompute_tasks");

  return (
    <div className="space-y-3">
      <PreviewTitle icon={<RotateCcw className="h-3.5 w-3.5" />}>
        Rerun with parameter overrides
      </PreviewTitle>
      <div className="rounded-md border border-border/60 bg-background/50">
        {Object.keys(overrides).length > 0 ? (
          Object.entries(overrides).map(([key, value]) => (
            <div key={key} className="grid gap-2 border-b border-border/40 px-3 py-2 text-xs last:border-b-0 md:grid-cols-[160px_minmax(0,1fr)]">
              <span className="font-mono text-text-3">{key}</span>
              <span className="font-mono text-text-1">{String(value)}</span>
            </div>
          ))
        ) : (
          <div className="px-3 py-2 text-xs text-text-3">No overrides reported.</div>
        )}
      </div>
      <div className="flex flex-wrap gap-2 text-xs">
        <Badge variant="outline">new run</Badge>
        <Badge variant="outline">cache identity changes</Badge>
        {tasks !== undefined ? <Badge variant="outline">{tasks} tasks recomputed</Badge> : null}
        {cost ? <Badge variant="outline">cost {cost}</Badge> : null}
      </div>
    </div>
  );
}

function SkipTaskPreview({
  action,
  jobId,
  incidentTaskName,
}: {
  action: AgentAction;
  jobId: string;
  incidentTaskName?: string;
}) {
  const target =
    stringField(action.params, "task", "task_name", "taskName") ??
    incidentTaskName;
  const dagQuery = useQuery({
    queryKey: ["job", jobId, "dag", "skip-preview", target],
    queryFn: async () => ({
      dag: await api.getJobDAG(jobId),
      tasks: await api.getJobTasks(jobId),
    }),
    enabled: Boolean(jobId && target),
  });

  const downstream = useMemo(() => {
    if (!target || !dagQuery.data) return [];
    return downstreamTaskNames(target, dagQuery.data.dag.nodes, dagQuery.data.dag.edges, dagQuery.data.tasks);
  }, [dagQuery.data, target]);

  return (
    <div className="space-y-3">
      <PreviewTitle icon={<SkipForward className="h-3.5 w-3.5" />}>
        Skip task DAG impact
      </PreviewTitle>
      <div className="rounded-md border border-border/60 bg-background/50 p-3">
        <div className="mb-2 flex flex-wrap gap-2">
          <Badge variant="destructive">{target ?? "unknown task"} skipped</Badge>
          <Badge variant="outline">{downstream.length} downstream reachable</Badge>
        </div>
        {dagQuery.isLoading ? (
          <div className="text-xs text-text-3">Loading DAG impact...</div>
        ) : downstream.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {downstream.map((name) => (
              <Badge key={name} variant="outline" className="text-[10px]">
                <GitBranch className="h-3 w-3" />
                {name}
              </Badge>
            ))}
          </div>
        ) : (
          <div className="text-xs text-text-3">No downstream tasks reported by the DAG.</div>
        )}
      </div>
    </div>
  );
}

function JsonBlock({ title, value }: { title: string; value: unknown }) {
  return (
    <div className="space-y-2">
      <PreviewTitle>{title}</PreviewTitle>
      <pre className="max-h-72 overflow-auto rounded-md border border-border/60 bg-void p-3 font-mono text-xs text-text-3">
        {formatJson(value)}
      </pre>
    </div>
  );
}

function PreviewTitle({
  children,
  icon,
}: {
  children: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-wide text-text-3">
      {icon}
      {children}
    </div>
  );
}

function DecisionIcon({ type }: { type: string }) {
  if (type === "apply_jobdef_patch") return <FileCode2 className="h-4 w-4 text-gold" />;
  if (type === "rerun_with_params") return <RotateCcw className="h-4 w-4 text-gold" />;
  if (type === "skip_task") return <SkipForward className="h-4 w-4 text-gold" />;
  return <CheckCircle2 className="h-4 w-4 text-gold" />;
}

function extractPatchText(value: unknown): string | undefined {
  return stringField(value, "diff", "yaml_diff", "patch", "jobdef_patch", "proposed_yaml", "yaml");
}

function downstreamTaskNames(
  target: string,
  nodes: DAGNode[],
  edges: Array<{ from: string; to: string }>,
  tasks: JobTask[],
): string[] {
  const idByName = new Map(tasks.map((task) => [task.name, task.id]));
  const nameById = new Map(tasks.map((task) => [task.id, task.name]));
  nodes.forEach((node) => {
    if (!nameById.has(node.id)) {
      nameById.set(node.id, node.id);
    }
  });

  const targetId = idByName.get(target) ?? target;
  const adjacency = new Map<string, string[]>();
  edges.forEach((edge) => {
    const list = adjacency.get(edge.from) ?? [];
    list.push(edge.to);
    adjacency.set(edge.from, list);
  });

  const seen = new Set<string>();
  const queue = [...(adjacency.get(targetId) ?? [])];
  while (queue.length > 0) {
    const next = queue.shift();
    if (!next || seen.has(next)) continue;
    seen.add(next);
    queue.push(...(adjacency.get(next) ?? []));
  }

  return [...seen].map((id) => nameById.get(id) ?? id);
}

function invalidateIncidentQueries(queryClient: ReturnType<typeof useQueryClient>, incidentId: string) {
  queryClient.invalidateQueries({ queryKey: ["incidents"] });
  queryClient.invalidateQueries({ queryKey: ["incident", incidentId] });
}
