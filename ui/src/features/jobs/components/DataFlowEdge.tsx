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
              className="flex items-center gap-1.5 rounded-full border border-border/50 bg-card/90 px-2.5 py-1 shadow-sm backdrop-blur-sm transition-colors hover:border-border hover:bg-card"
              onClick={onOpenDetails}
              title="View edge data details"
            >
              {outputCount > 0 ? (
                <>
                  <ArrowRight className="h-2.5 w-2.5 text-success" />
                  <span className="text-[9px] font-bold tabular-nums text-success">
                    {outputCount} {outputCount === 1 ? 'output' : 'outputs'}
                  </span>
                </>
              ) : null}
              <div
                className={`rounded-full border px-1.5 py-0.5 ${
                  contractDefined
                    ? 'border-running/35 bg-running/10'
                    : 'border-text-3/30 bg-text-3/10'
                }`}
                title={contractDefined ? 'Data contract defined' : 'No data contract defined'}
              >
                <ShieldCheck className={`h-2.5 w-2.5 ${contractDefined ? 'text-running' : 'text-text-3'}`} />
              </div>
            </button>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
});

DataFlowEdge.displayName = 'DataFlowEdge';
