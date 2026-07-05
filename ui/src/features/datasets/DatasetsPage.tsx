import { useMemo, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useQueries, useQuery } from "@tanstack/react-query";
import {
  AlertTriangle,
  ChevronLeft,
  ChevronRight,
  Database,
  GitBranch,
  History,
} from "lucide-react";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import {
  api,
  type DatasetDetail,
  type DatasetState,
  type DatasetStatus,
} from "@/lib/api";
import { cn, shortId } from "@/lib/utils";
import { DerivationsPanel } from "./DerivationsPanel";
import { FreshnessStatusChip } from "./FreshnessStatusChip";
import {
  DATASET_STATUS_FILTERS,
  cleanDatasetParam,
  datasetKey,
  datasetNamespace,
  displayNamespace,
  effectiveObservedAt,
  formatDurationShort,
  freshnessTone,
  normalizeStatusFilter,
  stalenessPercent,
  type DatasetStatusFilter,
} from "./freshness-utils";

const datasetsRouteApi = getRouteApi("/datasets");

// Backend caps a single /datasets page at 200 rows (maxListLimit), so the board
// pages through the feed by offset rather than fetching an unbounded slice.
const PAGE_SIZE = 50;

type DatasetsSearch = {
  status?: DatasetStatusFilter;
  namespace?: string;
  name?: string;
};

