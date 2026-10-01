import { MetadataValue } from "@/components/ui/metadata-value";
import { IdChip } from "@/components/ui/id-chip";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";

import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/ui/status-badge";
import { api, type FieldChange, type RunDiffTask, type RunDiffVerdict, type TaskRun, type WhyTrigger } from "@/lib/api";
import { cn, formatDurationNs, formatUTCTime, formatUTCTimestamp } from "@/lib/utils";

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

  const { data: leftRun } = useQuery({ queryKey: ["job", jobId, "runs", leftRunId], queryFn: () => api.getJobRun(jobId, leftRunId), enabled: hasComparison });
  const { data: rightRun } = useQuery({ queryKey: ["job", jobId, "runs", rightRunId], queryFn: () => api.getJobRun(jobId, rightRunId!), enabled: hasComparison });

  if (!rightRunId) {
    return (
      <div className="space-y-5" data-testid="run-diff-container">
        <DiffBreadcrumb jobId={jobId} runId={leftRunId} />
        <EmptyState
          title="Choose a comparison run"
          subtitle="Open a run detail page and use Compare to run… to select the other side."
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
        />
      </div>
    );
  }

  return (
    <div className="space-y-5" data-testid="run-diff-container">
      <DiffBreadcrumb jobId={jobId} runId={leftRunId} />

      <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
        <div>
          <div className="text-[11px] font-bold lowercase text-text-3 mb-1">
            Run diff
          </div>
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="text-2xl font-bold lowercase text-text-1">
              diff
            </h1>
            <StatusBadge status={diff.leftStatus} size="sm" label={`left ${diff.leftStatus}`} />
            <StatusBadge status={diff.rightStatus} size="sm" label={`right ${diff.rightStatus}`} />
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <IdChip value={diff.jobId} label="job id" />
            <span className="inline-block w-3" aria-hidden="true" />
            <span>{formatUTCTimestamp(diff.generatedAt, diff.generatedAt)}</span>
          </div>
        </div>
        <div className="flex w-fit max-w-full items-start gap-1.5 rounded-md border border-primary/30 bg-primary/5 px-2.5 py-1.5 text-xs font-normal text-primary">

          <span>Value diffs -&gt; dbt/Datafold; Caesium shows cache-bust attribution only.</span>
        </div>
      </div>

      <div className="grid grid-cols-2 gap-7 border-b border-border pb-3">
        {[{ id: leftRunId, run: leftRun }, { id: rightRunId, run: rightRun }].map(side => <div key={side.id} className="space-y-2">
          <div className="text-sm text-text-2">run started {formatUTCTime(side.run?.started_at, { fallback: "unknown" })}</div>
          <IdChip value={side.id} label="run id" />
        </div>)}
      </div>
      <section className="border-b border-border py-3">
        <div className="pb-3">
          <h3 className="text-sm">Comparison Inputs</h3>
        </div>
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Left Run ID" idChip value={diff.leftRunId} mono />
            <MetadataCell label="Right Run ID" idChip value={diff.rightRunId} mono />
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
        </div>
      </section>

      <div className="space-y-3">
        {diff.tasks.length > 0 ? (
          diff.tasks.map((task) => <TaskDiffRow key={task.taskName} task={task} leftTask={leftRun?.tasks?.find(runTask => runTask.id === task.leftTaskRunId)} rightTask={rightRun?.tasks?.find(runTask => runTask.id === task.rightTaskRunId)} />)
        ) : (
          <EmptyState
            title="No paired tasks"
            subtitle="The compared runs did not have terminal task runs with matching names."
          />
        )}
      </div>
    </div>
  );
}

