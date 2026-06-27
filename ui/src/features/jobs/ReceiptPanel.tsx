import { type ChangeEvent, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { AlertTriangle, FileJson, Loader2, ShieldAlert, ShieldCheck, Upload } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type Receipt, type ReceiptDrift, type ReceiptDriftKind, type VerifyResult } from "@/lib/api";
import { cn } from "@/lib/utils";

interface ReceiptPanelProps {
  jobId: string;
  runId: string;
}

export function ReceiptPanel({ jobId, runId }: ReceiptPanelProps) {
  const [committedReceiptText, setCommittedReceiptText] = useState("");
  const [inputError, setInputError] = useState<string | null>(null);

  const receiptQuery = useQuery({
    queryKey: ["job", jobId, "runs", runId, "receipt"],
    queryFn: () => api.getReceipt(jobId, runId),
    enabled: Boolean(jobId && runId),
    staleTime: 15_000,
  });

  const verifyMutation = useMutation({
    mutationFn: (receipt: Receipt) => api.postVerify(receipt),
    onSuccess: (result) => {
      toast.success(result.match ? "Committed receipt is reproducible" : "Committed receipt drift detected");
    },
    onError: (err: Error) => toast.error(`Receipt verify failed: ${err.message}`),
  });

  const degradedTasks = receiptQuery.data?.degraded_tasks ?? [];

  const handleVerify = () => {
    const parsed = parseCommittedReceipt(committedReceiptText, jobId, runId);
    if ("error" in parsed) {
      setInputError(parsed.error);
      return;
    }

    setInputError(null);
    verifyMutation.mutate(parsed.receipt);
  };

  const handleUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) {
      return;
    }

    try {
      setInputError(null);
      setCommittedReceiptText(await file.text());
    } catch (err) {
      const message = err instanceof Error ? err.message : "Could not read receipt file";
      setInputError(message);
    } finally {
      event.target.value = "";
    }
  };

  return (
    <Card data-testid="receipt-panel">
      <CardHeader className="pb-3">
        <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
          <div>
            <CardTitle className="flex items-center gap-2 text-sm">
              <FileJson className="h-4 w-4 text-primary" />
              Reproducibility Receipt
            </CardTitle>
            <p className="mt-1 max-w-3xl text-xs text-text-3">
              Verify checks a committed receipt you provide against the server's freshly re-derived state for this run. It is not a self-check of the receipt displayed here.
            </p>
          </div>
          {receiptQuery.data ? <ReceiptStatus receipt={receiptQuery.data} /> : null}
        </div>
      </CardHeader>
      <CardContent className="space-y-5">
        {receiptQuery.isLoading ? (
          <ReceiptSkeleton />
        ) : receiptQuery.error ? (
          <div
            data-testid="receipt-error"
            className="rounded-md border border-warning/25 bg-warning/10 p-3 text-xs text-warning"
          >
            <div className="flex items-start gap-2">
              <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
              <div>
                <div className="font-semibold">Receipt unavailable</div>
                <div className="mt-1 text-warning/80">{receiptQuery.error.message}</div>
              </div>
            </div>
          </div>
        ) : receiptQuery.data ? (
          <>
            <ReceiptContent receipt={receiptQuery.data} />
            {receiptQuery.data.degraded ? <UnverifiableSummary tasks={degradedTasks} /> : null}
          </>
        ) : (
          <div className="text-xs text-text-3">No receipt returned for this run.</div>
        )}

        <VerifyForm
          committedReceiptText={committedReceiptText}
          inputError={inputError}
          isPending={verifyMutation.isPending}
          result={verifyMutation.data}
          onChange={setCommittedReceiptText}
          onUpload={handleUpload}
          onVerify={handleVerify}
        />
      </CardContent>
    </Card>
  );
}

function ReceiptSkeleton() {
  return (
    <div className="space-y-3" data-testid="receipt-loading">
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-24 w-full" />
      <Skeleton className="h-28 w-full" />
    </div>
  );
}

function ReceiptStatus({ receipt }: { receipt: Receipt }) {
  return receipt.degraded ? (
    <Badge data-testid="receipt-degraded-status" variant="destructive" className="h-fit gap-1.5">
      <ShieldAlert className="h-3.5 w-3.5" />
      degraded-unverifiable
    </Badge>
  ) : (
    <Badge data-testid="receipt-degraded-status" variant="success" className="h-fit gap-1.5">
      <ShieldCheck className="h-3.5 w-3.5" />
      reproducible
    </Badge>
  );
}

