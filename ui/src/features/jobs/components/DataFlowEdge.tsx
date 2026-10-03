import { memo } from 'react';
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type EdgeProps } from 'reactflow';
import { ShieldCheck } from "lucide-react";

export const DataFlowEdge = memo(({
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

  const outputCount = data?.outputCount ?? 0;
  const contractDefined = data?.contractDefined ?? false;
  const showLabel = outputCount > 0 || contractDefined;
  const onOpenDetails = data?.onOpenDetails as (() => void) | undefined;

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} markerEnd={markerEnd} />
      {showLabel && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan pointer-events-auto"
            style={{
              position: 'absolute',
              transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
            }}
          >
            <button
              type="button"
              className="flex items-center gap-1.5 h-5 rounded-[10px] border border-border bg-obsidian px-2 transition-colors hover:border-border hover:bg-card"
              onClick={onOpenDetails}
              title="View edge data details"
            >
              <ShieldCheck className={`h-3 w-3 ${contractDefined ? 'text-running' : 'text-text-3'}`} aria-label={contractDefined ? 'Data contract defined' : 'No data contract defined'} />
              <span className="text-[11px] text-text-2">{outputCount} {outputCount === 1 ? 'output' : 'outputs'}</span>
            </button>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

DataFlowEdge.displayName = 'DataFlowEdge';
