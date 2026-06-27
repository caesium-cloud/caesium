import { type FormEvent, useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeftRight, Plus, RotateCcw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { StatusBadge } from "@/components/ui/status-badge";
import { InsufficientAccess } from "@/features/auth/InsufficientAccess";
import { ApiError, api, type JobRun, type ReplayResponse } from "@/lib/api";
import { shortId } from "@/lib/utils";
import { RunDiffView } from "./RunDiffView";

interface ReplayDialogProps {
  jobId: string;
  baselineRunId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

type OverrideRow = {
  id: string;
  key: string;
  value: string;
};

export function ReplayDialog({
  jobId,
  baselineRunId,
  open,
  onOpenChange,
}: ReplayDialogProps) {
  const queryClient = useQueryClient();
  const [overrideRows, setOverrideRows] = useState<OverrideRow[]>(() => [newOverrideRow()]);
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const [submittedKey, setSubmittedKey] = useState<string | null>(null);
  const [inlineError, setInlineError] = useState<string | null>(null);
  const [lastError, setLastError] = useState<unknown>(null);
  const [replayResponse, setReplayResponse] = useState<ReplayResponse | null>(null);
  const [showDiff, setShowDiff] = useState(false);

  const replayRunId = replayResponse?.run_id;
  const { data: replayRun } = useQuery<JobRun>({
    queryKey: ["job", jobId, "runs", replayRunId],
    queryFn: () => api.getJobRun(jobId, replayRunId!),
    enabled: open && Boolean(replayRunId),
    refetchInterval: (query) =>
      replayRunId && !isTerminalStatus(query.state.data?.status ?? replayResponse?.status)
        ? 2_000
        : false,
  });

  const mutation = useMutation({
    mutationFn: ({ set, key }: { set: Record<string, string>; key: string }) =>
      api.postReplay(jobId, baselineRunId, { set }, key),
    onSuccess: (response) => {
      setReplayResponse(response);
      setInlineError(null);
      setLastError(null);
      setShowDiff(false);
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs"] });
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "runs", response.run_id] });
      toast.success("Replay submitted");
    },
    onError: (error) => {
      setReplayResponse(null);
      setShowDiff(false);
      setLastError(error);
      setInlineError(replayErrorMessage(error));
    },
  });

  const hasSubmittedKey = Boolean(submittedKey);
  const resultStatus = replayRun?.status ?? replayResponse?.status ?? "pending";
  const resultQuarantined = Boolean(replayResponse?.quarantine || replayRun?.quarantine);
  const normalizedRows = useMemo(
    () => overrideRows.map((row) => ({ ...row, key: row.key.trim() })),
    [overrideRows],
  );

  function updateOverrideRow(id: string, patch: Partial<Pick<OverrideRow, "key" | "value">>) {
    setOverrideRows((rows) => rows.map((row) => (row.id === id ? { ...row, ...patch } : row)));
  }

  function removeOverrideRow(id: string) {
    setOverrideRows((rows) => (rows.length > 1 ? rows.filter((row) => row.id !== id) : [newOverrideRow()]));
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setInlineError(null);
    setLastError(null);

    const parsed = parseOverrides(overrideRows);
    if (typeof parsed === "string") {
      setReplayResponse(null);
      setShowDiff(false);
      setInlineError(parsed);
      return;
    }

    const key = idempotencyKey.trim() || newReplayKey();
    if (key !== idempotencyKey) {
      setIdempotencyKey(key);
    }
    setSubmittedKey(key);
    mutation.mutate({ set: parsed, key });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-h-[90vh] max-w-5xl overflow-y-auto"
        data-testid="replay-dialog"
      >
        <DialogHeader>
          <DialogTitle>Replay Run</DialogTitle>
          <DialogDescription>
            Launch a quarantined replay from baseline run {shortId(baselineRunId)}.
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-5">
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  --set overrides
                </div>
                <p className="mt-1 text-xs text-text-3">
                  Leave all rows blank for a no-override replay.
                </p>
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setOverrideRows((rows) => [...rows, newOverrideRow()])}
                disabled={mutation.isPending}
                data-testid="replay-add-set-row"
              >
                <Plus className="h-3.5 w-3.5" />
                Add
              </Button>
            </div>

            <div className="space-y-2" data-testid="replay-set-rows">
              {normalizedRows.map((row, index) => (
                <div
                  key={row.id}
                  className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
                  data-testid="replay-set-row"
                >
                  <input
                    type="text"
                    value={overrideRows[index].key}
                    onChange={(event) => updateOverrideRow(row.id, { key: event.target.value })}
                    placeholder="key"
                    disabled={mutation.isPending}
                    className={inputClassName}
                    data-testid={`replay-set-key-${index}`}
                  />
                  <input
                    type="text"
                    value={row.value}
                    onChange={(event) => updateOverrideRow(row.id, { value: event.target.value })}
                    placeholder="value"
                    disabled={mutation.isPending}
                    className={inputClassName}
                    data-testid={`replay-set-value-${index}`}
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    className="h-8 w-8 text-text-3"
                    onClick={() => removeOverrideRow(row.id)}
                    disabled={mutation.isPending}
                    title="Remove override"
                    data-testid={`replay-remove-set-${index}`}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </div>
              ))}
            </div>
          </div>

          <div>
            <label
              htmlFor="replay-idempotency-key"
              className="mb-1 block text-xs font-semibold uppercase tracking-wide text-muted-foreground"
            >
              Idempotency key
            </label>
            <input
              id="replay-idempotency-key"
              type="text"
              value={idempotencyKey}
              onChange={(event) => {
                setSubmittedKey(null);
                setIdempotencyKey(event.target.value);
              }}
              placeholder="Auto-generated when blank"
              disabled={mutation.isPending}
              className={inputClassName}
              data-testid="replay-idempotency-key-input"
            />
            {hasSubmittedKey ? (
              <p className="mt-1 text-xs text-text-3" data-testid="replay-idempotency-key-display">
                Submitted key: <span className="font-mono">{submittedKey}</span>
              </p>
            ) : null}
          </div>

          {inlineError ? (
            <div data-testid="replay-inline-error">
              {lastError instanceof ApiError && lastError.kind === "insufficient_access" ? (
                <InsufficientAccess
                  error={lastError}
                  message={inlineError}
                  className="w-full"
                />
              ) : (
                <div
                  role="alert"
                  className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive"
                >
                  <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
                  <span>{inlineError}</span>
                </div>
              )}
            </div>
          ) : null}

          <div className="flex justify-end">
            <Button
              type="submit"
              size="sm"
              disabled={mutation.isPending}
              data-testid="replay-submit"
            >
              <RotateCcw className="h-3.5 w-3.5" />
              {mutation.isPending ? "Submitting..." : "Launch Replay"}
            </Button>
          </div>
        </form>

        {replayResponse ? (
          <div
            className="space-y-4 rounded-md border border-border bg-muted/30 p-4"
            data-testid="replay-result"
          >
            <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
              <div>
                <div className="mb-1 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  Replay run
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <span
                    className="font-mono text-sm text-text-1"
                    data-testid="replay-result-run-id"
                  >
                    {replayResponse.run_id}
                  </span>
                  <span data-testid="replay-result-status">
                    <StatusBadge status={resultStatus} size="sm" />
                  </span>
                  {resultQuarantined ? (
                    <Badge
                      variant="outline"
                      className="border-warning/40 bg-warning/10 text-warning"
                      data-testid="replay-quarantine-badge"
                    >
                      Quarantine
                    </Badge>
                  ) : null}
                </div>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <Button asChild variant="outline" size="sm">
                  <Link
                    to="/jobs/$jobId/runs/$runId"
                    params={{ jobId, runId: replayResponse.run_id }}
                    data-testid="replay-open-run"
                  >
                    Open run
                  </Link>
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setShowDiff((current) => !current)}
                  data-testid="replay-show-diff"
                >
                  <ArrowLeftRight className="h-3.5 w-3.5" />
                  {showDiff ? "Hide diff vs baseline" : "Show diff vs baseline"}
                </Button>
              </div>
            </div>

            {showDiff ? (
              <div
                className="max-h-[56vh] overflow-y-auto rounded-md border border-border bg-background p-4"
                data-testid="replay-diff-panel"
              >
                <RunDiffView
                  jobId={jobId}
                  leftRunId={baselineRunId}
                  rightRunId={replayResponse.run_id}
                />
              </div>
            ) : null}
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

const inputClassName =
  "w-full rounded-md border border-border bg-background px-3 py-1.5 text-sm text-foreground " +
  "focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50";

function parseOverrides(rows: OverrideRow[]): Record<string, string> | string {
  const overrides: Record<string, string> = {};
  for (const row of rows) {
    const key = row.key.trim();
    const hasValue = row.value.trim() !== "";
    if (!key && !hasValue) {
      continue;
    }
    if (!key) {
      return "Each --set override needs a key.";
    }
    if (Object.prototype.hasOwnProperty.call(overrides, key)) {
      return `Duplicate --set override "${key}".`;
    }
    overrides[key] = row.value;
  }
  return overrides;
}

function replayErrorMessage(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return error instanceof Error ? error.message : "Replay request failed.";
  }

  switch (error.kind) {
    case "insufficient_access":
      return "Replay requires a runner key.";
    case "replay_requires_distributed_execution":
      return "This replay re-executes tasks, which requires distributed execution mode.";
    case "replay_safe_refusal":
      return `Replay-safe gate refused this replay: ${error.message}`;
    case "replay_refused":
      return `Descriptor refused this replay: ${error.message}`;
    case "replay_target_not_found":
      return "Replay target was not found or does not belong to this job.";
    case "replay_missing_idempotency_key":
      return "Replay request is missing an idempotency key.";
    case "replay_bad_request":
      return `Replay request is invalid: ${error.message}`;
    case "replay_request_too_large":
      return "Replay request is too large. Remove some --set overrides.";
    case "replay_conflict":
      return `Replay could not start from this baseline: ${error.message}`;
    case "authentication_required":
      return "Authentication is required to launch replay.";
    default:
      return error.message || "Replay request failed.";
  }
}

function isTerminalStatus(status?: string): boolean {
  const normalized = status?.toLowerCase();
  return (
    normalized === "succeeded" ||
    normalized === "completed" ||
    normalized === "failed" ||
    normalized === "cancelled"
  );
}

function newOverrideRow(): OverrideRow {
  return {
    id: `set-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`,
    key: "",
    value: "",
  };
}

function newReplayKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return `ui-replay-${crypto.randomUUID()}`;
  }
  return `ui-replay-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}
