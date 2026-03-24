import { memo } from 'react';
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type EdgeProps } from 'reactflow';
import { ArrowRight, ShieldCheck } from 'lucide-react';

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
            <div className="flex items-center gap-1">
              {outputCount > 0 && (
                <div className="flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/15 px-2 py-0.5 shadow-sm shadow-emerald-500/10 backdrop-blur-sm">
                  <ArrowRight className="h-2.5 w-2.5 text-emerald-400" />
                  <span className="text-[9px] font-bold tabular-nums text-emerald-300">
                    {outputCount} {outputCount === 1 ? 'output' : 'outputs'}
                  </span>
                </div>
              )}
              {contractDefined && (
                <div className="flex items-center gap-1 rounded-full border border-blue-500/40 bg-blue-500/15 px-2 py-0.5 shadow-sm shadow-blue-500/10 backdrop-blur-sm">
                  <ShieldCheck className="h-2.5 w-2.5 text-blue-400" />
                  <span className="text-[9px] font-bold text-blue-300">contract</span>
                </div>
              )}
            </div>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

DataFlowEdge.displayName = 'DataFlowEdge';
