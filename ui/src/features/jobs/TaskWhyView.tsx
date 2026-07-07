import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { AlertTriangle, Archive, Info } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import {
  ApiError,
  api,
  type BlobDiff,
  type FieldChange,
  type WhyBaseline,
  type WhyExplanation,
  type WhyTrigger,
  type WhyVerdict,
} from "@/lib/api";
import { cn, formatUTCTimestamp, shortId } from "@/lib/utils";

interface TaskWhyViewProps {
  jobId: string;
  runId: string;
  taskName?: string;
}

export function TaskWhyView({ jobId, runId, taskName }: TaskWhyViewProps) {
  const taskRef = taskName?.trim() ?? "";
  const whyQuery = useQuery({
    queryKey: ["job", jobId, "runs", runId, "why", taskRef],
    queryFn: () => api.getTaskWhy(jobId, runId, taskRef),
    enabled: Boolean(jobId && runId && taskRef),
    staleTime: 15_000,
  });

  return (
    <section
      data-testid="task-why-container"
      className="rounded-lg border border-border/60 bg-muted/30 p-3"
    >
      <div className="mb-3 flex items-center gap-2">
        <Info className="h-4 w-4 text-primary" />
        <div>
          <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Why this status?
          </div>
          <div className="text-xs text-text-3">
            Cache, hash input, trigger, and baseline causation from the run record.
          </div>
        </div>
      </div>

      {!taskRef ? (
        <EmptyState
          title="Task name unavailable"
          subtitle="The why endpoint explains a task by its step name."
          icon={<AlertTriangle className="h-8 w-8 text-warning" />}
          className="py-5"
        />
      ) : whyQuery.isLoading ? (
        <WhySkeleton />
      ) : whyQuery.error ? (
        <WhyError error={whyQuery.error} />
      ) : whyQuery.data ? (
        <WhyContent explanation={whyQuery.data} />
      ) : (
        <EmptyState
          title="No explanation returned"
          subtitle="The server returned an empty why response for this task."
          icon={<Info className="h-8 w-8 text-muted-foreground" />}
          className="py-5"
        />
      )}
    </section>
  );
}

function WhySkeleton() {
  return (
    <div className="space-y-3">
      <Skeleton className="h-8 w-44" />
      <Skeleton className="h-16 w-full" />
      <Skeleton className="h-20 w-full" />
    </div>
  );
}

function WhyError({ error }: { error: Error }) {
  const isAccessError = error instanceof ApiError && error.kind === "insufficient_access";
  return (
    <div className="rounded-md border border-warning/25 bg-warning/10 p-3 text-xs text-warning">
      <div className="flex items-start gap-2">
        <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <div>
          <div className="font-semibold">
            {isAccessError
              ? "Insufficient access for task explanation"
              : "Could not load task explanation"}
          </div>
          <div className="mt-1 text-warning/80">
            {isAccessError
              ? "Your current role cannot read this why endpoint."
              : error.message}
          </div>
        </div>
      </div>
    </div>
  );
}

function WhyContent({ explanation }: { explanation: WhyExplanation }) {
  const discriminatingChange = getDiscriminatingChange(explanation);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={verdictBadgeVariant(explanation.verdict)}>
          {formatVerdict(explanation.verdict)}
        </Badge>
        <span className="rounded border border-border/60 px-1.5 py-0.5 font-mono text-[10px] text-text-3">
          {explanation.status}
        </span>
        <span className="rounded border border-border/60 px-1.5 py-0.5 text-[10px] text-text-3">
          cache {explanation.cacheEnabled ? "enabled" : "disabled"}
        </span>
      </div>

      <div className="rounded-md border border-border/50 bg-background/40 p-3">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          Server summary
        </div>
        <p className="mt-1 text-xs leading-relaxed text-foreground">
          {explanation.summary}
        </p>
      </div>

      <DiscriminatingField
        explanation={explanation}
        discriminatingChange={discriminatingChange}
      />
      <DiffDetails
        diff={explanation.diff}
        skipFirstChange={Boolean(discriminatingChange)}
      />
      <BaselineDetails
        baseline={explanation.baseline}
        jobId={explanation.jobId}
        isCacheHit={explanation.verdict === "CACHE_HIT"}
      />
      <TriggerDetails trigger={explanation.trigger} />
      <HashDetails explanation={explanation} />
    </div>
  );
}

