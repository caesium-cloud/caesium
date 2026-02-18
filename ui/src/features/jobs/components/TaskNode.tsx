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
        return <CheckCircle2 className="w-5 h-5 text-green-400 fill-green-500/10" />;
      case 'failed':
        return <XCircle className="w-5 h-5 text-red-400 fill-red-500/10" />;
      case 'running':
        return <Activity className="w-5 h-5 text-blue-400 animate-spin" />;
      case 'pending':
        return <Clock className="w-5 h-5 text-slate-500" />;
      default:
        return <Circle className="w-5 h-5 text-slate-600" />;
    }
  };

  const getEngineIcon = () => {
    const e = (engine || atom?.engine || '').toLowerCase();
    if (e.includes('docker')) return <Container className="w-3.5 h-3.5 text-blue-400" />;
    if (e.includes('kubernetes') || e.includes('k8s')) return <Cloud className="w-3.5 h-3.5 text-blue-400" />;
    if (e.includes('podman')) return <Zap className="w-3.5 h-3.5 text-purple-400" />;
    if (e.includes('wasm')) return <Zap className="w-3.5 h-3.5 text-yellow-400" />;
    return <Settings className="w-3.5 h-3.5 text-slate-500" />;
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
        return 'border-green-500/40 bg-green-500/5 shadow-[0_0_20px_rgba(34,197,94,0.1)]';
      case 'failed':
        return 'border-red-500/40 bg-red-500/5 shadow-[0_0_20px_rgba(239,68,68,0.1)]';
      case 'running':
        return 'border-blue-500/60 bg-blue-500/10 shadow-[0_0_25px_rgba(59,130,246,0.3)]';
      default:
        return 'border-slate-800 bg-slate-900/40';
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
        isSelected ? 'ring-2 ring-primary ring-offset-2 ring-offset-slate-950' : ''
      )}
    >
      <Handle type="target" position={Position.Left} className="w-3 h-3 bg-slate-700 border-2 border-slate-900" />
      
      <div className="flex flex-col gap-3">
        {/* Row 1: Image & Status */}
        <div className="flex items-center justify-between gap-3">
          <div className="flex items-center gap-2 overflow-hidden">
            <div className="p-1.5 rounded-lg bg-slate-800/60 border border-slate-700/50 shadow-inner">
              {getEngineIcon()}
            </div>
            <div className="flex flex-col min-w-0">
              <span className="text-[11px] font-bold truncate text-slate-200" title={atom?.image}>
                {shortImage(atom?.image)}
              </span>
              <div className="flex items-center gap-1">
                {isShell && (
                  <span className="text-[8px] font-black px-1 rounded bg-blue-500/20 text-blue-400 border border-blue-500/30 tracking-tighter">
                    SHELL
                  </span>
                )}
                <span className="text-[9px] font-mono text-slate-500 truncate">
                  {label.substring(0, 8)}
                </span>
              </div>
            </div>
          </div>
          <div className="flex flex-col items-end gap-1">
            {getStatusIcon()}
            {startedAt && (
              <div className="text-[9px] font-mono text-slate-500">
                <Duration start={startedAt} end={completedAt} />
              </div>
            )}
          </div>
        </div>
        
        {/* Row 2: Command Summary (Structured) */}
        <div className={cn(
          "flex flex-col gap-1.5 px-2.5 py-2 rounded-lg bg-slate-950/60 border border-slate-800/80 max-h-32 overflow-y-auto custom-scrollbar shadow-inner",
          isShell && "border-blue-500/10"
        )}>
          {commandArray.length > 0 ? (
            commandArray.map((arg: string, i: number) => (
              <div key={i} className="flex gap-2 items-start group">
                <span className="text-blue-500/60 font-bold text-[10px] select-none leading-none mt-0.5">{isShell ? ">" : "-"}</span>
                <span className="text-[10px] font-mono text-slate-400 break-all leading-relaxed group-hover:text-slate-200 transition-colors">
                  {arg}
                </span>
              </div>
            ))
          ) : (
            <div className="flex gap-2 items-center opacity-50">
              <TerminalIcon className="w-3 h-3 text-slate-600" />
              <span className="text-[10px] font-mono text-slate-600 italic">no command</span>
            </div>
          )}
        </div>

        {/* Row 3: Error (if any) */}
        {error && (
          <div className="px-2.5 py-2 rounded-lg bg-red-500/10 border border-red-500/20 flex gap-2 items-start animate-in slide-in-from-top-1 duration-300">
            <AlertTriangle className="w-3.5 h-3.5 text-red-500 shrink-0 mt-0.5" />
            <div className="flex flex-col gap-0.5 min-w-0">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[8px] font-bold text-red-500/80 uppercase tracking-wider">Error Details</span>
                <span className="text-[7px] text-red-500/40 font-medium uppercase italic">Details ↗</span>
              </div>
              <span className="text-[9px] text-red-400/90 font-mono leading-relaxed break-all line-clamp-3">
                {error}
              </span>
            </div>
          </div>
        )}
      </div>

      <Handle type="source" position={Position.Right} className="w-3 h-3 bg-slate-700 border-2 border-slate-900" />
    </div>
  );
});

TaskNode.displayName = 'TaskNode';
