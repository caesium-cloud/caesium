import dagre from "dagre";
import type { CSSProperties } from "react";
import { MarkerType, Position, type Edge, type Node } from "reactflow";
import type {
  ContractDatasetRef,
  ContractEdgeClass,
  ContractGraphEdge,
  ContractGraphNode,
  ContractGraphResponse,
  ContractVerdict,
} from "@/lib/api";

export const contractNodeWidth = 300;
export const contractNodeHeight = 138;

export interface ContractFlowNodeData {
  id: string;
  kind: string;
  alias?: string;
  dataset?: ContractDatasetRef;
  label: string;
  detail: string;
  jobId?: string;
}

export interface ContractFlowEdgeData {
  edgeId: string;
  edgeClass: ContractEdgeClass;
  verdict?: ContractVerdict;
  findingCount: number;
  datasetLabel?: string;
  lastSeen?: string;
  sourceLabel: string;
  targetLabel: string;
  testId: string;
}

export function buildContractFlow(
  graph: ContractGraphResponse,
  jobIdsByAlias: Map<string, string>,
): { nodes: Node<ContractFlowNodeData>[]; edges: Edge<ContractFlowEdgeData>[] } {
  const nodeLabelById = new Map<string, string>();
  const initialNodes: Node<ContractFlowNodeData>[] = graph.nodes.map((node) => {
    const label = contractNodeLabel(node);
    nodeLabelById.set(node.id, label);
    return {
      id: node.id,
      type: "contract",
      data: {
        id: node.id,
        kind: node.kind,
        alias: node.alias,
        dataset: node.dataset,
        label,
        detail: contractNodeDetail(node),
        jobId: node.kind === "job" ? jobIdsByAlias.get(label) : undefined,
      },
      position: { x: 0, y: 0 },
    };
  });

  const initialEdges: Edge<ContractFlowEdgeData>[] = graph.edges.map((edge) => {
    const style = edgeVisualStyle(edge);
    return {
      id: edge.id,
      source: edge.from,
      target: edge.to,
      type: "contract",
      data: {
        edgeId: edge.id,
        edgeClass: edge.class,
        verdict: edge.verdict,
        findingCount: edge.findings?.length ?? 0,
        datasetLabel: edge.dataset ? formatDatasetRef(edge.dataset) : undefined,
        lastSeen: edge.lastSeen,
        sourceLabel: nodeLabelById.get(edge.from) ?? edge.from,
        targetLabel: nodeLabelById.get(edge.to) ?? edge.to,
        testId: contractEdgeTestId(edge),
      },
      markerEnd: {
        type: MarkerType.ArrowClosed,
        width: 20,
        height: 20,
        color: style.stroke,
      },
      style,
    };
  });

  return getLayoutedElements(initialNodes, initialEdges);
}

export function contractEdgeTestId(edge: ContractGraphEdge): string {
  return `contract-edge:${edge.class}:${edge.id}`;
}

export function formatDatasetRef(dataset: ContractDatasetRef): string {
  const namespace = dataset.namespace.trim();
  const name = dataset.name.trim();
  return namespace ? `${namespace}/${name}` : `/${name}`;
}

export function cleanDatasetFilter(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}

export function contractNodeTestId(data: ContractFlowNodeData): string {
  return `contract-node:${data.id}`;
}

function contractNodeLabel(node: ContractGraphNode): string {
  if (node.kind === "dataset" && node.dataset) {
    return formatDatasetRef(node.dataset);
  }
  if (node.alias) {
    return node.alias;
  }
  return node.id.startsWith("job:") ? node.id.slice("job:".length) : node.id;
}

function contractNodeDetail(node: ContractGraphNode): string {
  if (node.kind === "dataset") {
    return "Dataset";
  }
  return "Job";
}

function edgeVisualStyle(edge: ContractGraphEdge): CSSProperties {
  const stroke = edgeStrokeColor(edge);
  return {
    stroke,
    strokeWidth: edge.class === "evidence" ? 2 : 2.5,
    strokeDasharray: edgeDashArray(edge.class),
    strokeLinecap: edge.class === "evidence" ? "round" : undefined,
    opacity: edge.class === "evidence" ? 0.58 : 1,
  };
}

function edgeDashArray(edgeClass: ContractEdgeClass): string | undefined {
  switch (edgeClass) {
    case "inferred":
      return "8 5";
    case "evidence":
      return "1 8";
    default:
      return undefined;
  }
}

function edgeStrokeColor(edge: ContractGraphEdge): string {
  if (edge.class === "evidence") {
    return "hsl(var(--text-4))";
  }
  switch (edge.verdict) {
    case "breaking":
      return "hsl(var(--danger))";
    case "unknown":
      return "hsl(var(--warning))";
    case "compatible":
      return "hsl(var(--success))";
    default:
      return "hsl(var(--text-3))";
  }
}

function getLayoutedElements(
  nodes: Node<ContractFlowNodeData>[],
  edges: Edge<ContractFlowEdgeData>[],
  direction = "LR",
) {
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
    dagreGraph.setNode(node.id, { width: contractNodeWidth, height: contractNodeHeight });
  });
  edges.forEach((edge) => {
    dagreGraph.setEdge(edge.source, edge.target);
  });

  dagre.layout(dagreGraph);

  return {
    nodes: nodes.map((node) => {
      const layoutNode = dagreGraph.node(node.id) ?? { x: 0, y: 0 };
      return {
        ...node,
        targetPosition: direction === "LR" ? Position.Left : Position.Top,
        sourcePosition: direction === "LR" ? Position.Right : Position.Bottom,
        position: {
          x: layoutNode.x - contractNodeWidth / 2,
          y: layoutNode.y - contractNodeHeight / 2,
        },
      };
    }),
    edges,
  };
}
