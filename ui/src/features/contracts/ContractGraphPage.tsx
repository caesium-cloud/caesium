import { type FormEvent, memo, useMemo, useState } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import ReactFlow, {
  Background,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  Handle,
  Position,
  getSmoothStepPath,
  type EdgeProps,
  type NodeProps,
} from "reactflow";
import "reactflow/dist/style.css";
import {
  AlertTriangle,
  ArrowLeft,
  Database,
  GitBranch,
  RefreshCw,
  Search,
  ShieldAlert,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { NotFoundState } from "@/components/not-found-state";
import { Skeleton } from "@/components/ui/skeleton";
import { ApiError, api } from "@/lib/api";
import { cn, formatUTCTimestamp, shortId } from "@/lib/utils";
import {
  buildContractFlow,
  cleanDatasetFilter,
  contractNodeTestId,
  type ContractFlowEdgeData,
  type ContractFlowNodeData,
} from "./contractGraphModel";

type ContractSearch = {
  dataset?: string;
};

export function ContractGraphPage() {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as ContractSearch;
  const dataset = typeof search.dataset === "string" ? search.dataset : "";

  return (
    <ContractGraph
      key={dataset}
      initialDataset={dataset}
      onDatasetSubmit={(nextDataset) => {
        void navigate({
          to: "/contracts",
          search: { dataset: cleanDatasetFilter(nextDataset) },
        });
      }}
    />
  );
}

export function ContractGraph({
  initialDataset,
  onDatasetSubmit,
}: {
  initialDataset: string;
  onDatasetSubmit: (dataset: string) => void;
}) {
  const queryClient = useQueryClient();
  const datasetFilter = cleanDatasetFilter(initialDataset);
  const [datasetInput, setDatasetInput] = useState(initialDataset);

  const featuresQuery = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });
  const contractEnabled = featuresQuery.data?.contract_enforcement_enabled === true;

  const graphQuery = useQuery({
    queryKey: ["contracts", "graph", datasetFilter ?? ""],
    queryFn: () => api.getContractGraph({ dataset: datasetFilter }),
    enabled: contractEnabled,
  });

  const jobsQuery = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    enabled: contractEnabled,
    staleTime: 30_000,
  });

  const jobIdsByAlias = useMemo(() => {
    const map = new Map<string, string>();
    (jobsQuery.data ?? []).forEach((job) => {
      map.set(job.alias, job.id);
    });
    return map;
  }, [jobsQuery.data]);

  const graph = useMemo(() => {
    if (!graphQuery.data) {
      return { nodes: [], edges: [] };
    }
    return buildContractFlow(graphQuery.data, jobIdsByAlias);
  }, [graphQuery.data, jobIdsByAlias]);

  const findingCount = useMemo(
    () => (graphQuery.data?.edges ?? []).reduce((total, edge) => total + (edge.findings?.length ?? 0), 0),
    [graphQuery.data?.edges],
  );

  function applyDatasetFilter(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    onDatasetSubmit(datasetInput);
  }

  if (featuresQuery.isLoading) {
    return <ContractGraphSkeleton />;
  }

  if (featuresQuery.error) {
    return (
      <EmptyState
        title="Contract features unavailable"
        subtitle={featuresQuery.error instanceof Error ? featuresQuery.error.message : "The system feature endpoint returned an error."}
        icon={<AlertTriangle className="h-12 w-12 text-danger" />}
      />
    );
  }

  if (!contractEnabled) {
    return (
      <NotFoundState
        title="Contracts disabled"
        subtitle="Contract enforcement is not enabled on this Caesium server."
      />
    );
  }

  if (graphQuery.error) {
    if (graphQuery.error instanceof ApiError && graphQuery.error.status === 404) {
      return (
        <NotFoundState
          title="Contracts disabled"
          subtitle="The contract graph endpoint is not available on this Caesium server."
        />
      );
    }

    return (
      <div className="space-y-5" data-testid="contracts-page">
        <ContractBreadcrumb />
        <ContractHeader onRefresh={() => queryClient.invalidateQueries({ queryKey: ["contracts"] })} />
        <DatasetFilterForm
          datasetInput={datasetInput}
          onDatasetInput={setDatasetInput}
          onSubmit={applyDatasetFilter}
        />
        <EmptyState
          title="Contract graph unavailable"
          subtitle={graphQuery.error instanceof Error ? graphQuery.error.message : "The contract graph endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      </div>
    );
  }

  const hasEdges = (graphQuery.data?.edges.length ?? 0) > 0;

  return (
    <div className="space-y-5" data-testid="contracts-page">
      <ContractBreadcrumb />
      <ContractHeader onRefresh={() => queryClient.invalidateQueries({ queryKey: ["contracts"] })} />
      <DatasetFilterForm
        datasetInput={datasetInput}
        onDatasetInput={setDatasetInput}
        onSubmit={applyDatasetFilter}
      />

      {graphQuery.isLoading ? (
        <div className="space-y-4">
          <Skeleton className="h-8 w-[260px]" />
          <Skeleton className="h-[560px] w-full" />
        </div>
      ) : null}

      {graphQuery.data && !hasEdges ? (
        <div data-testid="contracts-empty-state">
          <EmptyState
            title="No contract edges yet"
            subtitle="Contracts appear when job definitions declare dataset schemas or when lifecycle paramMapping chains infer producer and consumer relationships."
            icon={<ShieldAlert className="h-12 w-12 text-text-3" />}
          />
        </div>
      ) : null}

      {graphQuery.data && hasEdges ? (
        <>
          <div className="grid gap-3 md:grid-cols-4">
            <MetadataCell label="Nodes" value={String(graphQuery.data.nodes.length)} />
            <MetadataCell label="Edges" value={String(graphQuery.data.edges.length)} />
            <MetadataCell label="Findings" value={String(findingCount)} />
            <MetadataCell label="Filter" value={datasetFilter ?? "all datasets"} />
          </div>
          <ContractLegend />
          <div
            className="relative h-[620px] min-h-[520px] w-full overflow-hidden rounded-lg bg-dag-bg"
            data-testid="contracts-graph"
          >
            <ReactFlow
              nodes={graph.nodes}
              edges={graph.edges}
              nodeTypes={nodeTypes}
              edgeTypes={edgeTypes}
              fitView
              fitViewOptions={{ padding: 0.2 }}
              minZoom={0.1}
              maxZoom={1.5}
            >
              <Background gap={20} />
              <Controls />
            </ReactFlow>
          </div>
        </>
      ) : null}
    </div>
  );
}

