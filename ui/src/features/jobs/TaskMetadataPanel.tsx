import type { JobTask, TaskRun } from "@/lib/api";
import { formatDurationNs, formatKeyValueMap } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

interface TaskMetadataPanelProps {
  task?: JobTask;
  runTask?: TaskRun;
  taskType?: string;
  framed?: boolean;
}

export function TaskMetadataPanel({ task, runTask, taskType, framed = true }: TaskMetadataPanelProps) {
  if (!task && !runTask) {
    return null;
  }

  const resolvedType = taskType || 'task';

  const content = (
    <div className="grid gap-3 text-sm md:grid-cols-2">
        <MetadataRow label="Task ID" value={task?.id ?? runTask?.task_id ?? "Unknown"} mono />
        <MetadataRow label="Status" value={runTask?.status ?? "pending"} badge />
        {resolvedType !== 'task' && <MetadataRow label="Type" value={resolvedType} badge />}
        <MetadataRow label="Trigger Rule" value={task?.trigger_rule ?? "all_success"} mono />
        <MetadataRow label="Attempts" value={formatAttempts(runTask)} mono />
        <MetadataRow label="Retries" value={String(task?.retries ?? 0)} mono />
        <MetadataRow label="Retry Delay" value={formatDurationNs(task?.retry_delay)} mono />
        <MetadataRow label="Backoff" value={task?.retry_backoff ? "Enabled" : "Disabled"} />
        <MetadataRow label="Claimed By" value={runTask?.claimed_by || "Unclaimed"} mono />
        <MetadataRow label="Node Selector" value={formatKeyValueMap((task?.node_selector || runTask?.node_selector) as Record<string, unknown>)} />
        <MetadataRow label="Outstanding Predecessors" value={String(runTask?.outstanding_predecessors ?? 0)} mono />
        {runTask?.error ? <MetadataRow label="Error" value={runTask.error} className="md:col-span-2" /> : null}
        {runTask?.output && Object.keys(runTask.output).length > 0 ? (
          <div className="md:col-span-2">
            <div className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">Output</div>
            <div className="rounded-md border bg-muted/50 p-2">
              {Object.entries(runTask.output).map(([key, value]) => (
                <div key={key} className="flex gap-2 font-mono text-xs">
                  <span className="font-semibold text-muted-foreground">{key}:</span>
                  <span className="text-foreground">{value}</span>
                </div>
              ))}
            </div>
          </div>
        ) : null}
      </div>
  );

  if (!framed) {
    return content;
  }

  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm">Task Metadata</CardTitle>
      </CardHeader>
      <CardContent>{content}</CardContent>
    </Card>
  );
}

function formatAttempts(runTask?: TaskRun): string {
  if (!runTask?.attempt && !runTask?.max_attempts) {
    return "1 / 1";
  }

  return `${runTask.attempt ?? 1} / ${runTask.max_attempts ?? 1}`;
}

interface MetadataRowProps {
  label: string;
  value: string;
  mono?: boolean;
  badge?: boolean;
  className?: string;
}

function MetadataRow({ label, value, mono = false, badge = false, className }: MetadataRowProps) {
  return (
    <div className={className}>
      <div className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</div>
      {badge ? (
        <Badge variant="outline" className="font-medium">
          {value}
        </Badge>
      ) : (
        <div className={mono ? "font-mono text-xs text-foreground" : "text-foreground"}>{value}</div>
      )}
    </div>
  );
}
