import { useMemo, useCallback, useState, type ReactNode } from 'react';
import ReactFlow, {
  Controls,
  Background,
  MarkerType,
  type Node,
  type Edge,
  Position,
} from 'reactflow';
import 'reactflow/dist/style.css';
import dagre from 'dagre';
import { ShieldCheck } from 'lucide-react';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import type { JobDAGResponse, Atom, JobTask, TaskRun } from '@/lib/api';
import { TaskNode } from './components/TaskNode';
import { BranchNode } from './components/BranchNode';
import { DataFlowEdge } from './components/DataFlowEdge';

const nodeWidth = 300;
const nodeHeight = 148;

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
    nodesep: 150, 
    ranksep: 200,
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
  output?: Record<string, string>;
}

interface JobDAGProps {
  dag: JobDAGResponse;
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

export function JobDAG({ dag, atoms, taskDefinitions, taskStatus, taskMetadata, taskRunData, onNodeClick, selectedTaskId }: JobDAGProps) {
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

    const initialNodes: Node[] = useMemo(() => {
        if (!dag.nodes) return [];
        return dag.nodes.map(n => {
            const atom = atoms[n.atom_id];
            const meta = taskMetadata?.[n.id];
            const status = resolvedTaskStatus[n.id] || 'pending';

            const nodeType = n.type === 'branch' ? 'branch' : 'task';

            return {
                id: n.id,
                type: nodeType,
                data: {
                  label: n.id,
                  atom: atom,
                  status: status,
                  isSelected: selectedTaskId === n.id,
                  startedAt: meta?.started_at,
                  completedAt: meta?.completed_at,
                  error: meta?.error,
                  taskType: n.type,
                },
                position: { x: 0, y: 0 }
            }
        });
    }, [dag, atoms, resolvedTaskStatus, taskMetadata, selectedTaskId]);

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
            const hasOutputs = outputCount > 0;
            const isBranchSkipped = targetStatus === 'skipped' && sourceStatus === 'succeeded';
            const stroke = isBranchSkipped ? 'hsl(var(--text-3))' : edgeColor(sourceStatus);
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
                animated: sourceStatus === 'running',
                data: {
                  outputCount,
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
                  strokeWidth: hasOutputs ? 3 : 2,
                  stroke,
                  strokeDasharray: isBranchSkipped ? '6 3' : undefined,
                  opacity: isBranchSkipped ? 0.5 : 1,
                }
            };
        });
    }, [dag, resolvedTaskStatus, taskDefinitions, taskRunData]);

    const { nodes: layoutedNodes, edges: layoutedEdges } = useMemo(
        () => getLayoutedElements(initialNodes, initialEdges),
        [initialNodes, initialEdges]
    );

    const handleNodeClick = useCallback((_event: React.MouseEvent, node: Node) => {
      onNodeClick?.(node.id);
    }, [onNodeClick]);

  return (
    <>
      <div className="relative h-full min-h-[500px] w-full overflow-hidden rounded-lg bg-dag-bg">
        <ReactFlow
          nodes={layoutedNodes}
          edges={layoutedEdges}
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

      <Dialog open={selectedEdge !== null} onOpenChange={(open) => !open && setSelectedEdge(null)}>
        <DialogContent className="max-w-2xl">
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
                <span className="rounded-full border border-success/35 bg-success/10 px-2.5 py-1 text-xs font-semibold text-success">
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
                            <div key={key} className="flex gap-2 font-mono text-xs">
                                <span className="font-semibold text-muted-foreground">{key}:</span>
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
                <div className="text-sm font-semibold text-foreground">{title}</div>
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
                            <span className="font-mono font-semibold text-foreground">{key}</span>
                            <span className="font-mono text-muted-foreground">{type}</span>
                            {isRequired ? (
                                <span className="rounded border border-running/35 bg-running/10 px-1.5 py-0.5 text-[10px] font-semibold text-running">
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

function isRecord(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}
