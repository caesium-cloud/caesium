import { useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { api, ApiError, type DatasetHold } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { formatUTCTimestamp } from "@/lib/utils";
import { AssertionEvidence } from "./DataAssertionsPanel";

export function HoldPanel({ hold }: { hold: DatasetHold }) {
  const client = useQueryClient();
  const principal = usePrincipal();
  const [reason, setReason] = useState("");
  const [tolerances, setTolerances] = useState<Record<string, string>>({});
  const [released, setReleased] = useState<DatasetHold | null>(null);
  const [stale, setStale] = useState(false);
  const current = released ?? hold;
  const canRelease =
    Boolean(principal.subject) &&
    !principal.isScoped &&
    (principal.role === "operator" || principal.role === "admin");
  const release = useMutation({
    mutationFn: async () => {
      const result = await api.releaseDatasetHold(
        hold.id,
        reason.trim(),
        Object.fromEntries(
          Object.entries(tolerances).filter(([, value]) => value),
        ),
      );
      if (
        result?.hold?.id !== hold.id ||
        result.hold.namespace !== hold.namespace ||
        result.hold.name !== hold.name ||
        result.hold.status !== "released"
      ) {
        throw new Error(
          "The server did not confirm release of this hold. Refresh its evidence before trying again.",
        );
      }
      return result;
    },
    onSuccess: (result) => setReleased(result.hold),
    onError: (error) => {
      if (
        error instanceof ApiError &&
        (error.status === 409 || error.status === 404)
      )
        setStale(true);
    },
    onSettled: () => {
      void client.invalidateQueries({ queryKey: ["datasets"] });
      void client.invalidateQueries({ queryKey: ["dataset-holds"] });
      void client.invalidateQueries({ queryKey: ["lineage"] });
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (
      canRelease &&
      current.status === "active" &&
      reason.trim() &&
      !stale &&
      !release.isPending
    )
      release.mutate();
  }
  const assertionKeys = [
    ...new Set((hold.violations ?? []).map((v) => v.assertion)),
  ];
  return (
    <section
      className="space-y-4 rounded-lg border border-fuchsia-400/40 bg-card p-4"
      data-testid="hold-panel"
      data-hold-id={hold.id}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold text-text-1">
          Dataset hold · {current.status}
        </h2>
        <span className="font-mono text-[11px] text-text-3">{hold.id}</span>
      </div>
      <p className="break-all font-mono text-sm text-fuchsia-300">
        {hold.namespace ? `${hold.namespace}/` : ""}
        {hold.name}
      </p>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-xs">
        <dt className="text-text-3">Opened</dt>
        <dd>{formatUTCTimestamp(hold.opened_at, hold.opened_at)}</dd>
        <dt className="text-text-3">Occurrences</dt>
        <dd data-testid="hold-occurrences">{hold.occurrence_count}</dd>
        <dt className="text-text-3">Producing step</dt>
        <dd>{hold.held_by_step || "Unavailable"}</dd>
        <dt className="text-text-3">Opening run</dt>
        <dd>
          {hold.held_by_run_id ? (
            <Link
              to="/jobs/$jobId/runs/$runId"
              params={{
                jobId: hold.held_by_job_id,
                runId: hold.held_by_run_id,
              }}
              className="text-cyan-glow hover:underline"
            >
              {hold.held_by_job_alias || hold.held_by_job_id} ·{" "}
              {hold.held_by_run_id}
            </Link>
          ) : (
            "Run reference unavailable"
          )}
        </dd>
        {hold.last_breach_run_id &&
        hold.last_breach_run_id !== hold.held_by_run_id ? (
          <>
            <dt className="text-text-3">Latest breach run</dt>
            <dd
              className="break-all font-mono"
              data-testid="hold-latest-breach-run"
            >
              {/* The latest occurrence may come from a different producer.
                  The API only supplies its run ID, not its owning job ID. */}
              {hold.last_breach_run_id}
            </dd>
          </>
        ) : null}
      </dl>
      <div className="space-y-4">
        <h3 className="text-xs font-semibold text-text-2">
          Latest breach evidence
        </h3>
        {hold.violations?.length ? (
          hold.violations.map((violation, i) => (
            <AssertionEvidence key={i} violation={violation} />
          ))
        ) : (
          <p className="text-xs text-text-3">Violation evidence unavailable.</p>
        )}
      </div>
      <Link
        to="/lineage"
        search={{ namespace: hold.namespace, name: hold.name }}
        className="block text-xs text-cyan-glow hover:underline"
      >
        Inspect downstream lineage
        {hold.impact
          ? ` (${hold.impact.downstream.length} outputs recorded when opened)`
          : ""}
      </Link>
      {current.status === "released" ? (
        <div
          className="space-y-1 rounded bg-success/10 p-3 text-xs"
          role="status"
        >
          <p>
            Released by {current.released_by || "unknown"} ·{" "}
            {current.release_reason}
          </p>
          <p>{current.release_note}</p>
          {current.released_at ? (
            <p>
              {formatUTCTimestamp(current.released_at, current.released_at)}
            </p>
          ) : null}
          {Object.entries(current.tolerances ?? {}).map(([key, value]) => (
            <p key={key}>
              Recorded tolerance: {key} = {value} (advisory only)
            </p>
          ))}
        </div>
      ) : (
        <form
          className="space-y-3 border-t border-border/50 pt-4"
          onSubmit={submit}
        >
          <h3 className="text-sm font-semibold">Release hold</h3>
          {!canRelease ? (
            <p className="text-xs text-warning" data-testid="hold-release-gate">
              Manual release requires an authenticated, unscoped operator or
              administrator. Enable authentication on the server for human
              acknowledgement.
            </p>
          ) : null}
          <label className="block space-y-1 text-xs">
            Reason (required)
            <textarea
              aria-label="Release reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
              required
              disabled={!canRelease || stale || release.isPending}
              className="mt-1 block min-h-20 w-full rounded border border-input bg-background p-2 text-text-1"
            />
          </label>
          {assertionKeys.map((key) => (
            <label
              key={key}
              className="flex items-center justify-between gap-3 text-xs"
            >
              Advisory tolerance · {key}
              <select
                aria-label={`Tolerance for ${key}`}
                value={tolerances[key] ?? ""}
                onChange={(event) =>
                  setTolerances((previous) => ({
                    ...previous,
                    [key]: event.target.value,
                  }))
                }
                disabled={!canRelease || stale || release.isPending}
                className="rounded border border-input bg-background p-2"
              >
                <option value="">None</option>
                <option value="1h">1 hour</option>
                <option value="24h">24 hours</option>
                <option value="72h">72 hours</option>
              </select>
            </label>
          ))}
          <p className="text-[11px] text-text-3">
            Tolerance windows are recorded as advisory evidence. They do not
            suppress assertions; another breach may open a new hold.
          </p>
          {release.error ? (
            <p role="alert" className="text-xs text-danger">
              {release.error.message}
              {stale
                ? " This hold changed. Its evidence has been refreshed; review any new active hold separately."
                : ""}
            </p>
          ) : null}
          <Button
            type="submit"
            size="sm"
            disabled={
              !canRelease || !reason.trim() || stale || release.isPending
            }
          >
            {release.isPending ? "Releasing…" : "Release hold"}
          </Button>
        </form>
      )}
    </section>
  );
}
