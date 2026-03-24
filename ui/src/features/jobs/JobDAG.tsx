import { useMemo, useCallback } from 'react';
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
import type { JobDAGResponse, Atom, TaskRun } from '@/lib/api';
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
  taskStatus?: Record<string, string>;
  taskMetadata?: Record<string, TaskRunMetadata>;
  taskRunData?: Record<string, TaskRun>;
  onNodeClick?: (taskId: string) => void;
  selectedTaskId?: string | null;
}

export function JobDAG({ dag, atoms, taskStatus, taskMetadata, taskRunData, onNodeClick, selectedTaskId }: JobDAGProps) {
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

    // Build a set of task IDs that receive outputs from predecessors.
    const taskReceivesOutputs = useMemo(() => {
        const receivers = new Set<string>();
        if (!dag.edges) return receivers;
        for (const e of dag.edges) {
            const sourceRun = taskRunData?.[e.from];
            if (sourceRun?.output && Object.keys(sourceRun.output).length > 0) {
                receivers.add(e.to);
            }
        }
        return receivers;
    }, [dag.edges, taskRunData]);

    const initialNodes: Node[] = useMemo(() => {
        if (!dag.nodes) return [];
        return dag.nodes.map(n => {
            const atom = atoms[n.atom_id];
            const meta = taskMetadata?.[n.id];
            const status = resolvedTaskStatus[n.id] || 'pending';
            const runTask = taskRunData?.[n.id];
            const outputCount = runTask?.output ? Object.keys(runTask.output).length : 0;

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
                  outputCount,
                  receivesOutputs: taskReceivesOutputs.has(n.id),
                  taskType: n.type,
                  hasOutputSchema: !!n.output_schema,
                },
                position: { x: 0, y: 0 }
            }
        });
    }, [dag, atoms, resolvedTaskStatus, taskMetadata, taskRunData, selectedTaskId, taskReceivesOutputs]);

    const initialEdges: Edge[] = useMemo(() => {
        if (!dag.edges) return [];
        return dag.edges.map((e) => {
            const sourceStatus = resolvedTaskStatus[e.from] || 'pending';
            const targetStatus = resolvedTaskStatus[e.to] || 'pending';
            const sourceRun = taskRunData?.[e.from];
            const outputCount = sourceRun?.output ? Object.keys(sourceRun.output).length : 0;
            const hasOutputs = outputCount > 0;
            const isBranchSkipped = targetStatus === 'skipped' && sourceStatus === 'succeeded';
            const stroke = isBranchSkipped ? '#64748b' : hasOutputs && sourceStatus === 'succeeded' ? '#10b981' : edgeColor(sourceStatus);

            return {
                id: `e${e.from}-${e.to}`,
                source: e.from,
                target: e.to,
                type: 'dataflow',
                animated: sourceStatus === 'running',
                data: { outputCount, contractDefined: !!e.contract_defined },
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
    }, [dag, resolvedTaskStatus, taskRunData]);

    const { nodes: layoutedNodes, edges: layoutedEdges } = useMemo(
        () => getLayoutedElements(initialNodes, initialEdges),
        [initialNodes, initialEdges]
    );

    const handleNodeClick = useCallback((_event: React.MouseEvent, node: Node) => {
      onNodeClick?.(node.id);
    }, [onNodeClick]);

  return (
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
            return '#00b4d8';
        case 'succeeded':
            return '#10b981';
        case 'failed':
            return '#f97316';
        case 'skipped':
            return '#f59e0b';
        default:
            return '#64748b';
    }
}
