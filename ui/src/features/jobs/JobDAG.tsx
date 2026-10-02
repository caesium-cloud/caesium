import { useMemo, useCallback, useEffect, useState, type ReactNode } from 'react';
import ReactFlow, {
  Controls,
  Background,
  MarkerType,
  useNodesInitialized,
  useReactFlow,
  useStore,
  type Node,
  type Edge,
  type FitViewOptions,
  Position,
} from 'reactflow';
import 'reactflow/dist/style.css';
import dagre from 'dagre';
import { ShieldCheck } from "lucide-react";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { isRecord } from '@/lib/typeGuards';
import type { JobDAGResponse, Atom, JobTask, TaskRun } from '@/lib/api';
import { TaskNode } from './components/TaskNode';
import { BranchNode } from './components/BranchNode';
import { DataFlowEdge } from './components/DataFlowEdge';

const nodeWidth = 260;
const nodeHeight = 112;

const nodeTypes = {
  task: TaskNode,
  branch: BranchNode,
};

const edgeTypes = {
  dataflow: DataFlowEdge,
};

const getLayoutedElements = (nodes: Node[], edges: Edge[], direction = 'LR') => {
  const dagreGraph = new dagre.graphlib.Graph();
  dagreGraph.setDefaultEdgeLabel(() => ({}));

  dagreGraph.setGraph({
    rankdir: direction,
    nodesep: 120,
    ranksep: 170,
    marginx: 50,
    marginy: 50,
    ranker: 'network-simplex',
  });

  nodes.forEach((node) => {
    dagreGraph.setNode(node.id, { width: nodeWidth, height: nodeHeight });
  });

  edges.forEach((edge) => {
    dagreGraph.setEdge(edge.source, edge.target);
  });

  dagre.layout(dagreGraph);

  const layoutedNodes = nodes.map((node) => {
    const nodeWithPosition = dagreGraph.node(node.id);
    return {
      ...node,
      targetPosition: direction === 'LR' ? Position.Left : Position.Top,
      sourcePosition: direction === 'LR' ? Position.Right : Position.Bottom,
      position: {
        x: nodeWithPosition.x - nodeWidth / 2,
        y: nodeWithPosition.y - nodeHeight / 2,
      },
    };
  });

  return { nodes: layoutedNodes, edges };
};

interface TaskRunMetadata {
  status: string;
  started_at?: string;
  completed_at?: string;
  error?: string;
  rate_limit_retry_after?: string;
  output?: Record<string, string>;
}

interface JobDAGProps {
  /**
   * The DAG canvas fills its parent. Embed it only in a container with a
   * resolved height (for example `useDagHeight` plus a pixel fallback).
   */
  dag: JobDAGResponse;
  runStartedAt?: string;
  atoms: Record<string, Atom>;
  taskDefinitions?: Record<string, JobTask>;
  taskStatus?: Record<string, string>;
  taskMetadata?: Record<string, TaskRunMetadata>;
  taskRunData?: Record<string, TaskRun>;
  onNodeClick?: (taskId: string) => void;
  selectedTaskId?: string | null;
}

interface EdgeDetailsState {
  sourceId: string;
  sourceName: string;
  targetId: string;
  targetName: string;
  outputCount: number;
  contractDefined: boolean;
  output?: Record<string, string>;
  outputSchema?: Record<string, unknown>;
  contractSchema?: Record<string, unknown>;
}

interface NodeEdgeDegree {
  incoming: number;
  outgoing: number;
  total: number;
}

/**
 * Own both initial framing and resize fitting after the viewport and nodes
 * are measured. A separate React Flow initial fit can race this mobile view
 * and shrink the whole graph back into an unreadable miniature.
 */
function FitViewOnResize({ fitViewOptions }: { fitViewOptions: FitViewOptions }) {
  const { fitView, getNodes, viewportInitialized } = useReactFlow();
  const nodesInitialized = useNodesInitialized();
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);

  useEffect(() => {
    if (!viewportInitialized || !nodesInitialized || width === 0 || height === 0) return;

    const frame = window.requestAnimationFrame(() => {
      const first = getNodes().slice().sort((a, b) => a.position.x - b.position.x || a.position.y - b.position.y)[0];
      void fitView(width < 640 && first ? { ...fitViewOptions, nodes: [{ id: first.id }], minZoom: 0.75, maxZoom: 0.9 } : fitViewOptions);
    });

    return () => window.cancelAnimationFrame(frame);
  }, [fitView, getNodes, fitViewOptions, height, nodesInitialized, viewportInitialized, width]);

  return null;
}

