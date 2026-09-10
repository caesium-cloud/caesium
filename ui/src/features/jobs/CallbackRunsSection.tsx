import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { StatusBadge } from "@/components/ui/status-badge";
import type { CallbackRun } from "@/lib/api";
import { shortId } from "@/lib/utils";

/**
 * CallbackRunsSection renders a run's webhook callback attempts. Beyond the
 * pre-formatted `error` string it surfaces the transport detail the backend now
 * records — the HTTP status the target answered with, how many attempts
 * preceded this one, and the (already truncated + scrubbed) response body — so
 * a transient network failure is distinguishable from a permanent 4xx without
 * leaving the page.
 */
export function CallbackRunsSection({ callbacks }: { callbacks: CallbackRun[] }) {
  return (
    <Card data-testid="run-callbacks-section">
      <CardHeader className="pb-3">
        <CardTitle className="text-sm">Callbacks</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {callbacks.map((callback) => (
          <CallbackRunRow key={callback.id} callback={callback} />
        ))}
      </CardContent>
    </Card>
  );
}

function CallbackRunRow({ callback }: { callback: CallbackRun }) {
  const [bodyOpen, setBodyOpen] = useState(false);
  const failed = callback.status?.toLowerCase() === "failed";
  const retryCount = callback.retry_count ?? 0;
  const bodyId = `run-callback-body-${callback.id}`;

  return (
    <div
      data-testid="run-callback-row"
      className={`rounded-md border px-3 py-2 ${
        failed ? "border-danger/40 bg-danger/5" : "border-border/50 bg-obsidian/20"
      }`}
    >
      <div className="flex flex-wrap items-center gap-2">
        <StatusBadge status={callback.status} size="sm" />
        <span className="font-mono text-xs text-text-2">callback {shortId(callback.callback_id)}</span>
        <span className="font-mono text-[10px] text-text-4">run {shortId(callback.id)}</span>
        {callback.http_status ? (
          <span
            data-testid="run-callback-http-status"
            className={`font-mono text-[10px] ${failed ? "text-danger" : "text-text-3"}`}
          >
            HTTP {callback.http_status}
          </span>
        ) : null}
        {retryCount > 0 ? (
          <span data-testid="run-callback-retry-count" className="font-mono text-[10px] text-text-4">
            {retryCount === 1 ? "1 retry" : `${retryCount} retries`}
          </span>
        ) : null}
      </div>
      {callback.error ? (
        <div
          data-testid="run-callback-error"
          className={`mt-2 break-words font-mono text-xs ${failed ? "text-danger" : "text-text-3"}`}
        >
          {callback.error}
        </div>
      ) : null}
      {callback.response_body ? (
        <>
          <button
            type="button"
            data-testid="run-callback-body-toggle"
            className="mt-2 flex items-center gap-1 text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3 transition-colors hover:text-text-2"
            onClick={() => setBodyOpen((open) => !open)}
            aria-expanded={bodyOpen}
            aria-controls={bodyId}
          >
            {bodyOpen ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
            Response body
          </button>
          {bodyOpen ? (
            <pre
              id={bodyId}
              data-testid="run-callback-response-body"
              className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded border border-border/50 bg-obsidian/40 p-2 font-mono text-[11px] text-text-3"
            >
              {callback.response_body}
            </pre>
          ) : null}
        </>
      ) : null}
    </div>
  );
}