export function TaskDiffRow({ task, leftTask, rightTask }: { task: RunDiffTask; leftTask?: TaskRun; rightTask?: TaskRun }) {
  const headline = headlineChange(task);
  const taskTestId = `run-diff-task-${testIdSlug(task.taskName)}`;
  const isCacheHit = task.verdict === "WOULD_CACHE_HIT";

  return (
    <section
      data-testid="run-diff-task-row"
      data-task-name={task.taskName}
      className="overflow-hidden"
    >
      <div className="p-0">
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
                <h2 className="text-sm font-bold text-text-1">{task.taskName}</h2>
                <span data-testid="run-diff-verdict">
                  <StatusBadge
                    status={verdictStatus(task.verdict)}
                    label={verdictLabel(task.verdict)}
                    size="sm"
                  />
                </span>
                {isCacheHit ? (
                  <Badge
                    data-testid="run-diff-cache-hit-marker"
                    variant="cached"
                    className="text-[11px]"
                  >
                    WOULD_CACHE_HIT
                  </Badge>
                ) : null}
              </div>
              <div
                data-testid="run-diff-discriminating-field"
                className="mt-2 text-xs text-text-3"
              >
                <span className="font-bold text-text-2">{headline.label}</span>
                {headline.detail ? <span className="ml-1">{headline.detail}</span> : null}
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <StatusBadge status={task.leftStatus} size="sm" label={`left ${task.leftStatus}`} /><span className="tabular-nums text-text-3">{taskDuration(leftTask)}</span>
              <StatusBadge status={task.rightStatus} size="sm" label={`right ${task.rightStatus}`} /><span className="tabular-nums text-text-3">{taskDuration(rightTask)}</span>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-2 gap-3 md:grid-cols-4">
            <MetadataCell label="Left Task Run ID" idChip value={task.leftTaskRunId} mono />
            <MetadataCell label="Right Task Run ID" idChip value={task.rightTaskRunId} mono />
            <MetadataCell label="Left Task ID" idChip value={task.leftTaskId} mono />
            <MetadataCell label="Right Task ID" idChip value={task.rightTaskId} mono />
            <MetadataCell label="Left Attempt" value={String(task.leftAttempt)} mono />
            <MetadataCell label="Right Attempt" value={String(task.rightAttempt)} mono />
            <MetadataCell label="Hash Equal" value={String(task.hashEqual)} mono />
            {!task.hashEqual ? <><MetadataCell label="Left Hash" idChip value={task.leftHash || "None"} mono /><MetadataCell label="Right Hash" idChip value={task.rightHash || "None"} mono /></> : null}
            {task.degraded ? <MetadataCell label="Degraded" value={task.degraded} /> : null}
          </div>

          {task.changes && task.changes.length > 0 ? (
            <div className="mt-4">
              <ChangeList title={`${task.taskName} Changes`} changes={task.changes} />
            </div>
          ) : null}
        </div>
      </div>
    </section>
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

        Run
      </Link>
      <span className="text-text-3">/</span>
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
      <div className="mb-1.5 text-xs font-normal lowercase text-muted-foreground">
        {title}
      </div>
      <div className="space-y-1.5 rounded-md border bg-muted/35 p-3">
        {changes.map((change) => (
          <div
            key={`${change.field}:${change.before ?? ""}:${change.after ?? ""}`}
            className="grid gap-1 text-xs md:grid-cols-[minmax(160px,0.8fr)_minmax(0,1.2fr)]"
          >
            <span className="text-gold">{change.field}</span>
            <span className="min-w-0 break-all text-text-3">
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
      <div className="mb-1.5 text-xs font-normal lowercase text-muted-foreground">
        {title}
      </div>
      <div className="flex flex-wrap gap-1.5">
        {names.map((name) => (
          <Badge key={name} variant="outline" className="text-[11px]">
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
  idChip,
}: {
  label: string;
  value: string;
  mono?: boolean;
  idChip?: boolean;
}) {
  return (
    <div className="min-w-0">
      <div className="mb-0.5 text-[11px] font-normal lowercase text-muted-foreground">
        {label}
      </div>
      <div className={cn("break-all text-xs text-foreground", mono && "")}><MetadataValue value={value} label={label} idChip={idChip} /></div>
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

function formatTrigger(trigger?: WhyTrigger | null): string {
  if (!trigger) {
    return "None";
  }
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

function verdictLabel(verdict: RunDiffVerdict): string {
  switch (verdict) {
    case "WOULD_CACHE_HIT":
      return "Cache reusable";
    case "RERAN":
      return "Reran";
    case "DEGRADED":
      return "Degraded";
  }
}

function testIdSlug(value: string): string {
  const slug = value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
  return slug || "task";
}

function taskDuration(task?: TaskRun): string {
  if (!task?.started_at || !task.completed_at) return "duration unknown";
  const elapsed = Date.parse(task.completed_at) - Date.parse(task.started_at);
  return Number.isFinite(elapsed) && elapsed >= 0 ? formatDurationNs(elapsed * 1_000_000) : "duration unknown";
}