export function JobDAG({ dag, runStartedAt, atoms, taskDefinitions, taskStatus, taskMetadata, taskRunData, onNodeClick, selectedTaskId }: JobDAGProps) {
    const [selectedEdge, setSelectedEdge] = useState<EdgeDetailsState | null>(null);
    const resolvedTaskStatus = useMemo(() => {
        const statusByTask: Record<string, string> = {};

        Object.entries(taskStatus ?? {}).forEach(([taskId, status]) => {
            statusByTask[taskId] = normalizeTaskStatus(status);
        });

        Object.entries(taskMetadata ?? {}).forEach(([taskId, metadata]) => {
            if (metadata?.status) {
                statusByTask[taskId] = normalizeTaskStatus(metadata.status);
            }
        });

        return statusByTask;
    }, [taskMetadata, taskStatus]);

    const edgeDegreeByNode = useMemo(() => {
        const degreeByNode = new Map<string, NodeEdgeDegree>();

        (dag.nodes ?? []).forEach((node) => {
            degreeByNode.set(node.id, { incoming: 0, outgoing: 0, total: 0 });
        });

        (dag.edges ?? []).forEach((edge) => {
            incrementEdgeDegree(degreeByNode, edge.from, 'outgoing');
            incrementEdgeDegree(degreeByNode, edge.to, 'incoming');
        });

        return degreeByNode;
    }, [dag.edges, dag.nodes]);

    const initialNodes: Node[] = useMemo(() => {
        if (!dag.nodes) return [];
        return dag.nodes.map(n => {
            const atom = atoms[n.atom_id];
            const taskDefinition = taskDefinitions?.[n.id];
            const meta = taskMetadata?.[n.id];
            const status = resolvedTaskStatus[n.id] || 'pending';

            const nodeType = n.type === 'branch' ? 'branch' : 'task';

            return {
                id: n.id,
                type: nodeType,
                data: {
                  label: taskDefinition?.name || "task",
                  runStartedAt,
                  atom: atom,
                  status: status,
                  isSelected: selectedTaskId === n.id,
                  startedAt: meta?.started_at,
                  completedAt: meta?.completed_at,
                  error: meta?.error,
                  rateLimitRetryAfter: meta?.rate_limit_retry_after,
                  partitionCount: taskRunData?.[n.id]?.partition_count,
                  partitionValue: taskRunData?.[n.id]?.partition_value,
                  partitionStatusCounts: taskRunData?.[n.id]?.partition_status_counts,
                  taskType: n.type,
                  edgeDegree: edgeDegreeByNode.get(n.id) ?? { incoming: 0, outgoing: 0, total: 0 },
                },
                position: { x: 0, y: 0 }
            }
        });
    }, [dag, runStartedAt, atoms, taskDefinitions, resolvedTaskStatus, taskMetadata, taskRunData, selectedTaskId, edgeDegreeByNode]);

    const initialEdges: Edge[] = useMemo(() => {
        if (!dag.edges) return [];
        const dagNodesById = new Map(dag.nodes.map((node) => [node.id, node]));

        return dag.edges.map((e) => {
            const sourceStatus = resolvedTaskStatus[e.from] || 'pending';
            const targetStatus = resolvedTaskStatus[e.to] || 'pending';
            const sourceRun = taskRunData?.[e.from];
            const sourceTask = taskDefinitions?.[e.from];
            const targetTask = taskDefinitions?.[e.to];
            const sourceNode = dagNodesById.get(e.from);
            const outputCount = sourceRun?.output ? Object.keys(sourceRun.output).length : 0;
                        const isBranchSkipped = targetStatus === 'skipped' && sourceStatus === 'succeeded';
            const stroke = isBranchSkipped ? 'hsl(var(--text-4))' : edgeColor(sourceStatus);
            const sourceName = sourceTask?.name || e.from;
            const targetName = targetTask?.name || e.to;
            const contractSchema = sourceTask?.name && targetTask?.input_schema
              ? targetTask.input_schema[sourceTask.name]
              : undefined;
            const outputSchema = sourceTask?.output_schema || sourceNode?.output_schema;

            return {
                id: `e${e.from}-${e.to}`,
                source: e.from,
                target: e.to,
                type: 'dataflow',
                animated: false,
                className: sourceStatus === 'running' ? 'cs-edge-running' : undefined,
                data: {
                  outputCount,
                  running: sourceStatus === "running",
                  contractDefined: !!e.contract_defined,
                  onOpenDetails: () => setSelectedEdge({
                    sourceId: e.from,
                    sourceName,
                    targetId: e.to,
                    targetName,
                    outputCount,
                    contractDefined: !!e.contract_defined,
                    output: sourceRun?.output,
                    outputSchema,
                    contractSchema,
                  }),
                },
                markerEnd: {
                  type: MarkerType.ArrowClosed,
                  width: 20,
                  height: 20,
                  color: stroke,
                },
                style: {
                  strokeWidth: sourceStatus === "running" ? 2 : 1.5,
                  stroke,
                  strokeDasharray: isBranchSkipped ? '2 5' : sourceStatus === 'running' ? '7 7' : undefined,
                  opacity: 1,
                }
            };
        });
    }, [dag, resolvedTaskStatus, taskDefinitions, taskRunData]);

    const { nodes: layoutedNodes, edges: layoutedEdges } = useMemo(
        () => getLayoutedElements(initialNodes, initialEdges),
        [initialNodes, initialEdges]
    );

    const isSingleNodeDAG = layoutedNodes.length === 1 && layoutedEdges.length === 0;
    const dagMaxZoom = isSingleNodeDAG ? 2.2 : 1.5;
    // A dense DAG can need to fit below the old 0.1 floor when the run page
    // has less vertical space than its default canvas height.
    const dagMinZoom = isSingleNodeDAG ? 0.1 : 0.05;
    const fitViewOptions = useMemo(
      () => ({
        padding: isSingleNodeDAG ? 0.06 : 0.2,
        minZoom: dagMinZoom,
        maxZoom: dagMaxZoom,
      }),
      [isSingleNodeDAG, dagMinZoom, dagMaxZoom]
    );

    const handleNodeClick = useCallback((event: React.MouseEvent, node: Node) => {
      (event.target as HTMLElement).closest<HTMLElement>(".react-flow__node")?.focus();
      onNodeClick?.(node.id);
    }, [onNodeClick]);

  return (
    <>
      <div className="relative h-full w-full overflow-hidden rounded-lg bg-dag-bg" onKeyDownCapture={(event) => {
        const target = event.target as HTMLElement;
        if ((event.key === "Enter" || event.key === " ") && target.matches(".react-flow__node")) {
          const id = target.dataset.id;
          if (id && onNodeClick) { event.preventDefault(); event.stopPropagation(); onNodeClick(id); }
        }
      }}>
        <ReactFlow
          nodes={layoutedNodes}
          edges={layoutedEdges}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          onNodeClick={handleNodeClick}
          fitViewOptions={fitViewOptions}
          minZoom={dagMinZoom}
          maxZoom={dagMaxZoom}
        >
          <FitViewOnResize fitViewOptions={fitViewOptions} />
          <Background gap={24} />
          <Controls fitViewOptions={fitViewOptions} />
          <div className="pointer-events-none absolute right-2 top-2 z-10 rounded bg-card/95 px-2 py-1 text-xs text-text-3 sm:hidden">Drag to explore · Fit view shows all</div>
        </ReactFlow>
      </div>

      <Dialog open={selectedEdge !== null} onOpenChange={(open) => !open && setSelectedEdge(null)}>
        <DialogContent className="max-w-2xl p-4 sm:rounded-md sm:p-6">
            <DialogHeader>
            <DialogTitle>
              {selectedEdge ? `${selectedEdge.sourceName} → ${selectedEdge.targetName}` : 'Edge details'}
            </DialogTitle>
            <DialogDescription>
              Inspect observed run outputs, the producer's published schema, and any downstream requirements declared for this connection.
            </DialogDescription>
          </DialogHeader>
          {selectedEdge ? <EdgeDetails edge={selectedEdge} /> : null}
        </DialogContent>
      </Dialog>
    </>
  );
}