function ReceiptContent({ receipt }: { receipt: Receipt }) {
  return (
    <div data-testid="receipt-summary" className="space-y-4">
      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
        <MetadataCell testId="receipt-version" label="receipt_version" value={`v${receipt.receipt_version}`} mono />
        <MetadataCell testId="receipt-run-id" label="run_id" value={receipt.run_id} mono />
        <MetadataCell testId="receipt-job-id" label="job_id" value={receipt.job_id} mono />
        <MetadataCell testId="receipt-job-alias" label="job_alias" value={receipt.job_alias || "None"} mono />
        <MetadataCell testId="receipt-git-commit" label="git_commit" value={receipt.git_commit || "None"} mono />
        <MetadataCell
          testId="receipt-manifest-content-hash"
          label="manifest_content_hash"
          value={receipt.manifest_content_hash || "None"}
          mono
        />
        <MetadataCell testId="receipt-task-count" label="tasks" value={String(receipt.tasks.length)} mono />
        <MetadataCell testId="receipt-degraded" label="degraded" value={String(receipt.degraded)} mono />
      </div>

      <div className="rounded-md border border-border/60 bg-background/40 p-3">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          receipt_digest
        </div>
        <div
          data-testid="receipt-digest"
          className="mt-1 break-all font-mono text-xs text-text-1"
          title={receipt.receipt_digest}
        >
          {receipt.receipt_digest}
        </div>
      </div>

      <div className="space-y-2">
        <SectionLabel>tasks</SectionLabel>
        <div className="space-y-2">
          {receipt.tasks.map((task) => (
            <div
              key={`${task.task_name}:${task.identity_hash}`}
              data-testid="receipt-task-row"
              data-task-name={task.task_name}
              className={cn(
                "rounded-md border bg-background/40 p-3",
                task.degraded ? "border-warning/35" : "border-border/50",
              )}
            >
              <div className="flex flex-col gap-2 md:flex-row md:items-start md:justify-between">
                <div className="min-w-0">
                  <div
                    data-testid="receipt-task-name"
                    className="break-words font-mono text-sm font-semibold text-text-1"
                  >
                    {task.task_name}
                  </div>
                  <div className="mt-1 break-all font-mono text-xs text-text-3" data-testid="receipt-task-image">
                    {task.image}
                  </div>
                </div>
                <div className="flex flex-wrap gap-2">
                  <Badge variant={task.digest_pinned ? "success" : "destructive"} className="text-[10px]">
                    digest_pinned={String(task.digest_pinned)}
                  </Badge>
                  {task.degraded ? (
                    <Badge data-testid="receipt-task-unverifiable-marker" variant="destructive" className="text-[10px]">
                      unverifiable
                    </Badge>
                  ) : null}
                </div>
              </div>

              <div className="mt-3 grid gap-3 md:grid-cols-2">
                <MetadataCell
                  testId="receipt-task-identity-hash"
                  label="identity_hash"
                  value={task.identity_hash || "None"}
                  mono
                />
                <MetadataCell
                  testId="receipt-task-resolved-image-digest"
                  label="resolved_image_digest"
                  value={task.resolved_image_digest || "None"}
                  mono
                />
                <MetadataCell
                  testId="receipt-task-digest-pinned"
                  label="digest_pinned"
                  value={String(task.digest_pinned)}
                  mono
                />
                <MetadataCell
                  testId="receipt-task-degraded"
                  label="degraded"
                  value={String(task.degraded)}
                  mono
                />
              </div>

              {task.degraded_reason ? (
                <div
                  data-testid="receipt-task-degraded-reason"
                  className="mt-3 rounded-md border border-warning/25 bg-warning/10 p-2 text-xs text-warning"
                >
                  {task.degraded_reason}
                </div>
              ) : null}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function UnverifiableSummary({ tasks }: { tasks: string[] }) {
  if (tasks.length === 0) {
    return null;
  }

  return (
    <div
      data-testid="receipt-unverifiable-summary"
      className="rounded-md border border-warning/25 bg-warning/10 p-3 text-xs text-warning"
    >
      <div className="flex items-start gap-2">
        <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <div>
          <div className="font-semibold">This receipt is degraded-unverifiable.</div>
          <div className="mt-1 text-warning/85">
            Unverifiable tasks:{" "}
            {tasks.map((task) => (
              <span key={task} data-testid="receipt-degraded-task" className="font-mono">
                {task}
              </span>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}

interface VerifyFormProps {
  committedReceiptText: string;
  inputError: string | null;
  isPending: boolean;
  result?: VerifyResult;
  onChange: (value: string) => void;
  onUpload: (event: ChangeEvent<HTMLInputElement>) => void;
  onVerify: () => void;
}

function VerifyForm({
  committedReceiptText,
  inputError,
  isPending,
  result,
  onChange,
  onUpload,
  onVerify,
}: VerifyFormProps) {
  return (
    <div className="space-y-3 rounded-md border border-border/60 bg-muted/25 p-3" data-testid="receipt-verify-form">
      <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
        <div>
          <SectionLabel>verify committed receipt</SectionLabel>
          <p className="mt-1 text-xs text-text-3">
            Paste or upload the receipt JSON committed with this run.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input
            id="receipt-file-upload"
            data-testid="receipt-verify-upload"
            type="file"
            accept=".receipt,.json,application/json"
            className="sr-only"
            onChange={onUpload}
          />
          <Button asChild variant="outline" size="sm">
            <label htmlFor="receipt-file-upload" className="cursor-pointer">
              <Upload className="h-3.5 w-3.5" />
              Upload .receipt
            </label>
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={onVerify}
            disabled={isPending || committedReceiptText.trim().length === 0}
            data-testid="receipt-verify-submit"
          >
            {isPending ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <ShieldCheck className="h-3.5 w-3.5" />}
            Verify
          </Button>
        </div>
      </div>

      <textarea
        data-testid="receipt-verify-input"
        value={committedReceiptText}
        onChange={(event) => onChange(event.target.value)}
        spellCheck={false}
        className="min-h-[128px] w-full resize-y rounded-md border border-border/70 bg-background/60 p-3 font-mono text-xs text-text-1 outline-none focus:border-primary"
        placeholder='{"receipt_version":1,"run_id":"...","job_id":"...","tasks":[],"degraded":false,"receipt_digest":"..."}'
      />

      {inputError ? (
        <div
          data-testid="receipt-verify-input-error"
          className="rounded-md border border-danger/25 bg-danger/10 p-2 text-xs text-danger"
        >
          {inputError}
        </div>
      ) : null}

      {result ? <VerifyResultView result={result} /> : null}
    </div>
  );
}

function VerifyResultView({ result }: { result: VerifyResult }) {
  const verdict = verifyVerdict(result);
  const drifts = result.drifts ?? [];

  return (
    <div
      data-testid="receipt-verify-result"
      className={cn(
        "space-y-3 rounded-md border p-3",
        verdict === "reproducible"
          ? "border-success/30 bg-success/10"
          : verdict === "degraded-unverifiable"
            ? "border-warning/30 bg-warning/10"
            : "border-danger/30 bg-danger/10",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <Badge
          data-testid="receipt-verify-verdict"
          variant={verdict === "reproducible" ? "success" : verdict === "degraded-unverifiable" ? "destructive" : "outline"}
        >
          {verdict}
        </Badge>
        <span className="font-mono text-[10px] text-text-3">run_id={result.run_id}</span>
      </div>

      <div className="grid gap-3 md:grid-cols-2">
        <MetadataCell
          testId="receipt-verify-expected-digest"
          label="expected_digest"
          value={result.expected_digest}
          mono
        />
        <MetadataCell
          testId="receipt-verify-actual-digest"
          label="actual_digest"
          value={result.actual_digest}
          mono
        />
        <MetadataCell testId="receipt-verify-match" label="match" value={String(result.match)} mono />
        <MetadataCell testId="receipt-verify-degraded" label="degraded" value={String(result.degraded)} mono />
      </div>

      {result.degraded_tasks && result.degraded_tasks.length > 0 ? (
        <div className="rounded-md border border-warning/25 bg-warning/10 p-2 text-xs text-warning">
          <span className="font-semibold">degraded_tasks </span>
          {result.degraded_tasks.map((task) => (
            <span key={task} data-testid="receipt-verify-degraded-task" className="font-mono">
              {task}
            </span>
          ))}
        </div>
      ) : null}

      {drifts.length > 0 ? (
        <div className="space-y-2">
          <SectionLabel>drifts</SectionLabel>
          {drifts.map((drift, index) => (
            <DriftRow key={`${drift.kind}:${drift.task ?? "run"}:${index}`} drift={drift} />
          ))}
        </div>
      ) : (
        <div data-testid="receipt-verify-no-drifts" className="text-xs text-text-3">
          No drift returned by the backend.
        </div>
      )}
    </div>
  );
}

function DriftRow({ drift }: { drift: ReceiptDrift }) {
  return (
    <div data-testid="receipt-verify-drift-row" className="rounded-md border border-border/50 bg-background/40 p-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge data-testid="receipt-verify-drift-kind" variant="outline" className="font-mono text-[10px]">
          {drift.kind}
        </Badge>
        {drift.task ? (
          <span data-testid="receipt-verify-drift-task" className="font-mono text-[10px] text-text-3">
            {drift.task}
          </span>
        ) : null}
      </div>
      <div data-testid="receipt-verify-drift-detail" className="mt-1 text-xs text-text-3">
        {drift.detail}
      </div>
      {drift.expected || drift.actual ? (
        <div className="mt-2 grid gap-2 md:grid-cols-2">
          <MetadataCell label="expected" value={drift.expected || "None"} mono />
          <MetadataCell label="actual" value={drift.actual || "None"} mono />
        </div>
      ) : null}
    </div>
  );
}

function MetadataCell({
  label,
  value,
  mono,
  testId,
}: {
  label: string;
  value: string;
  mono?: boolean;
  testId?: string;
}) {
  return (
    <div className="min-w-0 rounded-md border border-border/50 bg-background/40 p-2">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div
        data-testid={testId}
        className={cn("mt-1 break-all text-xs text-text-1", mono ? "font-mono" : undefined)}
        title={value}
      >
        {value}
      </div>
    </div>
  );
}

function SectionLabel({ children }: { children: string }) {
  return (
    <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
      {children}
    </div>
  );
}

function verifyVerdict(result: VerifyResult): "reproducible" | "degraded-unverifiable" | ReceiptDriftKind | "drift" {
  if (result.match) {
    return "reproducible";
  }
  if (result.degraded) {
    return "degraded-unverifiable";
  }

  const drifts = result.drifts ?? [];
  return drifts.find((drift) => drift.kind !== "receipt_digest_mismatch")?.kind ?? drifts[0]?.kind ?? "drift";
}

type ParseResult = { receipt: Receipt } | { error: string };

function parseCommittedReceipt(raw: string, currentJobId: string, currentRunId: string): ParseResult {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    const detail = err instanceof Error ? err.message : "invalid JSON";
    return { error: `Committed receipt JSON could not be parsed: ${detail}` };
  }

  if (!isReceipt(parsed)) {
    return { error: "Committed receipt JSON does not match the Receipt shape." };
  }

  if (parsed.job_id !== currentJobId || parsed.run_id !== currentRunId) {
    return { error: "Committed receipt job_id and run_id must match the current run." };
  }

  return { receipt: parsed };
}

function isReceipt(value: unknown): value is Receipt {
  if (!isObject(value)) {
    return false;
  }
  return (
    typeof value.receipt_version === "number" &&
    typeof value.run_id === "string" &&
    typeof value.job_id === "string" &&
    Array.isArray(value.tasks) &&
    value.tasks.every(isReceiptTaskEntry) &&
    typeof value.degraded === "boolean" &&
    typeof value.receipt_digest === "string"
  );
}

function isReceiptTaskEntry(value: unknown) {
  if (!isObject(value)) {
    return false;
  }
  return (
    typeof value.task_name === "string" &&
    typeof value.identity_hash === "string" &&
    typeof value.image === "string" &&
    typeof value.digest_pinned === "boolean" &&
    typeof value.degraded === "boolean" &&
    (value.resolved_image_digest === undefined || typeof value.resolved_image_digest === "string") &&
    (value.degraded_reason === undefined || typeof value.degraded_reason === "string")
  );
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
