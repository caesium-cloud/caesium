import { Link, useParams, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeft, ArrowLeftRight, Info } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/ui/status-badge";
import { api, type FieldChange, type RunDiffTask, type RunDiffVerdict, type WhyTrigger } from "@/lib/api";
import { cn, shortId } from "@/lib/utils";

export interface RunDiffViewProps {
  jobId: string;
  leftRunId: string;
  rightRunId?: string;
}

type RunDiffSearch = {
  to?: string;
};

export function RunDiffRoutePage() {
  const { jobId, runId } = useParams({ strict: false }) as { jobId: string; runId: string };
  const search = useSearch({ strict: false }) as RunDiffSearch;

  return <RunDiffView jobId={jobId} leftRunId={runId} rightRunId={search.to} />;
}

export function RunDiffView({ jobId, leftRunId, rightRunId }: RunDiffViewProps) {
  const hasComparison = Boolean(jobId && leftRunId && rightRunId);
  const { data: diff, isLoading, error } = useQuery({
    queryKey: ["job", jobId, "runs", "diff", leftRunId, rightRunId],
    queryFn: () => api.getRunDiff(jobId, leftRunId, rightRunId ?? ""),
    enabled: hasComparison,
  });

  if (!rightRunId) {
    return (
      <div className="space-y-5" data-testid="run-diff-container">
        <DiffBreadcrumb jobId={jobId} runId={leftRunId} />
        <EmptyState
          title="Choose a comparison run"
          subtitle="Open a run detail page and use Compare to run… to select the other side."
          icon={<ArrowLeftRight className="h-12 w-12 text-text-3" />}
        />
      </div>
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-4 p-8" data-testid="run-diff-container">
        <Skeleton className="h-8 w-[220px]" />
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-5" data-testid="run-diff-container">
        <DiffBreadcrumb jobId={jobId} runId={leftRunId} />
        <EmptyState
          title="Run diff unavailable"
          subtitle={error instanceof Error ? error.message : "The run diff endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      </div>
    );
  }

  if (!diff) {
    return (
      <div className="space-y-5" data-testid="run-diff-container">
        <DiffBreadcrumb jobId={jobId} runId={leftRunId} />
        <EmptyState
          title="No diff data"
          subtitle="The run diff endpoint returned no tasks for this comparison."
          icon={<ArrowLeftRight className="h-12 w-12 text-text-3" />}
        />
      </div>
    );
  }

  return (
    <div className="space-y-5" data-testid="run-diff-container">
      <DiffBreadcrumb jobId={jobId} runId={leftRunId} />

      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3 mb-1">
            Run diff
          </div>
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="text-xl font-semibold text-text-1 font-mono tracking-tight">
              {shortId(diff.leftRunId)} vs {shortId(diff.rightRunId)}
            </h1>
            <StatusBadge status={diff.leftStatus} size="sm" label={`left ${diff.leftStatus}`} />
            <StatusBadge status={diff.rightStatus} size="sm" label={`right ${diff.rightStatus}`} />
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <span className="font-mono text-text-4 text-[10px]">{diff.jobId}</span>
            <span className="text-text-4">·</span>
            <span>{new Date(diff.generatedAt).toLocaleString()}</span>
          </div>
        </div>
        <div className="flex w-fit max-w-full items-start gap-1.5 rounded-md border border-primary/30 bg-primary/5 px-2.5 py-1.5 text-xs font-medium text-primary">
          <Info className="h-3.5 w-3.5" />
          <span>Value diffs -&gt; dbt/Datafold; Caesium shows cache-bust attribution only.</span>
        </div>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm">Comparison Inputs</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Left Run ID" value={diff.leftRunId} mono />
            <MetadataCell label="Right Run ID" value={diff.rightRunId} mono />
            <MetadataCell label="Left Trigger" value={formatTrigger(diff.leftTrigger)} mono />
            <MetadataCell label="Right Trigger" value={formatTrigger(diff.rightTrigger)} mono />
          </div>
          <ChangeList title="Trigger Changes" changes={diff.triggerChanges ?? []} />
          <ChangeList title="Run Parameter Changes" changes={diff.paramChanges ?? []} />
          {diff.tasksAdded && diff.tasksAdded.length > 0 ? (
            <NameList title="Tasks Added" names={diff.tasksAdded} />
          ) : null}
          {diff.tasksRemoved && diff.tasksRemoved.length > 0 ? (
            <NameList title="Tasks Removed" names={diff.tasksRemoved} />
          ) : null}
        </CardContent>
      </Card>

      <div className="space-y-3">
        {diff.tasks.length > 0 ? (
          diff.tasks.map((task) => <TaskDiffRow key={task.taskName} task={task} />)
        ) : (
          <EmptyState
            title="No paired tasks"
            subtitle="The compared runs did not have terminal task runs with matching names."
            icon={<ArrowLeftRight className="h-12 w-12 text-text-3" />}
          />
        )}
      </div>
    </div>
  );
}

export function TaskDiffRow({ task }: { task: RunDiffTask }) {
  const headline = headlineChange(task);
  const taskTestId = `run-diff-task-${testIdSlug(task.taskName)}`;
  const isCacheHit = task.verdict === "WOULD_CACHE_HIT";

  return (
    <Card
      data-testid="run-diff-task-row"
      data-task-name={task.taskName}
      className="overflow-hidden"
    >
      <CardContent className="p-0">
        <div
          className={cn(
            "border-l-2 px-4 py-3",
            task.verdict === "WOULD_CACHE_HIT"
              ? "border-cached"
              : task.verdict === "RERAN"
                ? "border-running"
                : "border-danger",
          )}
          data-testid={taskTestId}
        >
          <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h2 className="font-mono text-sm font-semibold text-text-1">{task.taskName}</h2>
                <span data-testid="run-diff-verdict">
                  <StatusBadge
                    status={verdictStatus(task.verdict)}
                    label={task.verdict}
                    size="sm"
                  />
                </span>
                {isCacheHit ? (
                  <Badge
                    data-testid="run-diff-cache-hit-marker"
                    variant="cached"
                    className="text-[10px]"
                  >
                    WOULD_CACHE_HIT
                  </Badge>
                ) : null}
              </div>
              <div
                data-testid="run-diff-discriminating-field"
                className="mt-2 text-xs text-text-3"
              >
                <span className="font-semibold text-text-2">{headline.label}</span>
                {headline.detail ? <span className="ml-1 font-mono">{headline.detail}</span> : null}
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <StatusBadge status={task.leftStatus} size="sm" label={`left ${task.leftStatus}`} />
              <StatusBadge status={task.rightStatus} size="sm" label={`right ${task.rightStatus}`} />
            </div>
          </div>

          <div className="mt-4 grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Left Task Run ID" value={task.leftTaskRunId} mono />
            <MetadataCell label="Right Task Run ID" value={task.rightTaskRunId} mono />
            <MetadataCell label="Left Task ID" value={task.leftTaskId} mono />
            <MetadataCell label="Right Task ID" value={task.rightTaskId} mono />
            <MetadataCell label="Left Attempt" value={String(task.leftAttempt)} mono />
            <MetadataCell label="Right Attempt" value={String(task.rightAttempt)} mono />
            <MetadataCell label="Hash Equal" value={String(task.hashEqual)} mono />
            <MetadataCell label="Left Hash" value={task.leftHash || "None"} mono />
            <MetadataCell label="Right Hash" value={task.rightHash || "None"} mono />
            {task.degraded ? <MetadataCell label="Degraded" value={task.degraded} /> : null}
          </div>

          {task.changes && task.changes.length > 0 ? (
            <div className="mt-4">
              <ChangeList title={`${task.taskName} Changes`} changes={task.changes} />
            </div>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}

function DiffBreadcrumb({ jobId, runId }: { jobId: string; runId: string }) {
  return (
    <div className="flex items-center gap-2 text-[11px] text-text-3">
      <Link
        to="/jobs/$jobId/runs/$runId"
        params={{ jobId, runId }}
        className="flex items-center gap-1 hover:text-text-2 transition-colors"
      >
        <ArrowLeft className="h-3 w-3" />
        Run
      </Link>
      <span className="text-text-4">/</span>
      <span>Diff</span>
    </div>
  );
}

function ChangeList({ title, changes }: { title: string; changes: FieldChange[] }) {
  if (changes.length === 0) {
    return null;
  }

  return (
    <div>
      <div className="mb-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </div>
      <div className="space-y-1.5 rounded-lg border bg-muted/35 p-3">
        {changes.map((change) => (
          <div
            key={`${change.field}:${change.before ?? ""}:${change.after ?? ""}`}
            className="grid gap-1 text-xs md:grid-cols-[minmax(160px,0.8fr)_minmax(0,1.2fr)]"
          >
            <span className="font-mono font-semibold text-text-2">{change.field}</span>
            <span className="min-w-0 break-all font-mono text-text-3">
              {formatFieldChange(change)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

function NameList({ title, names }: { title: string; names: string[] }) {
  return (
    <div>
      <div className="mb-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </div>
      <div className="flex flex-wrap gap-1.5">
        {names.map((name) => (
          <Badge key={name} variant="outline" className="font-mono text-[10px]">
            {name}
          </Badge>
        ))}
      </div>
    </div>
  );
}

function MetadataCell({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="min-w-0">
      <div className="mb-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div className={cn("break-all text-xs text-foreground", mono && "font-mono")}>{value}</div>
    </div>
  );
}

function headlineChange(task: RunDiffTask): { label: string; detail?: string } {
  if (task.degraded) {
    return { label: "Degraded", detail: task.degraded };
  }
  if (task.verdict === "WOULD_CACHE_HIT") {
    return { label: "Discriminating field", detail: "none; compared hashes match" };
  }

  const first = task.changes?.[0];
  if (!first) {
    return { label: "Discriminating field", detail: "hash changed; no field detail returned" };
  }

  return { label: "Discriminating field", detail: formatFieldHeadline(first) };
}

function formatFieldHeadline(change: FieldChange): string {
  const direction = formatFieldChange(change);
  return `${change.field} (${direction})`;
}

function formatFieldChange(change: FieldChange): string {
  const kind = change.kind ? `${change.kind}: ` : "";
  if (change.added) {
    return `${kind}added ${formatValue(change.after, change)}`;
  }
  if (change.removed) {
    return `${kind}removed ${formatValue(change.before, change)}`;
  }
  if (change.kind === "structural") {
    return `${kind}changed`;
  }

  return `${kind}${formatValue(change.before, change)} -> ${formatValue(change.after, change)}`;
}

function formatValue(value: string | undefined, change: FieldChange): string {
  const rendered = value || "None";
  return change.redacted ? `${rendered} (redacted)` : rendered;
}

function formatTrigger(trigger: WhyTrigger): string {
  const parts = [trigger.type, trigger.alias].filter(Boolean);
  if (trigger.firedAt) {
    parts.push(trigger.firedAt);
  }
  if (trigger.params && Object.keys(trigger.params).length > 0) {
    parts.push(
      Object.entries(trigger.params)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => `${key}=${value}`)
        .join(", "),
    );
  }
  return parts.length > 0 ? parts.join(" / ") : "None";
}

function verdictStatus(verdict: RunDiffVerdict): string {
  switch (verdict) {
    case "WOULD_CACHE_HIT":
      return "cached";
    case "RERAN":
      return "running";
    case "DEGRADED":
      return "failed";
  }
}

function testIdSlug(value: string): string {
  const slug = value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
  return slug || "task";
}
