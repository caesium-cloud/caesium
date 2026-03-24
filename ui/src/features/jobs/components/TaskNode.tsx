import { memo } from 'react';
import { Handle, Position, type NodeProps } from 'reactflow';
import { cn, shortId } from '@/lib/utils';
import {
  Activity,
  CheckCircle2,
  Circle,
  XCircle,
  Clock,
  Container,
  Cloud,
  Zap,
  Settings,
  Terminal as TerminalIcon,
  AlertTriangle,
  ArrowRightFromLine,
  ArrowLeftToLine,
  SkipForward,
  ShieldCheck,
} from 'lucide-react';
import { Duration } from '@/components/duration';

export const TaskNode = memo(({ data }: NodeProps) => {
  const { label, atom, status, isSelected, startedAt, completedAt, engine, command, error, outputCount, receivesOutputs, hasOutputSchema } = data;
  const taskLabel = typeof label === 'string' ? label : '';

  const getStatusIcon = () => {
    switch (status) {
      case 'completed':
      case 'succeeded':
        return <CheckCircle2 data-testid="status-icon-succeeded" className="w-5 h-5 text-green-400 fill-green-500/10" />;
      case 'failed':
        return <XCircle data-testid="status-icon-failed" className="w-5 h-5 text-red-400 fill-red-500/10" />;
      case 'running':
        return <Activity data-testid="status-icon-running" className="w-5 h-5 text-blue-400 animate-spin" />;
      case 'skipped':
        return <SkipForward data-testid="status-icon-skipped" className="w-5 h-5 text-slate-400" />;
      case 'pending':
        return <Clock data-testid="status-icon-pending" className="w-5 h-5 text-slate-500" />;
      default:
        return <Circle data-testid="status-icon-unknown" className="w-5 h-5 text-slate-600" />;
    }
  };

  const getEngineIcon = () => {
    const e = (engine || atom?.engine || '').toLowerCase();
    if (e.includes('docker')) return <Container data-testid="engine-icon-docker" className="w-3.5 h-3.5 text-blue-400" />;
    if (e.includes('kubernetes') || e.includes('k8s')) return <Cloud data-testid="engine-icon-kubernetes" className="w-3.5 h-3.5 text-blue-400" />;
    if (e.includes('podman')) return <Zap data-testid="engine-icon-podman" className="w-3.5 h-3.5 text-purple-400" />;
    if (e.includes('wasm')) return <Zap data-testid="engine-icon-wasm" className="w-3.5 h-3.5 text-yellow-400" />;
    return <Settings data-testid="engine-icon-unknown" className="w-3.5 h-3.5 text-slate-500" />;
  };

  const getProcessedCommand = () => {
    let cmd = command || atom?.command || [];
    if (typeof cmd === 'string') {
      try {
        cmd = JSON.parse(cmd);
      } catch {
        cmd = [cmd];
      }
    }

    const isShell = cmd.length >= 2 &&
      (cmd[0] === 'sh' || cmd[0] === 'bash' || cmd[0] === '/bin/sh' || cmd[0] === '/bin/bash') &&
      cmd[1] === '-c';

    return {
      args: isShell ? cmd.slice(2) : cmd,
      isShell
    };
  };

  const { args: commandArray, isShell } = getProcessedCommand();

  const getStatusColor = () => {
    switch (status) {
      case 'completed':
      case 'succeeded':
        return 'border-emerald-400/45 bg-[linear-gradient(160deg,hsl(var(--caesium-cyan)/0.16),hsl(160_84%_36%/0.2)_30%,hsl(var(--node-surface)/0.95)_78%)] shadow-[0_0_24px_rgba(16,185,129,0.16)]';
      case 'failed':
        return 'border-red-400/50 bg-[linear-gradient(160deg,hsl(var(--caesium-cyan)/0.14),hsl(8_86%_58%/0.18)_34%,hsl(var(--node-surface)/0.95)_80%)] shadow-[0_0_24px_rgba(248,113,113,0.16)]';
      case 'running':
        return 'border-caesium-cyan/70 bg-[linear-gradient(155deg,hsl(var(--caesium-cyan)/0.28),hsl(var(--caesium-cyan)/0.12)_36%,hsl(var(--node-surface)/0.94)_78%)] shadow-[0_0_30px_rgba(0,180,216,0.28)]';
      case 'skipped':
        return 'border-slate-500/30 bg-[linear-gradient(155deg,hsl(var(--caesium-cyan)/0.06),hsl(var(--node-surface)/0.92)_32%)] shadow-none opacity-60';
      default:
        return 'border-caesium-cyan/35 bg-[linear-gradient(155deg,hsl(var(--caesium-cyan)/0.18),hsl(var(--caesium-cyan)/0.08)_32%,hsl(var(--node-surface)/0.94)_78%)] shadow-[0_0_22px_rgba(0,180,216,0.14)]';
    }
  };

  const shortImage = (image: string) => {
    if (!image) return 'unknown';
    const parts = image.split('/');
    return parts[parts.length - 1];
  };

  return (
    <div
      className={cn(
        'relative h-[148px] w-[300px] overflow-hidden rounded-xl border-2 px-4 py-2 transition-all duration-300',
        getStatusColor(),
        isSelected ? 'ring-2 ring-primary ring-offset-2 ring-offset-background' : ''
      )}
    >
      <Handle type="target" position={Position.Left} className="h-3 w-3 border-2 border-dag-bg bg-caesium-cyan" />
      {receivesOutputs && (
        <div className="absolute -left-1 top-1/2 -translate-y-1/2 translate-x-3">
          <div className="flex items-center gap-0.5 rounded-full border border-violet-500/40 bg-violet-500/15 px-1.5 py-0.5">
            <ArrowLeftToLine className="h-2 w-2 text-violet-400" />
            <span className="text-[7px] font-bold text-violet-300">IN</span>
          </div>
        </div>
      )}

      <div className="flex h-full flex-col gap-2">
        {/* Row 1: Image & Status */}
        <div className="flex min-h-[44px] items-start justify-between gap-3">
          <div className="flex items-center gap-2 overflow-hidden">
            <div className="rounded-lg border border-caesium-cyan/20 bg-muted p-1.5 shadow-inner shadow-caesium-cyan/10">
              {getEngineIcon()}
            </div>
            <div className="flex flex-col min-w-0">
              <span className="text-[11px] font-bold truncate text-foreground" title={atom?.image}>
                {shortImage(atom?.image)}
              </span>
              <div className="flex items-center gap-1">
                {isShell && (
                  <span className="rounded border border-caesium-cyan/40 bg-caesium-cyan/15 px-1 text-[8px] font-black tracking-tighter text-caesium-cyan">
                    SHELL
                  </span>
                )}
                <span className="truncate text-[9px] font-mono text-muted-foreground">
                  {shortId(taskLabel)}
                </span>
              </div>
            </div>
          </div>
          <div className="flex min-h-[30px] min-w-[44px] flex-col items-end justify-between gap-1">
            {getStatusIcon()}
            <div className={cn("text-[9px] font-mono text-muted-foreground", !startedAt && "invisible")}>
              {startedAt ? (
                <Duration start={startedAt} end={completedAt} />
              ) : (
                "00:00"
              )}
            </div>
          </div>
        </div>

        <div
          className={cn(
            "custom-scrollbar h-[72px] overflow-y-auto rounded-lg border px-2.5 py-1.5 shadow-inner",
            error && status !== 'skipped'
              ? "border-red-500/20 bg-red-500/10"
              : error && status === 'skipped'
                ? "border-slate-500/20 bg-slate-500/5"
                : "border-caesium-cyan/20 bg-muted/70",
            isShell && !error && "border-caesium-cyan/30"
          )}
        >
          {error && status === 'skipped' ? (
            <div className="flex gap-2 items-start">
              <SkipForward className="w-3.5 h-3.5 text-slate-400 shrink-0 mt-0.5" />
              <div className="flex flex-col gap-0.5 min-w-0">
                <span className="text-[8px] font-bold text-slate-400/80 uppercase tracking-wider">Skipped</span>
                <span className="text-[9px] text-slate-400/70 font-mono leading-relaxed break-all line-clamp-3">
                  {error}
                </span>
              </div>
            </div>
          ) : error ? (
            <div className="flex gap-2 items-start">
              <AlertTriangle className="w-3.5 h-3.5 text-red-500 shrink-0 mt-0.5" />
              <div className="flex flex-col gap-0.5 min-w-0">
                <span className="text-[8px] font-bold text-red-500/80 uppercase tracking-wider">Error Details</span>
                <span className="text-[9px] text-red-400/90 font-mono leading-relaxed break-all line-clamp-3">
                  {error}
                </span>
              </div>
            </div>
          ) : commandArray.length > 0 ? (
            <div className="flex flex-col gap-1">
              {commandArray.map((arg: string, i: number) => (
                <div key={i} className="flex items-start gap-2 group">
                  <span className="mt-0.5 text-[10px] font-bold leading-none text-caesium-cyan/70 select-none">{isShell ? ">" : "-"}</span>
                  <span className="break-all font-mono text-[10px] leading-relaxed text-foreground/70 transition-colors group-hover:text-foreground">
                    {arg}
                  </span>
                </div>
              ))}
            </div>
          ) : (
            <div className="flex h-full items-center gap-2 opacity-50">
              <TerminalIcon className="h-3 w-3 text-caesium-cyan/55" />
              <span className="text-[10px] font-mono italic text-muted-foreground">no command</span>
            </div>
          )}
        </div>
      </div>

      {(outputCount > 0 || hasOutputSchema) && (
        <div className="absolute -right-1 top-1/2 -translate-x-3 -translate-y-1/2 flex flex-col items-end gap-1">
          {outputCount > 0 && (
            <div className="flex items-center gap-0.5 rounded-full border border-emerald-500/40 bg-emerald-500/15 px-1.5 py-0.5">
              <span className="text-[7px] font-bold text-emerald-300">OUT</span>
              <ArrowRightFromLine className="h-2 w-2 text-emerald-400" />
            </div>
          )}
          {hasOutputSchema && (
            <div className="flex items-center gap-0.5 rounded-full border border-blue-500/40 bg-blue-500/15 px-1.5 py-0.5" title="Output schema defined">
              <ShieldCheck className="h-2 w-2 text-blue-400" />
            </div>
          )}
        </div>
      )}
      <Handle type="source" position={Position.Right} className="h-3 w-3 border-2 border-dag-bg bg-caesium-cyan" />
    </div>
  );
});

TaskNode.displayName = 'TaskNode';
