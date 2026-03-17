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
import type { JobDAGResponse, Atom } from '@/lib/api';
import { TaskNode } from './components/TaskNode';

const nodeWidth = 300;
const nodeHeight = 148;

const nodeTypes = {
  task: TaskNode,
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
}

interface JobDAGProps {
  dag: JobDAGResponse;
  atoms: Record<string, Atom>;
  taskStatus?: Record<string, string>;
  taskMetadata?: Record<string, TaskRunMetadata>;
  onNodeClick?: (taskId: string) => void;
  selectedTaskId?: string | null;
}

export function JobDAG({ dag, atoms, taskStatus, taskMetadata, onNodeClick, selectedTaskId }: JobDAGProps) {
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
            
            return {
                id: n.id,
                type: 'task',
                data: { 
                  label: n.id,
                  atom: atom,
                  status: status,
                  isSelected: selectedTaskId === n.id,
                  startedAt: meta?.started_at,
                  completedAt: meta?.completed_at,
                  error: meta?.error,
                },
                position: { x: 0, y: 0 } 
            }
        });
    }, [dag, atoms, resolvedTaskStatus, taskMetadata, selectedTaskId]);

    const initialEdges: Edge[] = useMemo(() => {
        if (!dag.edges) return [];
        return dag.edges.map((e) => {
            const sourceStatus = resolvedTaskStatus[e.from] || 'pending';
            const stroke = edgeColor(sourceStatus);

            return {
                id: `e${e.from}-${e.to}`,
                source: e.from,
                target: e.to,
                type: 'simplebezier',
                animated: sourceStatus === 'running',
                markerEnd: { 
                  type: MarkerType.ArrowClosed,
                  width: 20,
                  height: 20,
                  color: stroke,
                },
                style: {
                  strokeWidth: 2,
                  stroke,
                }
            };
        });
    }, [dag, resolvedTaskStatus]);

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
