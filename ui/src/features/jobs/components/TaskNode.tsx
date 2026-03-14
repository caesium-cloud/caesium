import { memo } from 'react';
import { Handle, Position, type NodeProps } from 'reactflow';
import { cn } from '@/lib/utils';
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
  AlertTriangle
} from 'lucide-react';
import { Duration } from '@/components/duration';

export const TaskNode = memo(({ data }: NodeProps) => {
  const { label, atom, status, isSelected, startedAt, completedAt, engine, command, error } = data;

  const getStatusIcon = () => {
    switch (status) {
      case 'completed':
      case 'succeeded':
        return <CheckCircle2 data-testid="status-icon-succeeded" className="w-5 h-5 text-emerald-400 fill-emerald-500/10" />;
      case 'failed':
        return <XCircle data-testid="status-icon-failed" className="w-5 h-5 text-destructive fill-destructive/10" />;
      case 'running':
        return <Activity data-testid="status-icon-running" className="w-5 h-5 text-primary animate-spin" />;
      case 'pending':
        return <Clock data-testid="status-icon-pending" className="w-5 h-5 text-muted-foreground" />;
      default:
        return <Circle data-testid="status-icon-unknown" className="w-5 h-5 text-muted-foreground/60" />;
    }
  };

  const getEngineIcon = () => {
    const e = (engine || atom?.engine || '').toLowerCase();
    if (e.includes('docker')) return <Container data-testid="engine-icon-docker" className="w-3.5 h-3.5 text-primary" />;
    if (e.includes('kubernetes') || e.includes('k8s')) return <Cloud data-testid="engine-icon-kubernetes" className="w-3.5 h-3.5 text-primary" />;
    if (e.includes('podman')) return <Zap data-testid="engine-icon-podman" className="w-3.5 h-3.5 text-purple-400" />;
    if (e.includes('wasm')) return <Zap data-testid="engine-icon-wasm" className="w-3.5 h-3.5 text-caesium-gold" />;
    return <Settings data-testid="engine-icon-default" className="w-3.5 h-3.5 text-muted-foreground" />;
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
        return 'border-emerald-500/40 bg-emerald-500/5 shadow-[0_0_20px_rgba(16,185,129,0.1)]';
      case 'failed':
        return 'border-destructive/40 bg-destructive/5 shadow-[0_0_20px_rgba(239,68,68,0.1)]';
      case 'running':
        return 'border-primary/60 bg-primary/10 shadow-[0_0_25px_rgba(0,180,216,0.3)]';
      default:
        return 'border-border bg-card/40';
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
        'px-4 py-3 rounded-xl border-2 transition-all duration-300 min-w-[260px]',
        getStatusColor(),
        isSelected ? 'ring-2 ring-primary ring-offset-2 ring-offset-background' : ''
      )}
    >
      <Handle type="target" position={Position.Left} className="w-3 h-3 bg-muted border-2 border-background" />

      <div className="flex flex-col gap-3">
        {/* Row 1: Image & Status */}
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-center gap-2 overflow-hidden">
            <div className="p-1.5 rounded-lg bg-secondary/60 border border-border/50 shadow-inner">
              {getEngineIcon()}
            </div>
            <div className="flex flex-col min-w-0">
              <span className="text-[11px] font-bold truncate text-foreground/90" title={atom?.image}>
                {shortImage(atom?.image)}
              </span>
              <div className="flex items-center gap-1">
                {isShell && (
                  <span className="text-[8px] font-black px-1 rounded bg-primary/20 text-primary border border-primary/30 tracking-tighter">
                    SHELL
                  </span>
                )}
                <span className="text-[9px] font-mono text-muted-foreground truncate">
                  {label.substring(0, 8)}
                </span>
              </div>
            </div>
          </div>
          <div className="flex flex-col items-end gap-1">
            {getStatusIcon()}
            {startedAt && (
              <div className="text-[9px] font-mono text-muted-foreground">
                <Duration start={startedAt} end={completedAt} />
              </div>
            )}
          </div>
        </div>

        {/* Row 2: Command Summary (Structured) */}
        <div className={cn(
          "flex flex-col gap-1.5 px-2.5 py-2 rounded-lg bg-caesium-void/60 border border-border/80 max-h-32 overflow-y-auto custom-scrollbar shadow-inner",
          isShell && "border-primary/10"
        )}>
          {commandArray.length > 0 ? (
            commandArray.map((arg: string, i: number) => (
              <div key={i} className="flex gap-2 items-start group">
                <span className="text-primary/60 font-bold text-[10px] select-none leading-none mt-0.5">{isShell ? ">" : "-"}</span>
                <span className="text-[10px] font-mono text-muted-foreground break-all leading-relaxed group-hover:text-foreground/90 transition-colors">
                  {arg}
                </span>
              </div>
            ))
          ) : (
            <div className="flex gap-2 items-center opacity-50">
              <TerminalIcon className="w-3 h-3 text-muted-foreground/60" />
              <span className="text-[10px] font-mono text-muted-foreground/60 italic">no command</span>
            </div>
          )}
        </div>

        {/* Row 3: Error (if any) */}
        {error && (
          <div className="px-2.5 py-2 rounded-lg bg-destructive/10 border border-destructive/20 flex gap-2 items-start animate-in slide-in-from-top-1 duration-300">
            <AlertTriangle className="w-3.5 h-3.5 text-destructive shrink-0 mt-0.5" />
            <div className="flex flex-col gap-0.5 min-w-0">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[8px] font-bold text-destructive/80 uppercase tracking-wider">Error Details</span>
                <span className="text-[7px] text-destructive/40 font-medium uppercase italic">Details &#8599;</span>
              </div>
              <span className="text-[9px] text-destructive/90 font-mono leading-relaxed break-all line-clamp-3">
                {error}
              </span>
            </div>
          </div>
        )}
      </div>

      <Handle type="source" position={Position.Right} className="w-3 h-3 bg-muted border-2 border-background" />
    </div>
  );
});

TaskNode.displayName = 'TaskNode';