function getDiscriminatingChange(explanation: WhyExplanation) {
  if (explanation.verdict !== "CACHE_MISS") {
    return undefined;
  }
  return explanation.diff?.changes?.[0];
}

function DiscriminatingField({
  explanation,
  discriminatingChange,
}: {
  explanation: WhyExplanation;
  discriminatingChange?: FieldChange;
}) {
  if (discriminatingChange) {
    return (
      <div>
        <SectionLabel>Discriminating hash input</SectionLabel>
        <FieldChangeRow change={discriminatingChange} highlighted />
      </div>
    );
  }

  if (explanation.diff?.hashEqual) {
    return (
      <div>
        <SectionLabel>Discriminating hash input</SectionLabel>
        <div
          data-testid="task-why-discriminating-field"
          className="rounded-md border border-cached/30 bg-cached/10 p-3"
        >
          <div className="flex items-center gap-2">
            <Archive className="h-3.5 w-3.5 text-cached" />
            <span className="font-mono text-xs text-cached">hashEqual=true</span>
          </div>
          <div className="mt-1 text-xs text-cached/90">
            The backend reported identical subject and baseline hash inputs.
          </div>
        </div>
      </div>
    );
  }

  if (explanation.verdict === "CACHE_DISABLED") {
    return (
      <div>
        <SectionLabel>Discriminating hash input</SectionLabel>
        <div
          data-testid="task-why-discriminating-field"
          className="rounded-md border border-border/50 bg-background/40 p-3 text-xs text-text-3"
        >
          cacheEnabled=false
        </div>
      </div>
    );
  }

  return null;
}

