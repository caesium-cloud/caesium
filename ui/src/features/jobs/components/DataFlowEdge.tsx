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
              className="flex items-center gap-1.5 rounded-full border border-slate-400/35 bg-slate-950/80 px-2.5 py-1 shadow-sm backdrop-blur-sm transition-colors hover:border-slate-300/45 hover:bg-slate-900/90"
              onClick={onOpenDetails}
              title="View edge data details"
            >
              {outputCount > 0 ? (
                <>
                  <ArrowRight className="h-2.5 w-2.5 text-emerald-400" />
                  <span className="text-[9px] font-bold tabular-nums text-emerald-300">
                    {outputCount} {outputCount === 1 ? 'output' : 'outputs'}
                  </span>
                </>
              ) : null}
              <div
                className={`rounded-full border px-1.5 py-0.5 ${
                  contractDefined
                    ? 'border-blue-500/35 bg-blue-500/10'
                    : 'border-slate-500/30 bg-slate-500/10'
                }`}
                title={contractDefined ? 'Data contract defined' : 'No data contract defined'}
              >
                <ShieldCheck className={`h-2.5 w-2.5 ${contractDefined ? 'text-blue-400' : 'text-slate-500'}`} />
              </div>
            </button>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

DataFlowEdge.displayName = 'DataFlowEdge';