function incrementEdgeDegree(degreeByNode: Map<string, NodeEdgeDegree>, nodeId: string, direction: 'incoming' | 'outgoing') {
    const degree = degreeByNode.get(nodeId) ?? { incoming: 0, outgoing: 0, total: 0 };
    degree[direction] += 1;
    degree.total += 1;
    degreeByNode.set(nodeId, degree);
}

function normalizeTaskStatus(status?: string) {
    switch (status) {
        case 'completed':
            return 'succeeded';
        default:
            return status || 'pending';
    }
}

function edgeColor(status: string) {
    switch (status) {
        case 'running':
            return 'hsl(var(--running))';
        case 'succeeded':
            return 'hsl(var(--success))';
        case 'cached':
            return 'hsl(var(--cached))';
        case 'failed':
            return 'hsl(var(--danger))';
        case 'skipped':
            return 'hsl(var(--warning))';
        default:
            return 'hsl(var(--text-3))';
    }
}

function EdgeDetails({ edge }: { edge: EdgeDetailsState }) {
    return (
        <div className="space-y-5">
            <div className="flex flex-wrap items-center gap-2">
                <span className="rounded-full border border-success/35 bg-success/10 px-2.5 py-1 text-xs font-bold text-success">
                    {edge.outputCount} {edge.outputCount === 1 ? 'output' : 'outputs'}
                </span>
                <span
                    className={`inline-flex items-center rounded-full border px-2 py-1 ${
                        edge.contractDefined
                            ? 'border-running/35 bg-running/10'
                            : 'border-text-3/30 bg-text-3/10'
                    }`}
                    title={edge.contractDefined ? 'Consumer requirements declared' : 'No consumer requirements declared'}
                >
                    <ShieldCheck className={`h-3 w-3 ${edge.contractDefined ? 'text-running' : 'text-text-3'}`} />
                </span>
            </div>

            <SchemaSection
                title="Observed outputs for this run"
                description="Values emitted by the upstream task on the selected run overlay."
            >
                {edge.output && Object.keys(edge.output).length > 0 ? (
                    <div className="rounded-md border bg-muted/40 p-3">
                        {Object.entries(edge.output).map(([key, value]) => (
                            <div key={key} className="flex gap-2 text-xs">
                                <span className="font-bold text-muted-foreground">{key}:</span>
                                <span className="break-all text-foreground">{value}</span>
                            </div>
                        ))}
                    </div>
                ) : (
                    <EmptyState message="No run-time outputs are available for this edge on the current overlay." />
                )}
            </SchemaSection>

            <div className="grid gap-4 md:grid-cols-2">
                <SchemaSection
                    title="Published output schema"
                    description="The schema the upstream task declares for the data it emits."
                >
                    <SchemaPreview schema={edge.outputSchema} emptyMessage="This producer does not declare an output schema." />
                </SchemaSection>
                <SchemaSection
                    title="Consumer requirements"
                    description="The fields and types the downstream task explicitly requires from this producer."
                >
                    <SchemaPreview schema={edge.contractSchema} emptyMessage="This consumer uses the producer output without declaring specific required fields." />
                </SchemaSection>
            </div>
        </div>
    );
}