function DiffDetails({
  diff,
  skipFirstChange = false,
}: {
  diff?: BlobDiff;
  skipFirstChange?: boolean;
}) {
  if (!diff) {
    return null;
  }

  const changes = diff.changes ?? [];
  const visibleChanges = changes
    .map((change, index) => ({ change, index }))
    .slice(skipFirstChange ? 1 : 0);
  return (
    <div className="space-y-2">
      {diff.degraded ? (
        <div
          data-testid="task-why-degraded-reason"
          className="rounded-md border border-warning/25 bg-warning/10 p-3 text-xs text-warning"
        >
          <div className="font-semibold">Field-level diff unavailable</div>
          <div className="mt-1 text-warning/85">{diff.degraded}</div>
        </div>
      ) : null}

      {visibleChanges.length > 0 ? (
        <div>
          <SectionLabel>
            {skipFirstChange ? "Additional changed fields" : "Changed fields"}
          </SectionLabel>
          <div className="space-y-2">
            {visibleChanges.map(({ change, index }) => (
              <FieldChangeRow
                key={`${change.field}:${index}`}
                change={change}
              />
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function BaselineDetails({
  baseline,
  jobId,
  isCacheHit,
}: {
  baseline?: WhyBaseline | null;
  jobId: string;
  isCacheHit: boolean;
}) {
  return (
    <div className="rounded-md border border-border/50 bg-background/40 p-3">
      <SectionLabel>Baseline</SectionLabel>
      <div className="grid gap-2 text-xs">
        <DetailRow label="Kind" value={baseline?.kind || "none"} mono />
        <div className="grid grid-cols-[96px_1fr] gap-2">
          <span className="text-muted-foreground">Run</span>
          {baseline?.runId ? (
            <Link
              to="/jobs/$jobId/runs/$runId"
              params={{ jobId, runId: baseline.runId }}
              data-testid={isCacheHit ? "task-why-source-run-link" : undefined}
              className="font-mono text-primary hover:underline"
            >
              {isCacheHit ? "Source run " : "Baseline run "}
              {shortId(baseline.runId)}
              {isCacheHit && baseline.taskRunId ? (
                <span className="text-primary/80">
                  {" "}
                  / task {shortId(baseline.taskRunId)}
                </span>
              ) : null}
            </Link>
          ) : (
            <span className="text-text-3">none</span>
          )}
        </div>
        <DetailRow
          label="Task run"
          value={baseline?.taskRunId ? shortId(baseline.taskRunId) : "none"}
          mono
        />
        <DetailRow
          label="Started"
          value={baseline?.startedAt ? formatDateTime(baseline.startedAt) : "unknown"}
        />
      </div>
    </div>
  );
}

function TriggerDetails({ trigger }: { trigger?: WhyTrigger | null }) {
  const params = Object.entries(trigger?.params ?? {});
  return (
    <div className="rounded-md border border-border/50 bg-background/40 p-3">
      <SectionLabel>Trigger causation</SectionLabel>
      <div className="grid gap-2 text-xs">
        <DetailRow label="Type" value={trigger?.type || "unknown"} mono />
        <DetailRow label="Alias" value={trigger?.alias || "none"} mono />
        <DetailRow
          label="Fired"
          value={trigger?.firedAt ? formatDateTime(trigger.firedAt) : "unknown"}
        />
        <div className="grid grid-cols-[96px_1fr] gap-2">
          <span className="text-muted-foreground">Params</span>
          {params.length > 0 ? (
            <div className="space-y-1">
              {params.map(([key, value]) => (
                <div key={key} className="font-mono text-text-2">
                  {key}={value}
                </div>
              ))}
            </div>
          ) : (
            <span className="text-text-3">none</span>
          )}
        </div>
      </div>
    </div>
  );
}

function HashDetails({ explanation }: { explanation: WhyExplanation }) {
  return (
    <div className="rounded-md border border-border/50 bg-background/40 p-3">
      <SectionLabel>Hashes</SectionLabel>
      <div className="grid gap-2 text-xs">
        <DetailRow label="Task hash" value={explanation.hash || "none"} mono wrap />
        <DetailRow label="Subject" value={explanation.diff?.subjectHash || "none"} mono wrap />
        <DetailRow label="Baseline" value={explanation.diff?.baselineHash || "none"} mono wrap />
      </div>
    </div>
  );
}

function FieldChangeRow({
  change,
  highlighted = false,
}: {
  change: FieldChange;
  highlighted?: boolean;
}) {
  return (
    <div
      data-testid={highlighted ? "task-why-discriminating-field" : undefined}
      className={cn(
        "rounded-md border p-3",
        highlighted
          ? "border-primary/30 bg-primary/10"
          : "border-border/50 bg-background/40",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs font-semibold text-foreground">
          {change.field}
        </span>
        <span className="rounded border border-border/60 px-1.5 py-0.5 text-[10px] text-text-3">
          {change.kind}
        </span>
        {change.redacted ? (
          <span className="rounded border border-warning/30 px-1.5 py-0.5 text-[10px] text-warning">
            redacted digest
          </span>
        ) : null}
      </div>
      <div className="mt-2 text-xs text-text-2">
        {describeChange(change)}
      </div>
    </div>
  );
}

function DetailRow({
  label,
  value,
  mono = false,
  wrap = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
  wrap?: boolean;
}) {
  return (
    <div className="grid grid-cols-[96px_1fr] gap-2">
      <span className="text-muted-foreground">{label}</span>
      <span className={cn("text-text-2", mono && "font-mono", wrap && "break-all")}>
        {value}
      </span>
    </div>
  );
}

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="mb-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
      {children}
    </div>
  );
}

function describeChange(change: FieldChange) {
  if (change.added) {
    return `Added ${formatFieldValue(change.after)}`;
  }
  if (change.removed) {
    return `Removed ${formatFieldValue(change.before)}`;
  }
  if (change.kind === "structural") {
    return "Structural value changed";
  }
  return `Before ${formatFieldValue(change.before)}; after ${formatFieldValue(change.after)}`;
}

function formatFieldValue(value?: string) {
  return value && value.trim() ? value : "empty";
}

function verdictBadgeVariant(
  verdict: WhyVerdict,
): "default" | "secondary" | "outline" | "cached" {
  switch (verdict) {
    case "CACHE_HIT":
      return "cached";
    case "CACHE_MISS":
      return "outline";
    case "CACHE_DISABLED":
      return "secondary";
    case "UNKNOWN":
      return "default";
    default:
      return assertNever(verdict);
  }
}

function assertNever(value: never): never {
  throw new Error(`Unhandled why verdict: ${value}`);
}

function formatVerdict(verdict: WhyVerdict) {
  return verdict.replaceAll("_", " ");
}

function formatDateTime(value: string) {
  return formatUTCTimestamp(value, value);
}
