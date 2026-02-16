import { useMemo } from 'react';
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

const nodeWidth = 200;
const nodeHeight = 50;

const getLayoutedElements = (nodes: Node[], edges: Edge[], direction = 'LR') => {
  const dagreGraph = new dagre.graphlib.Graph();
  dagreGraph.setDefaultEdgeLabel(() => ({}));

  dagreGraph.setGraph({ rankdir: direction });

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

interface JobDAGProps {
  dag: JobDAGResponse;
  atoms: Record<string, Atom>;
  taskStatus?: Record<string, string>;
}

export function JobDAG({ dag, atoms, taskStatus }: JobDAGProps) {
    const initialNodes: Node[] = useMemo(() => {
        if (!dag.nodes) return [];
        return dag.nodes.map(n => {
            const atom = atoms[n.atom_id];
            const status = taskStatus?.[n.id];
            
            let className = "border-2 rounded-md bg-white min-w-[150px]";
            if (status === 'running') className += " border-blue-500 animate-pulse";
            else if (status === 'completed') className += " border-green-500";
            else if (status === 'failed') className += " border-red-500";
            else if (status === 'cancelled') className += " border-yellow-500";
            else className += " border-slate-200";

            return {
                id: n.id,
                type: 'input', 
                data: { label: atom ? `${atom.image}` : n.atom_id },
                className,
                position: { x: 0, y: 0 } 
            }
        });
    }, [dag, atoms, taskStatus]);

    const initialEdges: Edge[] = useMemo(() => {
        if (!dag.edges) return [];
        return dag.edges.map((e, i) => ({
            id: `e${i}`,
            source: e.from,
            target: e.to,
            type: 'smoothstep',
            markerEnd: { type: MarkerType.ArrowClosed },
        }));
    }, [dag]);

    const { nodes: layoutedNodes, edges: layoutedEdges } = useMemo(
        () => getLayoutedElements(initialNodes, initialEdges),
        [initialNodes, initialEdges]
    );

  return (
    <div style={{ height: 600 }} className="border rounded-md bg-white">
      <ReactFlow
        nodes={layoutedNodes}
        edges={layoutedEdges}
        fitView
      >
        <Background />
        <Controls />
      </ReactFlow>
    </div>
  );
}
