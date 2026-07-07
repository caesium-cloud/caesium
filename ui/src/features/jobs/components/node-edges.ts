// Shared edge-degree helpers for DAG node components (TaskNode, BranchNode).
// JobDAG threads a per-node `edgeDegree` ({ incoming, outgoing, total }) into
// node data; nodes hide their input/output handles when they have no incident
// edges on that side.

import { isRecord } from '@/lib/typeGuards';

export function getHandleVisibility(edgeDegree: unknown) {
  if (!isRecord(edgeDegree)) {
    return {
      showTargetHandle: true,
      showSourceHandle: true,
    };
  }

  return {
    showTargetHandle: readEdgeCount(edgeDegree.incoming) > 0,
    showSourceHandle: readEdgeCount(edgeDegree.outgoing) > 0,
  };
}

function readEdgeCount(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}
