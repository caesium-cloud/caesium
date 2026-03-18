import { memo } from 'react';
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type EdgeProps } from 'reactflow';
import { ArrowRight } from 'lucide-react';

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

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} markerEnd={markerEnd} />
      {outputCount > 0 && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan pointer-events-auto"
            style={{
              position: 'absolute',
              transform: `translate(-50%, -50%) translate(${labelX}px,${labelY}px)`,
            }}
          >
            <div className="flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/15 px-2 py-0.5 shadow-sm shadow-emerald-500/10 backdrop-blur-sm">
              <ArrowRight className="h-2.5 w-2.5 text-emerald-400" />
              <span className="text-[9px] font-bold tabular-nums text-emerald-300">
                {outputCount} {outputCount === 1 ? 'output' : 'outputs'}
              </span>
            </div>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

DataFlowEdge.displayName = 'DataFlowEdge';
