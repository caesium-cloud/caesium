import { type FormEvent, memo, useCallback, useMemo, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import ReactFlow, {
  Background,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  Handle,
  MarkerType,
  Position,
  getSmoothStepPath,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeProps,
} from "reactflow";
import "reactflow/dist/style.css";
import dagre from "dagre";
import { AlertTriangle, ArrowLeft, Database, GitBranch, Search, ShieldAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type ImpactNode } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { cn, shortId } from "@/lib/utils";

const lineageRouteApi = getRouteApi("/lineage");
const nodeWidth = 320;
const nodeHeight = 150;

type LineageSearch = {
  namespace: string | undefined;
  name: string | undefined;
};

type LineageNodeData = {
  kind: "root" | "downstream";
  namespace: string;
  name: string;
  jobId?: string;
  jobAlias?: string;
  producingStep?: string;
  lastSeen?: string;
  depth?: number;
};

export function LineageRoutePage() {
  const search = lineageRouteApi.useSearch() as LineageSearch;
  const initialNamespace = search.namespace ?? "";
  const initialName = search.name ?? "";

  return (
    <LineageGraph
      key={`${initialNamespace}:${initialName}`}
      initialNamespace={initialNamespace}
      initialName={initialName}
    />
  );
}

export function LineageGraph({
  initialNamespace,
  initialName,
}: {
  initialNamespace: string;
  initialName: string;
}) {
  const navigate = useNavigate();
  const principal = usePrincipal();
  const namespace = cleanParam(initialNamespace) ?? "";
  const name = cleanParam(initialName) ?? "";
  const [namespaceInput, setNamespaceInput] = useState(namespace);
  const [nameInput, setNameInput] = useState(name);
  const hasDataset = namespace !== "" && name !== "";
  const isScoped = principal.isScoped;

  const impactQuery = useQuery({
    queryKey: ["lineage", "impact", namespace, name],
    queryFn: () => api.getLineageImpact({ namespace, name }),
    enabled: hasDataset && !isScoped,
  });

  const graph = useMemo(() => {
    if (!impactQuery.data) {
      return { nodes: [] as Node<LineageNodeData>[], edges: [] as Edge[] };
    }
    return buildGraph(impactQuery.data.root_namespace, impactQuery.data.root_name, impactQuery.data.downstream ?? []);
  }, [impactQuery.data]);

  function applyDataset(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void navigate({
      to: "/lineage",
      search: buildSearch(namespaceInput, nameInput),
    });
  }

  const handleNodeClick = useCallback(
    (_event: React.MouseEvent, node: Node<LineageNodeData>) => {
      if (node.data.kind !== "downstream" || !node.data.jobId) {
        return;
      }
      void navigate({
        to: "/jobs/$jobId",
        params: { jobId: node.data.jobId },
      });
    },
    [navigate],
  );

  // Presently unreachable for scoped API keys: UI login calls GET /auth/whoami,
  // which scoped keys receive as 403, and whoami does not expose a scope marker
  // for already-authenticated principals. Keep this as the future scoped-session
  // guard for the global lineage graph.
  if (isScoped) {
    return (
      <div className="space-y-5" data-testid="lineage-container">
        <LineageBreadcrumb />
        <LineageHeader />
        <DatasetForm
          namespaceInput={namespaceInput}
          nameInput={nameInput}
          onNamespaceInput={setNamespaceInput}
          onNameInput={setNameInput}
          onSubmit={applyDataset}
        />
        <div data-testid="lineage-scoped-denied">
          <EmptyState
            title="cross-job lineage requires an unscoped key"
            subtitle="Scoped API keys are limited to job-local routes and cannot query the global lineage-impact graph."
            icon={<ShieldAlert className="h-12 w-12 text-warning" />}
          />
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-5" data-testid="lineage-container">
      <LineageBreadcrumb />
      <LineageHeader />
      <DatasetForm
        namespaceInput={namespaceInput}
        nameInput={nameInput}
        onNamespaceInput={setNamespaceInput}
        onNameInput={setNameInput}
        onSubmit={applyDataset}
      />

      {!hasDataset ? (
        <EmptyState
          title="No dataset selected"
          icon={<Database className="h-12 w-12 text-text-3" />}
        />
      ) : null}

      {impactQuery.isLoading ? (
        <div className="space-y-4">
          <Skeleton className="h-8 w-[240px]" />
          <Skeleton className="h-[500px] w-full" />
        </div>
      ) : null}

      {impactQuery.error ? (
        <EmptyState
          title="Lineage impact unavailable"
          subtitle={impactQuery.error instanceof Error ? impactQuery.error.message : "The lineage-impact endpoint returned an error."}
          icon={<AlertTriangle className="h-12 w-12 text-danger" />}
        />
      ) : null}

      {impactQuery.data && impactQuery.data.downstream.length === 0 ? (
        <div data-testid="lineage-empty-state">
          <EmptyState
            title="no downstream impact recorded yet (lineage datasets populate as jobs declare/consume outputs)"
            icon={<GitBranch className="h-12 w-12 text-text-3" />}
          />
        </div>
      ) : null}

      {impactQuery.data && impactQuery.data.downstream.length > 0 ? (
        <>
          <div className="grid gap-3 md:grid-cols-3">
            <MetadataCell label="Root Namespace" value={impactQuery.data.root_namespace} />
            <MetadataCell label="Root Dataset" value={impactQuery.data.root_name} />
            <MetadataCell label="Downstream Nodes" value={String(impactQuery.data.downstream.length)} />
          </div>
          <div className="relative h-[560px] min-h-[500px] w-full overflow-hidden rounded-lg bg-dag-bg" data-testid="lineage-graph">
            <ReactFlow
              nodes={graph.nodes}
              edges={graph.edges}
              nodeTypes={nodeTypes}
              edgeTypes={edgeTypes}
              onNodeClick={handleNodeClick}
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

function lineageImpactNodeTestId(namespace: string, name: string, jobId?: string) {
  return `lineage-impact-node:${namespace}:${name}:${jobId ?? "unknown-job"}`;
}

function LineageHeader() {
  return (
    <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
      <div className="min-w-0">
        <div className="mb-1 text-[10px] font-semibold uppercase tracking-[0.18em] text-text-3">
          Lineage
        </div>
        <h1 className="text-xl font-semibold tracking-tight text-text-1">Lineage impact</h1>
      </div>
      <Badge variant="outline" className="text-[10px]">
        Downstream only
      </Badge>
    </div>
  );
}

function LineageBreadcrumb() {
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
      <span>Lineage</span>
    </div>
  );
}

function DatasetForm({
  namespaceInput,
  nameInput,
  onNamespaceInput,
  onNameInput,
  onSubmit,
}: {
  namespaceInput: string;
  nameInput: string;
  onNamespaceInput: (value: string) => void;
  onNameInput: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-sm">Dataset</CardTitle>
      </CardHeader>
      <CardContent>
        <form className="grid gap-3 md:grid-cols-[0.5fr_1fr_auto]" onSubmit={onSubmit}>
          <LabeledInput
            id="lineage-namespace"
            label="Namespace"
            value={namespaceInput}
            placeholder="caesium"
            testId="lineage-namespace-input"
            onChange={onNamespaceInput}
          />
          <LabeledInput
            id="lineage-name"
            label="Name"
            value={nameInput}
            placeholder="job.extract.output"
            testId="lineage-name-input"
            onChange={onNameInput}
          />
          <div className="flex items-end">
            <Button
              type="submit"
              size="sm"
              className="h-9 w-full md:w-auto"
              data-testid="lineage-submit"
              disabled={namespaceInput.trim() === "" || nameInput.trim() === ""}
            >
              <Search className="h-3.5 w-3.5" />
              Inspect
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function LabeledInput({
  id,
  label,
  value,
  placeholder,
  testId,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  placeholder: string;
  testId: string;
  onChange: (value: string) => void;
}) {
  return (
    <label htmlFor={id} className="space-y-1.5">
      <span className="text-[11px] font-semibold uppercase tracking-[0.14em] text-text-3">
        {label}
      </span>
      <input
        id={id}
        value={value}
        placeholder={placeholder}
        data-testid={testId}
        onChange={(event) => onChange(event.target.value)}
        className="h-9 w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-sm text-foreground shadow-sm outline-none transition-colors placeholder:text-muted-foreground focus:border-primary focus:ring-1 focus:ring-ring"
      />
    </label>
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

function buildGraph(rootNamespace: string, rootName: string, downstream: ImpactNode[]) {
  const rootId = `root:${rootNamespace}:${rootName}`;
  const initialNodes: Node<LineageNodeData>[] = [
    {
      id: rootId,
      type: "lineage",
      data: {
        kind: "root",
        namespace: rootNamespace,
        name: rootName,
      },
      position: { x: 0, y: 0 },
    },
  ];

  const initialEdges: Edge[] = [];
  downstream.forEach((node, index) => {
    const nodeId = `impact:${node.dataset_namespace}:${node.dataset_name}:${node.job_id}:${index}`;
    initialNodes.push({
      id: nodeId,
      type: "lineage",
      data: {
        kind: "downstream",
        namespace: node.dataset_namespace,
        name: node.dataset_name,
        jobId: node.job_id,
        jobAlias: node.job_alias,
        producingStep: node.producing_step,
        lastSeen: node.last_seen,
        depth: node.depth,
      },
      position: { x: 0, y: 0 },
    });
    initialEdges.push({
      id: `lineage-edge-${index}`,
      source: rootId,
      target: nodeId,
      type: "lineage",
      data: {
        depth: node.depth,
      },
      markerEnd: {
        type: MarkerType.ArrowClosed,
        width: 20,
        height: 20,
        color: "hsl(var(--running))",
      },
      style: {
        strokeWidth: 2,
        stroke: "hsl(var(--running))",
      },
    });
  });

  return getLayoutedElements(initialNodes, initialEdges);
}

function getLayoutedElements(nodes: Node<LineageNodeData>[], edges: Edge[], direction = "LR") {
  const dagreGraph = new dagre.graphlib.Graph();
  dagreGraph.setDefaultEdgeLabel(() => ({}));
  dagreGraph.setGraph({
    rankdir: direction,
    nodesep: 150,
    ranksep: 200,
    marginx: 50,
    marginy: 50,
    ranker: "network-simplex",
  });

  nodes.forEach((node) => {
    dagreGraph.setNode(node.id, { width: nodeWidth, height: nodeHeight });
  });
  edges.forEach((edge) => {
    dagreGraph.setEdge(edge.source, edge.target);
  });

  dagre.layout(dagreGraph);

  return {
    nodes: nodes.map((node) => {
      const layoutNode = dagreGraph.node(node.id);
      return {
        ...node,
        targetPosition: direction === "LR" ? Position.Left : Position.Top,
        sourcePosition: direction === "LR" ? Position.Right : Position.Bottom,
        position: {
          x: layoutNode.x - nodeWidth / 2,
          y: layoutNode.y - nodeHeight / 2,
        },
      };
    }),
    edges,
  };
}

const LineageDatasetNode = memo(({ data }: NodeProps<LineageNodeData>) => {
  const isRoot = data.kind === "root";
  const testId = isRoot ? "lineage-root-node" : lineageImpactNodeTestId(data.namespace, data.name, data.jobId);
  const hop = typeof data.depth === "number" ? data.depth + 1 : undefined;

  return (
    <div
      data-testid={testId}
      data-lineage-node-kind={isRoot ? "root" : "impact"}
      data-dataset-namespace={data.namespace}
      data-dataset-name={data.name}
      data-job-id={data.jobId}
      data-job-alias={data.jobAlias}
      className={cn(
        "relative h-[150px] w-[320px] overflow-hidden rounded-lg border-2 px-4 py-3 shadow-sm transition-colors",
        isRoot
          ? "border-gold/50 bg-[linear-gradient(155deg,hsl(var(--gold)/0.18),hsl(var(--node-surface)/0.95)_60%)]"
          : "cursor-pointer border-caesium-cyan/45 bg-[linear-gradient(155deg,hsl(var(--caesium-cyan)/0.18),hsl(var(--node-surface)/0.95)_60%)] hover:border-caesium-cyan/80",
      )}
      title={isRoot ? `${data.namespace}/${data.name}` : `Open job ${data.jobAlias ?? data.jobId}`}
    >
      {!isRoot ? (
        <Handle type="target" position={Position.Left} className="h-3 w-3 border-2 border-dag-bg bg-caesium-cyan" />
      ) : null}
      <Handle type="source" position={Position.Right} className="h-3 w-3 border-2 border-dag-bg bg-caesium-cyan" />

      <div className="flex h-full flex-col justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Database className={cn("h-4 w-4 shrink-0", isRoot ? "text-gold" : "text-running")} />
            <Badge variant="outline" className="text-[10px]">
              {isRoot ? "Root dataset" : "Downstream output"}
            </Badge>
            {hop ? (
              <span className="rounded border border-running/30 bg-running/10 px-1.5 py-0.5 text-[10px] font-semibold text-running">
                Hop {hop}
              </span>
            ) : null}
          </div>
          <div className="mt-3 truncate font-mono text-sm font-semibold text-text-1" title={data.name}>
            {data.name}
          </div>
          <div className="mt-1 truncate font-mono text-[11px] text-text-3" title={data.namespace}>
            {data.namespace}
          </div>
        </div>

        {isRoot ? (
          <div className="text-xs text-text-3">Impact root</div>
        ) : (
          <div className="space-y-1 text-xs text-text-3">
            <div className="truncate">
              Job <span className="font-mono text-text-1">{data.jobAlias ?? shortId(data.jobId)}</span>
            </div>
            {data.producingStep ? (
              <div className="truncate">
                Step <span className="font-mono text-text-1">{data.producingStep}</span>
              </div>
            ) : null}
            {data.lastSeen ? (
              <div className="truncate font-mono text-[10px]" title={data.lastSeen}>
                {new Date(data.lastSeen).toLocaleString()}
              </div>
            ) : null}
          </div>
        )}
      </div>
    </div>
  );
});

LineageDatasetNode.displayName = "LineageDatasetNode";

const LineageImpactEdge = memo(({
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
}: EdgeProps) => {
  const [edgePath, labelX, labelY] = getSmoothStepPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    borderRadius: 16,
  });
  const depth = typeof data?.depth === "number" ? data.depth : 0;

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} markerEnd={markerEnd} />
      <EdgeLabelRenderer>
        <div
          data-testid="lineage-edge"
          data-edge-id={id}
          className="nodrag nopan rounded-full border border-running/35 bg-card/95 px-2 py-1 text-[10px] font-semibold text-running shadow-sm"
          style={{
            position: "absolute",
            transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
          }}
        >
          {depth === 0 ? "downstream" : `depth ${depth + 1}`}
        </div>
      </EdgeLabelRenderer>
    </>
  );
});

LineageImpactEdge.displayName = "LineageImpactEdge";

const nodeTypes = {
  lineage: LineageDatasetNode,
};

const edgeTypes = {
  lineage: LineageImpactEdge,
};

function buildSearch(namespace: string, name: string) {
  return {
    namespace: cleanParam(namespace),
    name: cleanParam(name),
  };
}

function cleanParam(value: string | undefined) {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}
