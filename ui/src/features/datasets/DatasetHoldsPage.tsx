import { StatusBadge } from "@/components/ui/status-badge";
import { useState } from "react";
import { getRouteApi, Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { HoldPanel } from "./HoldPanel";
import { holdSearch, readIdentityHold } from "./hold-utils";
import {
  useDataAssertionsEnabled,
  useHoldInvalidation,
} from "./useDataAssertions";

const route = getRouteApi("/datasets/holds");
const pageSize = 20;

export function DatasetHoldsPage() {
  const search = route.useSearch();
  const principal = usePrincipal();
  const enabled = useDataAssertionsEnabled();
  const [status, setStatus] = useState<"active" | "released" | "all">("active");
  const [page, setPage] = useState(0);
  const identity = `${search.namespace ?? ""}\0${search.name ?? ""}`;
  const [previousIdentity, setPreviousIdentity] = useState(identity);
  if (previousIdentity !== identity) {
    setPreviousIdentity(identity);
    setPage(0);
  }
  useHoldInvalidation(enabled);
  const list = useQuery({
    queryKey: [
      "dataset-holds",
      "list",
      status,
      search.namespace,
      search.name,
      page,
    ],
    queryFn: () =>
      api.getDatasetHolds({
        status,
        namespace: search.namespace,
        name: search.name,
        limit: pageSize,
        offset: page * pageSize,
      }),
    enabled: enabled && !principal.isScoped,
    refetchInterval: 15_000,
  });
  const selected = useQuery({
    queryKey: [
      "dataset-holds",
      "identity",
      search.namespace ?? "",
      search.name,
      search.hold,
    ],
    queryFn: () =>
      readIdentityHold(search.namespace ?? "", search.name!, search.hold),
    enabled: enabled && !principal.isScoped && Boolean(search.name),
    refetchInterval: 15_000,
  });
  if (!enabled) return null;
  if (principal.isScoped)
    return (
      <p className="text-sm text-warning">
        Dataset holds require an unscoped key.
      </p>
    );
  return (
    <div className="space-y-5" data-testid="dataset-holds-page">
      <h1 className="text-2xl font-bold lowercase text-text-1">Dataset holds</h1>
      <p className="text-sm text-text-3">
        A held dataset skips downstream runs at admission. Review the producer
        evidence before acknowledging it.
      </p>
      {search.name ? (
        <div className="flex items-center gap-3 text-xs">
          <span className="">
            {search.namespace ? `${search.namespace}/` : ""}
            {search.name}
          </span>
          <Link to="/datasets/holds" search={{}} className="text-cyan-glow">
            All datasets
          </Link>
        </div>
      ) : null}
      <div className="grid items-start gap-5 xl:grid-cols-2">
        <section className="space-y-3">
          <label className="flex items-center gap-3 text-xs">
            Hold status
            <select
              aria-label="Hold status"
              value={status}
              onChange={(event) => {
                setStatus(event.target.value as typeof status);
                setPage(0);
              }}
              className="rounded border border-input bg-background p-2"
            >
              <option value="active">Active</option>
              <option value="released">Released</option>
              <option value="all">All history</option>
            </select>
          </label>
          {list.isPending ? (
            <p>Loading holds…</p>
          ) : list.error ? (
            <p role="alert">{list.error.message}</p>
          ) : (
            <>
              <p className="text-xs text-text-3" data-testid="hold-page-total">
                {list.data.total} holds, page {page + 1} of{" "}
                {Math.max(1, Math.ceil(list.data.total / pageSize))}
              </p>
              {list.data.holds.length === 0 ? (
                <p className="text-sm text-text-3">
                  No {status === "all" ? "recorded" : status} holds match.
                </p>
              ) : null}
              {list.data.holds.map((hold) => (
                <Link
                  key={hold.id}
                  to="/datasets/holds"
                  search={holdSearch(hold)}
                  data-testid="hold-list-row"
                  className="block min-h-14 space-y-1 border-b border-border py-2 hover:bg-obsidian"
                >
                  <p className="break-all text-sm">
                    {hold.namespace ? `${hold.namespace}/` : ""}
                    {hold.name}
                  </p>
                  <p className="text-xs text-text-3">
                    <StatusBadge status={hold.status === "active" ? "queued" : "succeeded"} label={hold.status} size="sm" /> <span className="ml-4">{hold.reason}</span> <span className="ml-4">{hold.occurrence_count}</span>{" "}
                    occurrences
                  </p>
                </Link>
              ))}
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => setPage((p) => p - 1)}
                  disabled={page === 0}
                >
                  Previous holds
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => setPage((p) => p + 1)}
                  disabled={(page + 1) * pageSize >= list.data.total}
                >
                  Next holds
                </Button>
              </div>
            </>
          )}
        </section>
        <div>
          {search.name ? (
            selected.isPending ? (
              <p>Loading hold evidence…</p>
            ) : selected.error ? (
              <p role="alert">{selected.error.message}</p>
            ) : selected.data ? (
              <HoldPanel key={selected.data.id} hold={selected.data} />
            ) : (
              <p className="text-sm text-text-3">
                {search.hold
                  ? "This recorded hold is unavailable."
                  : "No active hold for this dataset. Select All history to inspect released holds."}
              </p>
            )
          ) : (
            <p className="text-sm text-text-3">
              Select a hold to inspect its evidence.
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
