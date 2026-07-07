// Shared edge-degree helpers for DAG node components (TaskNode, BranchNode).
// JobDAG threads a per-node `edgeDegree` ({ incoming, outgoing, total }) into
// node data; nodes hide their input/output handles when they have no incident
// edges on that side.

export function getHandleVisibility(edgeDegree: unknown) {
  if (!isEdgeDegree(edgeDegree)) {
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

function isEdgeDegree(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function readEdgeCount(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}
