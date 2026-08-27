import { useMemo } from "react";
import { FileDiff } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { JobTask, TaskRun } from "@/lib/api";
import { parseProposal, type ActionCount } from "./proposal-renderers";

interface RunProposalSummaryProps {
  /** The run's tasks (`JobRun.tasks`). A task is included when its `output` carries `proposal_kind`. */
  tasks?: TaskRun[];
  /** Job-level task definitions, keyed by id, used only to resolve a human-readable row label. */
  taskDefinitions?: Record<string, JobTask>;
  /** Opens (or toggles) the task's detail panel — the same handler wired to the DAG's node click. */
  onSelectTask?: (taskId: string) => void;
}

interface TaskProposalRow {
  taskId: string;
  taskName: string;
  kind: string;
  /** Empty for a `generic`-section proposal (unregistered kind, or `proposal_summary` that failed to parse) — the row still renders, just with no counts. */
  counts: ActionCount[];
}

/**
 * Run-level aggregate of every task's proposal (spec §11 open question 3 —
 * the "more useful and more work" half, alongside the per-task `ProposalPanel`
 * that already lives in `TaskDetailPanel`). Sums per-action counts across
 * every task in the run whose output carries `proposal_kind`, and lists one
 * row per such task linking to its own task panel. Renders nothing when no
 * task in the run has a proposal — this reuses `parseProposal` rather than
 * re-implementing any of the `proposal_summary` parsing.
 */
export function RunProposalSummary({ tasks, taskDefinitions, onSelectTask }: RunProposalSummaryProps) {
  const rows = useMemo<TaskProposalRow[]>(() => {
    const result: TaskProposalRow[] = [];
    (tasks ?? []).forEach((task) => {
      const proposal = parseProposal(task.output);
      if (!proposal) {
        return;
      }
      const taskName = taskDefinitions?.[task.task_id]?.name ?? task.task_id;
      const counts = proposal.section.kind === "structured" ? proposal.section.counts : [];
      result.push({ taskId: task.task_id, taskName, kind: proposal.kind, counts });
    });
    return result;
  }, [tasks, taskDefinitions]);

  const totals = useMemo<ActionCount[]>(() => {
    const order: string[] = [];
    const sums = new Map<string, number>();
    rows.forEach((row) => {
      row.counts.forEach(({ action, count }) => {
        if (!sums.has(action)) {
          order.push(action);
          sums.set(action, 0);
        }
        sums.set(action, (sums.get(action) ?? 0) + count);
      });
    });
    return order.map((action) => ({ action, count: sums.get(action) ?? 0 }));
  }, [rows]);

  if (rows.length === 0) {
    return null;
  }

  return (
    <Card data-testid="run-proposal-summary">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-sm">
          <FileDiff className="h-4 w-4 text-primary" />
          Proposals
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {totals.length > 0 ? (
          <div data-testid="run-proposal-summary-totals" className="flex flex-wrap gap-2">
            {totals.map((total) => (
              <Badge
                key={total.action}
                data-testid="run-proposal-summary-total"
                data-action={total.action}
                variant={countBadgeVariant(total.action)}
                className="gap-1 font-mono text-[11px]"
              >
                <span className="capitalize">{total.action}</span>
                <span>{total.count}</span>
              </Badge>
            ))}
          </div>
        ) : null}

        <div className="space-y-1">
          {rows.map((row) => (
            <button
              key={row.taskId}
              type="button"
              data-testid="run-proposal-summary-row"
              data-task-id={row.taskId}
              data-kind={row.kind}
              onClick={() => onSelectTask?.(row.taskId)}
              className="flex w-full items-center gap-2 rounded-md border border-border/50 bg-obsidian/20 px-3 py-2 text-left text-xs transition-colors hover:bg-obsidian/30"
            >
              <span className="min-w-0 flex-1 truncate font-mono text-text-2" title={row.taskName}>
                {row.taskName}
              </span>
              <Badge data-testid="run-proposal-summary-kind" variant="outline" className="shrink-0 font-mono text-[10px]">
                {row.kind}
              </Badge>
              {row.counts.length > 0 ? (
                <div className="flex shrink-0 flex-wrap gap-1">
                  {row.counts.map((count) => (
                    <Badge
                      key={count.action}
                      data-testid="run-proposal-summary-row-count"
                      data-action={count.action}
                      variant={countBadgeVariant(count.action)}
                      className="gap-1 font-mono text-[10px]"
                    >
                      <span className="capitalize">{count.action}</span>
                      <span>{count.count}</span>
                    </Badge>
                  ))}
                </div>
              ) : (
                <span
                  data-testid="run-proposal-summary-row-counts-blank"
                  className="shrink-0 text-[10px] text-muted-foreground"
                >
                  —
                </span>
              )}
            </button>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

/**
 * Same action-to-variant mapping as `ProposalPanel`'s private `countBadgeVariant`.
 * Kept as a small local copy rather than an import: it is display-only (not
 * parsing, which is what the plan requires reusing from `proposal-renderers.ts`),
 * isn't exported by `ProposalPanel.tsx`, and this stream doesn't own that file.
 */
function countBadgeVariant(action: string): "success" | "destructive" | "secondary" | "outline" {
  switch (action) {
    case "add":
      return "success";
    case "destroy":
      return "destructive";
    case "change":
    case "replace":
      return "secondary";
    default:
      return "outline";
  }
}