function ContractGraphSkeleton() {
  return (
    <div className="space-y-4">
      <Skeleton className="h-5 w-32" />
      <Skeleton className="h-10 w-80" />
      <Skeleton className="h-24 w-full" />
      <Skeleton className="h-[560px] w-full" />
    </div>
  );
}

function ContractHeader({ onRefresh }: { onRefresh: () => void }) {
  return (
    <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
      <div className="min-w-0">
        <div className="mb-1 text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3">
          Contracts
        </div>
        <h1 className="text-xl font-semibold tracking-tight text-text-1">Contract graph</h1>
      </div>
      <Button type="button" variant="outline" size="sm" onClick={onRefresh}>
        <RefreshCw className="h-3.5 w-3.5" />
        Refresh
      </Button>
    </div>
  );
}

function ContractBreadcrumb() {
  return (
    <div className="flex items-center gap-2 text-[11px] text-text-3">
      <Link
        to="/jobs"
        className="flex items-center gap-1 transition-colors hover:text-text-2"
      >
        <ArrowLeft className="h-3 w-3" />
        Jobs
      </Link>
      <span className="text-text-4">/</span>
      <span>Contracts</span>
    </div>
  );
}

function DatasetFilterForm({
  datasetInput,
  onDatasetInput,
  onSubmit,
}: {
  datasetInput: string;
  onDatasetInput: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  return (
    <Card className="border-graphite/40 bg-midnight/30">
      <CardHeader className="border-b border-border/50 pb-3">
        <CardTitle className="flex items-center gap-2 text-sm">
          <Search className="h-4 w-4 text-cyan-glow" />
          Dataset filter
        </CardTitle>
      </CardHeader>
      <CardContent className="p-4">
        <form className="grid gap-3 md:grid-cols-[minmax(0,1fr)_auto]" onSubmit={onSubmit}>
          <label htmlFor="contract-dataset-filter" className="space-y-1.5">
            <span className="text-[11px] font-semibold uppercase tracking-[0.14em] text-text-3">
              Dataset
            </span>
            <input
              id="contract-dataset-filter"
              value={datasetInput}
              placeholder="lake/customers"
              data-testid="contract-dataset-filter"
              onChange={(event) => onDatasetInput(event.target.value)}
              className="h-9 w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-sm text-foreground shadow-sm outline-none transition-colors placeholder:text-muted-foreground focus:border-primary focus:ring-1 focus:ring-ring"
            />
          </label>
          <div className="flex items-end">
            <Button type="submit" size="sm" className="h-9 w-full md:w-auto" data-testid="contract-filter-submit">
              <Search className="h-3.5 w-3.5" />
              Apply
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function MetadataCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-border/60 bg-card px-3 py-2">
      <div className="text-[10px] font-medium uppercase tracking-[0.16em] text-text-3">
        {label}
      </div>
      <div className="mt-1 truncate font-mono text-xs text-text-1" title={value}>
        {value}
      </div>
    </div>
  );
}

function ContractLegend() {
  return (
    <div
      className="flex flex-wrap items-center gap-3 rounded-md border border-border/60 bg-card/70 px-3 py-2 text-xs text-text-3"
      data-testid="contract-legend"
    >
      <LegendLine label="Declared" className="border-success" />
      <LegendLine label="Inferred" className="border-warning border-dashed" />
      <LegendLine label="Evidence" className="border-text-4 border-dotted opacity-70" />
      <span className="hidden h-4 w-px bg-border/70 sm:inline-block" aria-hidden="true" />
      <LegendVerdict label="Breaking" className="border-danger/35 bg-danger/10 text-danger" />
      <LegendVerdict label="Unknown" className="border-warning/35 bg-warning/10 text-warning" />
      <LegendVerdict label="Compatible" className="border-success/35 bg-success/10 text-success" />
    </div>
  );
}

function LegendLine({ label, className }: { label: string; className: string }) {
  return (
    <span className="inline-flex items-center gap-2">
      <span className={cn("inline-block w-10 border-t-2", className)} />
      {label}
    </span>
  );
}

function LegendVerdict({ label, className }: { label: string; className: string }) {
  return <span className={cn("rounded border px-2 py-0.5 text-[10px] font-semibold", className)}>{label}</span>;
}

const ContractNode = memo(({ data }: NodeProps<ContractFlowNodeData>) => {
  const isJob = data.kind === "job";
  const testId = contractNodeTestId(data);
  const className = cn(
    "relative block h-[138px] w-[300px] overflow-hidden rounded-lg border-2 px-4 py-3 text-left shadow-sm transition-colors",
    isJob
      ? "border-caesium-cyan/45 bg-[linear-gradient(155deg,hsl(var(--caesium-cyan)/0.16),hsl(var(--node-surface)/0.96)_62%)]"
      : "border-gold/45 bg-[linear-gradient(155deg,hsl(var(--gold)/0.16),hsl(var(--node-surface)/0.96)_62%)]",
    isJob && data.jobId ? "cursor-pointer hover:border-caesium-cyan/80" : "",
  );
  const body = (
    <>
      <Handle
        type="target"
        position={Position.Left}
        className={cn("h-3 w-3 border-2 border-dag-bg", isJob ? "bg-caesium-cyan" : "bg-gold")}
      />
      <Handle
        type="source"
        position={Position.Right}
        className={cn("h-3 w-3 border-2 border-dag-bg", isJob ? "bg-caesium-cyan" : "bg-gold")}
      />
      <div className="flex h-full flex-col justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            {isJob ? (
              <GitBranch className="h-4 w-4 shrink-0 text-running" />
            ) : (
              <Database className="h-4 w-4 shrink-0 text-gold" />
            )}
            <Badge variant="outline" className="text-[10px]">
              {isJob ? "Job" : "Dataset"}
            </Badge>
          </div>
          <div className="mt-3 truncate font-mono text-sm font-semibold text-text-1" title={data.label}>
            {data.label}
          </div>
          <div className="mt-1 truncate text-[11px] text-text-3" title={data.detail}>
            {data.detail}
          </div>
        </div>
        <div className="truncate font-mono text-[10px] text-text-4" title={data.id}>
          {shortId(data.id, 28)}
        </div>
      </div>
    </>
  );

  if (isJob && data.jobId) {
    return (
      <Link
        to="/jobs/$jobId"
        params={{ jobId: data.jobId }}
        data-testid={testId}
        data-node-kind={data.kind}
        data-job-alias={data.alias ?? data.label}
        data-job-id={data.jobId}
        className={className}
        title={`Open job ${data.label}`}
      >
        {body}
      </Link>
    );
  }

  return (
    <div
      data-testid={testId}
      data-node-kind={data.kind}
      data-job-alias={data.alias}
      data-dataset-name={data.dataset?.name}
      data-dataset-namespace={data.dataset?.namespace}
      className={className}
      title={data.label}
    >
      {body}
    </div>
  );
});

