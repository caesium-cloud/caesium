import type { JobTask, TaskRun } from "@/lib/api";
import { formatDurationNs, formatKeyValueMap } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { isTaskCached } from "./cache-utils";

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
  const cached = isTaskCached(runTask);

  const content = (
    <div className="grid gap-3 text-sm md:grid-cols-2">
        <MetadataRow label="Task ID" value={task?.id ?? runTask?.task_id ?? "Unknown"} mono />
        <MetadataRow label="Status" value={runTask?.status ?? "pending"} badge badgeVariant={cached ? "cached" : "outline"} />
        {cached ? <MetadataRow label="Cache" value="Served from cache" badge badgeVariant="cached" className="md:col-span-2" /> : null}
        {resolvedType !== 'task' && <MetadataRow label="Type" value={resolvedType} badge />}
        <MetadataRow label="Trigger Rule" value={task?.trigger_rule ?? "all_success"} mono />
        <MetadataRow label="Attempts" value={formatAttempts(runTask)} mono />
        <MetadataRow label="Retries" value={String(task?.retries ?? 0)} mono />
        <MetadataRow label="Retry Delay" value={formatDurationNs(task?.retry_delay)} mono />
        <MetadataRow label="Backoff" value={task?.retry_backoff ? "Enabled" : "Disabled"} />
        <MetadataRow label="Claimed By" value={runTask?.claimed_by || "Unclaimed"} mono />
        <MetadataRow label="Node Selector" value={formatKeyValueMap((task?.node_selector || runTask?.node_selector) as Record<string, unknown>)} />
        <MetadataRow label="Outstanding Predecessors" value={String(runTask?.outstanding_predecessors ?? 0)} mono />
        {runTask?.cache_origin_run_id ? <MetadataRow label="Source Run" value={runTask.cache_origin_run_id} mono /> : null}
        {runTask?.cache_created_at ? <MetadataRow label="Cached At" value={new Date(runTask.cache_created_at).toLocaleString()} /> : null}
        {runTask?.cache_expires_at ? <MetadataRow label="Cache Expires" value={new Date(runTask.cache_expires_at).toLocaleString()} /> : null}
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
        {task?.output_schema ? (
          <OutputSchemaSection schema={task.output_schema} />
        ) : null}
        {runTask?.schema_violations && runTask.schema_violations.length > 0 ? (
          <SchemaViolationsSection violations={runTask.schema_violations} />
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
  badgeVariant?: "cached" | "outline";
  className?: string;
}

function MetadataRow({ label, value, mono = false, badge = false, badgeVariant = "outline", className }: MetadataRowProps) {
  return (
    <div className={className}>
      <div className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</div>
      {badge ? (
        <Badge variant={badgeVariant} className="font-medium">
          {value}
        </Badge>
      ) : (
        <div className={mono ? "font-mono text-xs text-foreground" : "text-foreground"}>{value}</div>
      )}
    </div>
  );
}

interface OutputSchemaSectionProps {
  schema: Record<string, unknown>;
}

function OutputSchemaSection({ schema }: OutputSchemaSectionProps) {
  const properties = schema.properties as Record<string, Record<string, unknown>> | undefined;
  const required = schema.required as string[] | undefined;

  return (
    <div className="md:col-span-2">
      <div className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">Output Schema</div>
      <div className="rounded-md border bg-muted/50 p-2">
        {properties ? (
          Object.entries(properties).map(([key, prop]) => {
            const type = (prop as Record<string, unknown>).type as string | undefined;
            const isRequired = required?.includes(key);
            return (
              <div key={key} className="flex gap-2 font-mono text-xs">
                <span className="font-semibold text-muted-foreground">{key}:</span>
                <span className="text-foreground">{type ?? "any"}</span>
                {isRequired && (
                  <Badge variant="outline" className="h-4 px-1 text-[10px] font-normal">required</Badge>
                )}
              </div>
            );
          })
        ) : (
          <span className="font-mono text-xs text-muted-foreground">schema defined</span>
        )}
      </div>
    </div>
  );
}

interface SchemaViolationsSectionProps {
  violations: Array<{ key: string; message: string }>;
}

function SchemaViolationsSection({ violations }: SchemaViolationsSectionProps) {
  return (
    <div className="md:col-span-2">
      <div className="mb-1 text-xs font-medium uppercase tracking-wide text-amber-600">Schema Violations</div>
      <div className="rounded-md border border-amber-200 bg-amber-50 p-2 dark:border-amber-900 dark:bg-amber-950/30">
        {violations.map((v, i) => (
          <div key={i} className="flex gap-2 font-mono text-xs">
            {v.key && (
              <span className="font-semibold text-amber-700 dark:text-amber-400">{v.key}:</span>
            )}
            <span className="text-amber-800 dark:text-amber-300">{v.message}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