function SchemaSection({
    title,
    description,
    children,
}: {
    title: string;
    description: string;
    children: ReactNode;
}) {
    return (
        <div className="space-y-2">
            <div>
                <div className="text-sm font-bold text-foreground">{title}</div>
                <div className="text-xs text-muted-foreground">{description}</div>
            </div>
            {children}
        </div>
    );
}

function SchemaPreview({
    schema,
    emptyMessage,
}: {
    schema?: Record<string, unknown>;
    emptyMessage: string;
}) {
    if (!schema) {
        return <EmptyState message={emptyMessage} />;
    }

    const properties = isRecord(schema.properties) ? schema.properties : null;
    const required = Array.isArray(schema.required)
        ? schema.required.filter((item): item is string => typeof item === 'string')
        : [];

    if (properties && Object.keys(properties).length > 0) {
        return (
            <div className="rounded-md border bg-muted/40 p-3">
                {Object.entries(properties).map(([key, value]) => {
                    const prop = isRecord(value) ? value : undefined;
                    const type = typeof prop?.type === 'string' ? prop.type : 'any';
                    const isRequired = required.includes(key);

                    return (
                        <div key={key} className="flex items-center gap-2 text-xs">
                            <span className="font-bold text-foreground">{key}</span>
                            <span className="text-muted-foreground">{type}</span>
                            {isRequired ? (
                                <span className="rounded border border-running/35 bg-running/10 px-1.5 py-0.5 text-[11px] font-bold text-running">
                                    required
                                </span>
                            ) : null}
                        </div>
                    );
                })}
            </div>
        );
    }

    return (
        <pre className="max-h-64 overflow-auto rounded-md border bg-muted/40 p-3 text-[11px] leading-relaxed text-foreground">
            {JSON.stringify(schema, null, 2)}
        </pre>
    );
}

function EmptyState({ message }: { message: string }) {
    return (
        <div className="rounded-md border border-dashed bg-muted/20 p-3 text-xs text-muted-foreground">
            {message}
        </div>
    );
}