ContractNode.displayName = "ContractNode";

const ContractEdge = memo(({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
  data,
}: EdgeProps<ContractFlowEdgeData>) => {
  const [edgePath, labelX, labelY] = getSmoothStepPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    borderRadius: 16,
  });
  const verdict = data?.verdict ?? "unknown";
  const findingLabel = data?.findingCount ? `${data.findingCount} finding${data.findingCount === 1 ? "" : "s"}` : "no findings";

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} markerEnd={markerEnd} />
      <EdgeLabelRenderer>
        <div
          data-testid={data?.testId ?? `contract-edge:${id}`}
          data-edge-id={id}
          data-edge-class={data?.edgeClass}
          data-edge-verdict={verdict}
          data-edge-source={data?.sourceLabel}
          data-edge-target={data?.targetLabel}
          className={cn(
            "nodrag nopan max-w-[220px] rounded-full border bg-card/95 px-2 py-1 text-[10px] font-semibold shadow-sm",
            edgeLabelClass(data?.edgeClass, verdict),
          )}
          style={{
            position: "absolute",
            transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
          }}
          title={edgeTitle(data, findingLabel)}
        >
          <span className="uppercase">{data?.edgeClass ?? "edge"}</span>
          <span className="text-text-4"> · </span>
          <span>{data?.edgeClass === "evidence" ? "observed" : verdict}</span>
        </div>
      </EdgeLabelRenderer>
    </>
  );
});

ContractEdge.displayName = "ContractEdge";

const nodeTypes = {
  contract: ContractNode,
};

const edgeTypes = {
  contract: ContractEdge,
};

function edgeLabelClass(edgeClass: string | undefined, verdict: string) {
  if (edgeClass === "evidence") {
    return "border-text-4/40 text-text-3";
  }
  switch (verdict) {
    case "breaking":
      return "border-danger/35 bg-danger/10 text-danger";
    case "unknown":
      return "border-warning/35 bg-warning/10 text-warning";
    case "compatible":
      return "border-success/35 bg-success/10 text-success";
    default:
      return "border-text-3/30 text-text-3";
  }
}

function edgeTitle(data: ContractFlowEdgeData | undefined, findingLabel: string) {
  if (!data) {
    return findingLabel;
  }
  const parts = [
    `${data.sourceLabel} -> ${data.targetLabel}`,
    data.datasetLabel ? `dataset ${data.datasetLabel}` : undefined,
    data.lastSeen ? `last seen ${formatUTCTimestamp(data.lastSeen, data.lastSeen)}` : undefined,
    findingLabel,
  ];
  return parts.filter(Boolean).join(" | ");
}