export function DatasetsPage() {
  const search = datasetsRouteApi.useSearch() as DatasetsSearch;
  const navigate = useNavigate();
  const statusFilter = normalizeStatusFilter(search.status);
  const selectedNamespace = search.namespace ?? "";
  const selectedName = cleanDatasetParam(search.name);
  const [page, setPage] = useState(0);

  // Reset to the first page whenever the status filter changes so the offset
  // never points past the end of a narrower result set. Adjusting state during
  // render (rather than in an effect) is React's recommended pattern for
  // deriving state from a changed input and avoids a cascading-render pass.
  const [pagedFilter, setPagedFilter] = useState(statusFilter);
  if (pagedFilter !== statusFilter) {
    setPagedFilter(statusFilter);
    setPage(0);
  }

  const listQuery = useQuery({
    queryKey: ["datasets", "list", statusFilter, page],
    queryFn: () =>
      api.getDatasets({
        status: statusFilter === "all" ? undefined : statusFilter,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
    placeholderData: (previous) => previous,
    refetchInterval: 30_000,
  });

  const total = listQuery.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const rows = useMemo(() => listQuery.data?.datasets ?? [], [listQuery.data]);
  const detailQueries = useQueries({
    queries: rows.map((state) => {
      const namespace = datasetNamespace(state);
      return {
        queryKey: ["datasets", "detail", namespace, state.name],
        queryFn: () => api.getDataset(namespace, state.name),
        enabled: listQuery.isSuccess,
        staleTime: 30_000,
      };
    }),
  });

  const detailByKey = useMemo(() => {
    const map = new Map<string, DatasetDetail>();
    detailQueries.forEach((query, index) => {
      if (query.data) {
        const state = rows[index];
        if (!state) {
          return;
        }
        map.set(datasetKey(datasetNamespace(state), state.name), query.data as DatasetDetail);
      }
    });
    return map;
  }, [detailQueries, rows]);

  const selectedDetailQuery = useQuery({
    queryKey: ["datasets", "detail", selectedNamespace, selectedName],
    queryFn: () => api.getDataset(selectedNamespace, selectedName!),
    enabled: Boolean(selectedName),
    staleTime: 30_000,
    placeholderData: selectedName
      ? detailByKey.get(datasetKey(selectedNamespace, selectedName))
      : undefined,
  });

  const selectedDetail = selectedDetailQuery.data;

  function selectDataset(state: DatasetState) {
    const namespace = datasetNamespace(state);
    void navigate({
      to: "/datasets",
      search: {
        status: statusFilter === "all" ? undefined : statusFilter,
        namespace: namespace || undefined,
        name: state.name,
      },
    });
  }

  function setStatusFilter(next: DatasetStatusFilter) {
    void navigate({
      to: "/datasets",
      search: {
        status: next === "all" ? undefined : next,
        namespace: selectedNamespace || undefined,
        name: selectedName,
      },
    });
  }

  return (
    <div className="space-y-5" data-testid="datasets-page">
      <PageHeader total={listQuery.data?.total} />

      <StatusFilterBar value={statusFilter} onChange={setStatusFilter} />

      <div className="grid gap-5 xl:grid-cols-[minmax(0,1fr)_390px]">
        <section className="min-w-0 rounded-md border border-border/50 bg-card">
          <DatasetBoard
            rows={rows}
            detailByKey={detailByKey}
            selectedKey={selectedName ? datasetKey(selectedNamespace, selectedName) : undefined}
            isLoading={listQuery.isLoading}
            error={listQuery.error}
            statusFilter={statusFilter}
            onSelect={selectDataset}
          />
          {!listQuery.isLoading && !listQuery.error && total > PAGE_SIZE ? (
            <BoardPagination
              page={page}
              totalPages={totalPages}
              rangeStart={total === 0 ? 0 : page * PAGE_SIZE + 1}
              rangeEnd={Math.min((page + 1) * PAGE_SIZE, total)}
              total={total}
              onPrev={() => setPage((current) => Math.max(0, current - 1))}
              onNext={() => setPage((current) => Math.min(totalPages - 1, current + 1))}
            />
          ) : null}
        </section>

        <aside className="space-y-4">
          <DatasetDetailPanel
            namespace={selectedNamespace}
            name={selectedName}
            detail={selectedDetail}
            isLoading={selectedDetailQuery.isLoading}
            error={selectedDetailQuery.error}
          />
        </aside>
      </div>
    </div>
  );
}

function PageHeader({ total }: { total: number | undefined }) {
  return (
    <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
      <div>
        <div className="mb-1 text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3">
          Freshness
        </div>
        <h1 className="text-xl font-semibold tracking-tight text-text-1">Datasets</h1>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="outline" className="font-mono text-[10px]">
          {total ?? 0} datasets
        </Badge>
        <Badge variant="outline" className="text-[10px]">
          Live SLO state
        </Badge>
      </div>
    </div>
  );
}

function StatusFilterBar({
  value,
  onChange,
}: {
  value: DatasetStatusFilter;
  onChange: (value: DatasetStatusFilter) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-md border border-border/50 bg-card p-1">
      {DATASET_STATUS_FILTERS.map((filter) => {
        const active = value === filter.key;
        return (
          <button
            key={filter.key}
            type="button"
            onClick={() => onChange(filter.key)}
            className={cn(
              "rounded px-2.5 py-1 text-[11px] font-medium transition-colors",
              active
                ? "bg-obsidian text-text-1 shadow-sm"
                : "text-text-3 hover:bg-obsidian/50 hover:text-text-2",
            )}
          >
            {filter.label}
          </button>
        );
      })}
    </div>
  );
}

function DatasetBoard({
  rows,
  detailByKey,
  selectedKey,
  isLoading,
  error,
  statusFilter,
  onSelect,
}: {
  rows: DatasetState[];
  detailByKey: Map<string, DatasetDetail>;
  selectedKey: string | undefined;
  isLoading: boolean;
  error: unknown;
  statusFilter: DatasetStatusFilter;
  onSelect: (state: DatasetState) => void;
}) {
  if (isLoading) {
    return (
      <div className="space-y-0 divide-y divide-border/40">
        {Array.from({ length: 6 }).map((_, index) => (
          <Skeleton key={index} className="h-16 rounded-none" />
        ))}
      </div>
    );
  }

  if (error) {
    return (
      <div className="p-6">
        <EmptyState
          title="Datasets unavailable"
          subtitle={error instanceof Error ? error.message : "The dataset endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      </div>
    );
  }

  if (rows.length === 0) {
    return (
      <EmptyState
        title={statusFilter === "all" ? "No datasets yet" : "No datasets match"}
        subtitle={
          statusFilter === "all"
            ? "Declared or observed datasets will appear here once jobs are applied."
            : "Try another freshness status filter."
        }
        icon={<Database className="h-12 w-12 text-text-3" />}
        className="py-20"
      />
    );
  }

  return (
    <div className="overflow-x-auto">
      <div
        className="grid min-w-[980px] items-center border-b border-border/50 bg-obsidian/30 px-4 py-2"
        style={{ gridTemplateColumns: "1.45fr 128px 190px 190px 1.1fr 110px" }}
      >
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Dataset</span>
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Status</span>
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Staleness / SLO</span>
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Producer</span>
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Reason</span>
        <span className="text-[10px] font-semibold uppercase tracking-[0.1em] text-text-3">Observed</span>
      </div>
      <div className="min-w-[980px] divide-y divide-border/40">
        {rows.map((state) => {
          const namespace = datasetNamespace(state);
          const key = datasetKey(namespace, state.name);
          const detail = detailByKey.get(key);
          const selected = selectedKey === key;
          return (
            <div
              key={key}
              role="button"
              tabIndex={0}
              data-testid="dataset-row"
              data-state={selected ? "selected" : undefined}
              onClick={() => onSelect(state)}
              onKeyDown={(event) => {
                if (event.key === "Enter" || event.key === " ") {
                  event.preventDefault();
                  onSelect(state);
                }
              }}
              className={cn(
                "grid w-full cursor-pointer items-center px-4 text-left transition-colors hover:bg-obsidian/60 focus:outline-none focus:ring-1 focus:ring-cyan/40",
                selected && "bg-cyan/5 shadow-[inset_2px_0_0_hsl(var(--cyan-glow))]",
              )}
              style={{ gridTemplateColumns: "1.45fr 128px 190px 190px 1.1fr 110px", minHeight: "64px" }}
            >
              <DatasetIdentity state={state} />
              <div className="py-3">
                <FreshnessStatusChip status={state.status} />
              </div>
              <div className="py-3 pr-4">
                <StalenessBar state={state} detail={detail} />
              </div>
              <div className="min-w-0 py-3 pr-4">
                <ProducingJobCell detail={detail} />
              </div>
              <div className="min-w-0 py-3 pr-4">
                <ReasonCell status={state.status} reason={state.reason || detail?.last_decision?.reason} />
              </div>
              <div className="py-3 text-xs text-text-3">
                <ObservedAt state={state} />
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function BoardPagination({
  page,
  totalPages,
  rangeStart,
  rangeEnd,
  total,
  onPrev,
  onNext,
}: {
  page: number;
  totalPages: number;
  rangeStart: number;
  rangeEnd: number;
  total: number;
  onPrev: () => void;
  onNext: () => void;
}) {
  return (
    <div
      className="flex items-center justify-between gap-3 border-t border-border/50 px-4 py-2.5 text-xs text-text-3"
      data-testid="datasets-pagination"
    >
      <span data-testid="datasets-pagination-range">
        Showing {rangeStart}-{rangeEnd} of {total}
      </span>
      <div className="flex items-center gap-2">
        <Button
          variant="outline"
          size="icon"
          className="h-7 w-7"
          onClick={onPrev}
          disabled={page === 0}
          aria-label="Previous page"
        >
          <ChevronLeft className="h-4 w-4" />
        </Button>
        <span className="px-1 tabular-nums">
          Page {page + 1} of {totalPages}
        </span>
        <Button
          variant="outline"
          size="icon"
          className="h-7 w-7"
          onClick={onNext}
          disabled={page >= totalPages - 1}
          aria-label="Next page"
        >
          <ChevronRight className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}

function DatasetIdentity({ state }: { state: DatasetState }) {
  const namespace = datasetNamespace(state);
  return (
    <div className="min-w-0 py-3 pr-4">
      <div className="truncate font-mono text-sm font-medium text-text-1" title={state.name}>
        {state.name}
      </div>
      <div className="mt-1 flex items-center gap-2 text-[10px] text-text-4">
        <span className="font-mono">{displayNamespace(namespace)}</span>
        {state.watermark ? (
          <span className="truncate font-mono" title={state.watermark}>
            wm {state.watermark}
          </span>
        ) : (
          <span>no watermark</span>
        )}
      </div>
    </div>
  );
}

function ProducingJobCell({ detail }: { detail: DatasetDetail | undefined }) {
  const producer = detail?.producing_job;
  if (!producer) {
    return <span className="text-xs text-text-4">No Caesium producer</span>;
  }
  return (
    <div className="min-w-0 space-y-1">
      <Link
        to="/jobs/$jobId"
        params={{ jobId: producer.id }}
        className="block truncate text-sm font-medium text-text-1 hover:text-cyan-glow"
        title={producer.alias}
        onClick={(event) => event.stopPropagation()}
      >
        {producer.alias}
      </Link>
      {producer.step_name ? (
        <div className="truncate font-mono text-[10px] text-text-4" title={producer.step_name}>
          {producer.step_name}
        </div>
      ) : null}
    </div>
  );
}

function ReasonCell({ status, reason }: { status: DatasetStatus; reason: string | undefined }) {
  if (!reason) {
    return <span className="text-xs text-text-4">-</span>;
  }
  const tone = freshnessTone(status);
  return (
    <div className={cn("truncate text-xs", status === "stale-upstream" ? tone.textClass : "text-text-3")} title={reason}>
      {reason}
    </div>
  );
}

function ObservedAt({ state }: { state: DatasetState }) {
  const observedAt = effectiveObservedAt(state);
  if (!observedAt) {
    return <span className="text-text-4">never</span>;
  }
  return <RelativeTime date={observedAt} />;
}

function StalenessBar({
  state,
  detail,
}: {
  state: DatasetState;
  detail: DatasetDetail | undefined;
}) {
  const [nowMs] = useState(() => Date.now());
  const slo = freshnessSLO(detail);
  const percent = stalenessPercent(state, slo, nowMs);
  const observedAt = effectiveObservedAt(state);
  const tone = freshnessTone(state.status);

  if (!slo) {
    return (
      <div className="space-y-1">
        <div className="flex items-center justify-between gap-2 text-[11px]">
          <span className="text-text-4">No SLO</span>
          <span className="font-mono text-text-4">-</span>
        </div>
        <div className="h-2 rounded-full bg-graphite/60" />
      </div>
    );
  }

  if (percent == null || !observedAt) {
    return (
      <div className="space-y-1">
        <div className="flex items-center justify-between gap-2 text-[11px]">
          <span className="text-text-4">Awaiting first observation</span>
          <span className="font-mono text-text-3">{slo}</span>
        </div>
        <div className="h-2 overflow-hidden rounded-full bg-graphite/60">
          <div className="h-full w-[8%] rounded-full bg-text-4" />
        </div>
      </div>
    );
  }

  const ageMs = nowMs - new Date(observedAt).getTime();
  const width = Math.max(4, Math.min(100, percent));
  const label = `${formatDurationShort(ageMs)} / ${slo}`;
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2 text-[11px]">
        <span className={cn("font-mono tabular-nums", tone.textClass)}>{label}</span>
        <span className="font-mono text-text-4">{Math.round(percent)}%</span>
      </div>
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.min(100, Math.round(percent))}
        aria-label={`Dataset staleness ${label}`}
        className="h-2 overflow-hidden rounded-full bg-graphite/60"
      >
        <div className={cn("h-full rounded-full transition-[width] duration-500", tone.barClass)} style={{ width: `${width}%` }} />
      </div>
    </div>
  );
}

function DatasetDetailPanel({
  namespace,
  name,
  detail,
  isLoading,
  error,
}: {
  namespace: string | undefined;
  name: string | undefined;
  detail: DatasetDetail | undefined;
  isLoading: boolean;
  error: unknown;
}) {
  if (!name) {
    return (
      <div className="rounded-md border border-border/50 bg-card p-4">
        <EmptyState
          title="Select a dataset"
          subtitle="Choose a row to inspect the producer, SLO, and derivation audit."
          icon={<Database className="h-12 w-12 text-text-3" />}
          className="py-12"
        />
      </div>
    );
  }

  if (isLoading && !detail) {
    return (
      <div className="rounded-md border border-border/50 bg-card p-4">
        <Skeleton className="h-8 w-52" />
        <Skeleton className="mt-4 h-28 w-full" />
        <Skeleton className="mt-4 h-40 w-full" />
      </div>
    );
  }

  if (error && !detail) {
    return (
      <div className="rounded-md border border-danger/30 bg-danger/5 p-4">
        <EmptyState
          title="Dataset detail unavailable"
          subtitle={error instanceof Error ? error.message : "The dataset detail endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      </div>
    );
  }

  const state = detail?.state;
  const producer = detail?.producing_job;
  const slo = detail?.slo;

  return (
    <div className="rounded-md border border-border/50 bg-card p-4" data-testid="dataset-detail-panel">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3">
            Dataset detail
          </div>
          <h2 className="mt-1 truncate font-mono text-base font-semibold text-text-1" title={name}>
            {name}
          </h2>
          <div className="mt-1 font-mono text-[11px] text-text-4">{displayNamespace(namespace)}</div>
        </div>
        <FreshnessStatusChip status={state?.status} />
      </div>

      <dl className="mt-4 grid gap-3 text-xs">
        <MetadataRow label="Watermark" value={state?.watermark || "-"} mono />
        <MetadataRow label="Reason" value={state?.reason || detail?.last_decision?.reason || "-"} />
        <MetadataRow label="Freshness" value={slo?.freshness || "-"} mono />
        <MetadataRow label="Max staleness" value={slo?.max_staleness || "-"} mono />
        <MetadataRow label="Expected every" value={slo?.expected_every || "-"} mono />
        <MetadataRow label="Direction" value={detail?.declaration?.direction || "-"} />
        {detail?.declaration?.watermark_key ? (
          <MetadataRow label="Watermark key" value={detail.declaration.watermark_key} mono />
        ) : null}
      </dl>

      {producer ? (
        <div className="mt-4 rounded-md border border-border/50 bg-obsidian/30 p-3">
          <div className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.14em] text-text-3">
            <GitBranch className="h-3 w-3" />
            Producer
          </div>
          <Link
            to="/jobs/$jobId"
            params={{ jobId: producer.id }}
            className="mt-2 block truncate text-sm font-medium text-cyan-glow hover:underline"
          >
            {producer.alias}
          </Link>
          {producer.step_name ? (
            <div className="mt-1 font-mono text-[11px] text-text-4">{producer.step_name}</div>
          ) : null}
        </div>
      ) : null}

      {detail?.last_decision ? (
        <div className="mt-4 rounded-md border border-border/50 bg-obsidian/30 p-3">
          <div className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-[0.14em] text-text-3">
            <History className="h-3 w-3" />
            Last decision
          </div>
          <div className="mt-2 text-sm text-text-1">{detail.last_decision.decision.replaceAll("_", " ")}</div>
          {detail.last_decision.reason ? (
            <div className="mt-1 text-xs text-text-3">{detail.last_decision.reason}</div>
          ) : null}
        </div>
      ) : null}

      <div className="mt-5 border-t border-border/50 pt-4">
        <div className="mb-3 flex items-center justify-between gap-3">
          <div className="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3">
            Derivations
          </div>
          {state?.last_run_id ? (
            <span className="font-mono text-[10px] text-text-4">run {shortId(state.last_run_id)}</span>
          ) : null}
        </div>
        <DerivationsPanel namespace={namespace} name={name} producingJob={producer} />
      </div>
    </div>
  );
}

function MetadataRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="grid grid-cols-[120px_minmax(0,1fr)] gap-3">
      <dt className="text-text-4">{label}</dt>
      <dd className={cn("truncate text-text-2", mono && "font-mono")} title={value}>
        {value}
      </dd>
    </div>
  );
}

function freshnessSLO(detail: DatasetDetail | undefined): string | undefined {
  return (
    detail?.slo?.freshness ||
    detail?.slo?.expected_every ||
    detail?.slo?.max_staleness ||
    detail?.declaration?.freshness ||
    detail?.declaration?.expected_every ||
    detail?.declaration?.max_staleness
  );
}
