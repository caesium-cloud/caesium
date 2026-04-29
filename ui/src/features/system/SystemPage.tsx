import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Activity, Database, CheckCircle2, XCircle, Clock, ScrollText, Server, Zap, RefreshCw, TerminalSquare } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useClusterHealth } from "./useClusterHealth";
import { UsageBar } from "@/components/ui/usage-bar";
import { AtomLogo } from "@/components/brand/atom-logo";
import { PROMETHEUS_METRICS } from "./metrics";
import type { Node } from "@/lib/api";
import React from "react";

function StatusDot({ ok }: { ok: boolean }) {
  return (
    <span 
      className={`inline-block h-2 w-2 rounded-full shadow-[0_0_6px] flex-shrink-0 ${
        ok 
          ? "bg-success shadow-success/60" 
          : "bg-danger shadow-danger/60 animate-pulse"
      }`} 
    />
  );
}

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${Math.floor(seconds % 60)}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}

export function SystemPage() {
  const queryClient = useQueryClient();
  const [pruneDialogOpen, setPruneDialogOpen] = useState(false);
  const health = useClusterHealth();
  const rawHealth = health.raw;
  
  const { data: nodes = [], isLoading: isLoadingNodes } = useQuery({
    queryKey: ["system-nodes"],
    queryFn: api.getSystemNodes,
    refetchInterval: 15000,
  });

  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
  });

  const pruneCacheMutation = useMutation({
    mutationFn: api.pruneCache,
    onSuccess: (result) => {
      setPruneDialogOpen(false);
      toast.success(`Pruned ${result.pruned} expired cache entries`);
      queryClient.invalidateQueries({ queryKey: ["jobs"] });
    },
    onError: (err: Error) => {
      toast.error(`Failed to prune cache: ${err.message}`);
    },
  });

  const db = rawHealth?.checks?.database;
  const activeRuns = rawHealth?.checks?.active_runs;
  const triggers = rawHealth?.checks?.triggers;
  const nodesCheck = rawHealth?.checks?.nodes;

  return (
    <div className="space-y-6 pb-12">
      <div className="flex items-end justify-between gap-4">
        <div>
          <div className="text-[10px] font-bold uppercase tracking-widest text-gold/85 mb-1">Cluster · Health</div>
          <h1 className="text-2xl font-bold tracking-tight m-0 leading-tight">System</h1>
          <p className="text-sm text-text-3 mt-1">Fleet observability, operator tools, and Prometheus reference</p>
        </div>
        <div className="flex items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => queryClient.invalidateQueries()} className="bg-transparent border-graphite/50 text-text-2 hover:bg-graphite/20 hover:text-text-1">
            <RefreshCw className="h-3.5 w-3.5 mr-1.5" />
            Refresh
          </Button>
        </div>
      </div>

      {/* Health banner */}
      <div className={`rounded-lg border px-4 py-3 flex items-center gap-3 ${
        health.state === "incident" || health.state === "unknown"
          ? "border-danger/35 bg-gradient-to-r from-danger/10 to-danger/0"
          : health.state === "operational"
            ? "border-success/35 bg-gradient-to-r from-success/10 to-success/0"
            : "border-gold/35 bg-gradient-to-r from-gold/10 to-gold/0"
      }`}>
        <div className="relative w-8 h-8 flex items-center justify-center flex-shrink-0">
          <span className={`absolute inset-0 rounded-full animate-pulse opacity-20 ${
            health.state === "incident" || health.state === "unknown" ? "bg-danger" : health.state === "operational" ? "bg-success" : "bg-gold"
          }`} />
          {health.state === "incident" || health.state === "unknown"
            ? <XCircle className="h-4 w-4 text-danger relative" />
            : health.state === "operational"
              ? <CheckCircle2 className="h-4 w-4 text-success relative" />
              : <Activity className="h-4 w-4 text-gold relative" />
          }
        </div>
        <div className="flex-1 min-w-0">
          <p className="font-medium text-sm text-text-1 truncate">
            {health.state === "incident" || health.state === "unknown"
              ? "Health check failed — API unreachable"
              : health.state === "operational"
                ? "All systems operational"
                : "System degraded"}
          </p>
          {health.uptimeSeconds != null && (
            <p className="text-[11px] font-mono text-text-3 mt-0.5 truncate">
              Uptime <span className="text-text-2">{formatUptime(health.uptimeSeconds)}</span>
            </p>
          )}
        </div>
        <span className={`inline-flex items-center px-2.5 py-1 rounded text-[11px] font-bold tracking-widest uppercase flex-shrink-0 ${
          health.state === "incident" || health.state === "unknown"
            ? "bg-danger/15 text-danger"
            : health.state === "operational"
              ? "bg-success/15 text-success"
              : "bg-gold/15 text-gold"
        }`}>
          {health.state}
        </span>
      </div>

      {/* KPI strips */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
        <SysKpi icon={Database} label="Database" value={<span className="capitalize text-success">{db?.status || "unknown"}</span>} sub={db?.latency_ms != null ? `${db.latency_ms}ms latency` : "--"} dot={db?.status === "healthy"} />
        <SysKpi icon={Activity} label="Active runs" value={<span className="font-mono text-cyan-glow">{activeRuns?.count ?? 0}</span>} sub="Currently executing" />
        <SysKpi icon={Zap} label="Triggers" value={<span className="font-mono">{triggers?.count ?? 0}</span>} sub="Registered" />
        <SysKpi icon={Server} label="Nodes" value={<span className="font-mono">{nodesCheck?.count ?? nodes.length}</span>} sub="Active workers" dot={nodes.length > 0} />
      </div>

      <div className="grid lg:grid-cols-[2fr_1fr] gap-4 items-start">
        {/* Nodes Table */}
        <Card className="bg-midnight/30 border-graphite/50 overflow-hidden rounded-lg">
          <div className="px-4 py-3 border-b border-graphite/50 flex justify-between items-center bg-obsidian/30">
            <div>
              <div className="text-[13px] font-medium text-text-1">Cluster nodes</div>
              <div className="text-[11px] text-text-3 mt-0.5">dqlite quorum · dynamic allocation</div>
            </div>
            <span className="font-mono text-[11px] text-text-3">{nodes.length} total</span>
          </div>
          <div className="grid grid-cols-[minmax(0,1.5fr)_80px_70px_100px_100px_90px] px-4 py-2 bg-obsidian/50 border-b border-graphite/50 text-[10px] font-semibold tracking-[0.16em] uppercase text-text-3">
            <span>Address</span><span>Role</span><span>Arch</span><span>CPU</span><span>Mem</span><span>Workers</span>
          </div>
          <div>
            {isLoadingNodes ? (
              <div className="p-4 space-y-3">
                <Skeleton className="h-6 w-full bg-graphite/10" />
                <Skeleton className="h-6 w-full bg-graphite/10" />
              </div>
            ) : nodes.length === 0 ? (
              <div className="p-8 text-center text-sm text-text-4">No nodes detected</div>
            ) : (
              nodes.map((n, i) => (
                <div key={n.address} className={`grid grid-cols-[minmax(0,1.5fr)_80px_70px_100px_100px_90px] px-4 py-3 items-center ${i !== nodes.length - 1 ? "border-b border-graphite/30" : ""}`}>
                  <div className="flex items-center gap-2.5 min-w-0 pr-2">
                    <StatusDot ok={true} />
                    <div className="min-w-0">
                      <div className="font-mono text-xs text-text-1 truncate">{n.address}</div>
                      <div className="font-mono text-[10px] text-text-4 truncate mt-0.5">uptime {n.uptime}</div>
                    </div>
                  </div>
                  <span className={`inline-flex px-2 py-0.5 rounded text-[10px] font-bold tracking-widest uppercase w-fit ${
                    n.role === "leader" 
                      ? "bg-gold/15 text-gold border border-gold/30" 
                      : "bg-cyan/10 text-cyan-glow border border-cyan/25"
                  }`}>
                    {n.role}
                  </span>
                  <span className="font-mono text-[11px] text-text-2 truncate">{n.arch}</span>
                  <div className="pr-4"><UsageBar value={n.cpu} /></div>
                  <div className="pr-4"><UsageBar value={n.mem} /></div>
                  <span className="font-mono text-xs text-text-2 truncate">{n.workers_busy}/{n.workers_total}</span>
                </div>
              ))
            )}
          </div>
        </Card>

        {/* Cluster Topology */}
        <Card className="bg-midnight/30 border-graphite/50 p-4">
          <div className="text-[13px] font-medium text-text-1 mb-1">Topology</div>
          <div className="text-[11px] text-text-3 mb-4">Quorum view</div>
          <ClusterTopology nodes={nodes} />
        </Card>
      </div>

      {/* Operator Tools */}
      <Card className="relative overflow-hidden border-cyan/25 bg-gradient-to-br from-midnight to-cyan/5 p-5">
        <div className="absolute -top-10 -right-10 w-[200px] h-[200px] opacity-5 pointer-events-none">
          <AtomLogo size={200} animated={false} />
        </div>
        <div className="flex justify-between items-start mb-4 relative z-10">
          <div>
            <div className="flex items-center gap-2">
              <TerminalSquare className="h-4 w-4 text-cyan-glow" />
              <span className="text-sm font-medium text-text-1">Operator tools</span>
            </div>
            <div className="text-[11px] text-text-3 mt-1">Read-only debugging surfaces — gated by environment variables</div>
          </div>
          <span className="text-[10px] font-bold tracking-widest uppercase px-2 py-1 rounded border border-gold/40 text-gold bg-gold/5">Power feature</span>
        </div>
        <div className="grid sm:grid-cols-3 gap-3 relative z-10">
          <ToolCard 
            icon={Database} 
            title="Database console" 
            desc="Schema-aware SQL with read-only safeguards." 
            env="CAESIUM_DATABASE_CONSOLE_ENABLED" 
            enabled={features?.database_console_enabled}
            to="/system/database"
          />
          <ToolCard 
            icon={ScrollText} 
            title="Log console" 
            desc="Stream server logs in real time with level filtering." 
            env="CAESIUM_LOG_CONSOLE_ENABLED" 
            enabled={features?.log_console_enabled}
            to="/system/logs"
          />
          <ToolCard 
            icon={RefreshCw} 
            title="Cache maintenance" 
            desc="Prune expired task cache entries across all jobs." 
            onClick={() => setPruneDialogOpen(true)}
            enabled={true} // Always available
          />
        </div>
      </Card>

      <div className="grid md:grid-cols-2 gap-4">
        {/* Health Checks */}
        <Card className="bg-midnight/30 border-graphite/50 overflow-hidden">
          <div className="px-4 py-3 border-b border-graphite/50">
            <div className="text-[13px] font-medium text-text-1">Health checks</div>
            <div className="text-[11px] text-text-3 mt-0.5">Polled every 15s · {health.state === "operational" ? "all green" : "review degraded items"}</div>
          </div>
          <div className="flex flex-col">
            {[
              { key: "Database", result: db, detail: db?.latency_ms != null ? `${db.latency_ms} ms latency` : undefined },
              { key: "Active Runs", result: activeRuns, detail: activeRuns?.count != null ? `${activeRuns.count} running` : undefined, alwaysOk: true },
              { key: "Triggers", result: triggers, detail: triggers?.count != null ? `${triggers.count} registered` : undefined, alwaysOk: true },
              { key: "Nodes", result: nodesCheck, detail: nodesCheck?.count != null ? `${nodesCheck.count} tracking` : undefined, alwaysOk: true },
            ].map((c, i, arr) => (
              <div key={c.key} className={`flex justify-between items-center px-4 py-3 ${i !== arr.length - 1 ? "border-b border-graphite/30" : ""}`}>
                <div className="flex items-center gap-2.5">
                  <StatusDot ok={c.alwaysOk ? true : c.result?.status === "healthy"} />
                  <span className="text-[13px] text-text-1">{c.key}</span>
                </div>
                <span className="font-mono text-[11px] text-text-3">{c.detail || "--"}</span>
              </div>
            ))}
          </div>
        </Card>

        {/* Prometheus Metrics */}
        <Card className="bg-midnight/30 border-graphite/50 overflow-hidden">
          <div className="px-4 py-3 border-b border-graphite/50 flex justify-between items-center">
            <div>
              <div className="text-[13px] font-medium text-text-1 flex items-center gap-2">
                <Clock className="h-3.5 w-3.5 text-text-3" />
                Prometheus metrics
              </div>
              <div className="text-[11px] text-text-3 mt-1">
                Exposed at <code className="font-mono bg-obsidian px-1.5 py-0.5 rounded text-cyan-glow">/metrics</code>
              </div>
            </div>
            <Button variant="ghost" size="sm" asChild className="h-7 text-xs text-text-3 hover:text-text-1 border border-graphite/50">
              <a href="/metrics" target="_blank" rel="noreferrer">Open</a>
            </Button>
          </div>
          <div className="p-4 flex flex-wrap gap-1.5 max-h-[250px] overflow-y-auto custom-scrollbar">
            {PROMETHEUS_METRICS.map((m) => (
              <code key={m.name} className="font-mono text-[10.5px] px-2 py-1 rounded bg-obsidian border border-graphite/50 text-text-2 hover:border-cyan/50 hover:text-cyan-glow transition-colors cursor-default" title={m.help}>
                {m.name}
              </code>
            ))}
          </div>
        </Card>
      </div>

      <Dialog open={pruneDialogOpen} onOpenChange={setPruneDialogOpen}>
        <DialogContent className="sm:max-w-[425px] bg-midnight border-graphite/50 text-text-1">
          <DialogHeader>
            <DialogTitle className="text-lg">Prune expired cache entries</DialogTitle>
            <DialogDescription className="text-text-3 mt-1.5">
              This removes only expired records across all jobs. Active cache entries are not affected and will continue to serve hits.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter className="mt-4 gap-2 sm:gap-0">
            <Button variant="outline" onClick={() => setPruneDialogOpen(false)} disabled={pruneCacheMutation.isPending} className="bg-transparent border-graphite/50 text-text-2 hover:text-text-1 hover:bg-graphite/20">
              Cancel
            </Button>
            <Button onClick={() => pruneCacheMutation.mutate()} disabled={pruneCacheMutation.isPending} className="bg-cyan-glow text-midnight hover:bg-cyan-dim">
              {pruneCacheMutation.isPending ? "Pruning..." : "Confirm prune"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function SysKpi({ icon: Icon, label, value, sub, dot }: { icon: React.ComponentType<{ className?: string }>; label: string; value: React.ReactNode; sub: string; dot?: boolean }) {
  return (
    <Card className="bg-midnight/30 border-graphite/50 p-3.5 hover:bg-midnight/50 transition-colors">
      <div className="flex justify-between items-center mb-2">
        <span className="text-[10px] font-bold tracking-widest uppercase text-text-3">{label}</span>
        <Icon className="h-3.5 w-3.5 text-text-4" />
      </div>
      <div className="flex items-center gap-2">
        {dot && <StatusDot ok={true} />}
        <div className="text-[22px] font-medium text-text-1 tracking-tight leading-none">{value}</div>
      </div>
      <div className="font-mono text-[10px] text-text-4 mt-2 truncate">{sub}</div>
    </Card>
  );
}

function ClusterTopology({ nodes }: { nodes: Node[] }) {
  const cx = 110, cy = 110, R = 70;
  return (
    <svg viewBox="0 0 220 220" className="w-full h-[220px]">
      <defs>
        <radialGradient id="topo-glow" cx="0.5" cy="0.5" r="0.5">
          <stop offset="0%" stopColor="var(--cyan)" stopOpacity="0.3" />
          <stop offset="100%" stopColor="var(--cyan)" stopOpacity="0" />
        </radialGradient>
      </defs>
      {/* connections */}
      {nodes.map((_n1, i) => nodes.slice(i + 1).map((_n2, j) => {
        const a1 = (i / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const a2 = ((i + j + 1) / nodes.length) * Math.PI * 2 - Math.PI / 2;
        return (
          <line key={`${i}-${j}`}
            x1={cx + Math.cos(a1) * R} y1={cy + Math.sin(a1) * R}
            x2={cx + Math.cos(a2) * R} y2={cy + Math.sin(a2) * R}
            stroke="var(--cyan)" strokeOpacity="0.3" strokeWidth="1" strokeDasharray="2 3"
          />
        );
      }))}
      {/* center label */}
      <circle cx={cx} cy={cy} r="40" fill="url(#topo-glow)" />
      <text x={cx} y={cy - 2} textAnchor="middle" fontSize="9" fill="var(--text-3)" style={{ letterSpacing: "0.18em", textTransform: "uppercase", fontWeight: 600 }}>quorum</text>
      <text x={cx} y={cy + 12} textAnchor="middle" fontSize="14" fill="var(--text-1)" fontWeight="500" className="font-mono">{nodes.length}/{nodes.length}</text>
      {/* nodes */}
      {nodes.map((n, i) => {
        const angle = (i / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const x = cx + Math.cos(angle) * R;
        const y = cy + Math.sin(angle) * R;
        const isLeader = n.role === "leader";
        return (
          <g key={n.address}>
            <circle cx={x} cy={y} r="14" fill={isLeader ? "var(--gold)" : "var(--cyan)"} opacity="0.18" />
            <circle cx={x} cy={y} r="8" fill={isLeader ? "var(--gold)" : "var(--cyan)"} stroke="var(--midnight)" strokeWidth="2" />
            <text x={x} y={y + Math.sin(angle) * 24 + (Math.sin(angle) > 0 ? 8 : -2)} textAnchor="middle" fontSize="9" fill="var(--text-2)" className="font-mono">{n.address.split(":")[0]}</text>
          </g>
        );
      })}
    </svg>
  );
}

function ToolCard({ icon: Icon, title, desc, env, enabled, onClick, to }: { icon: React.ComponentType<{ className?: string }>; title: string; desc: string; env?: string; enabled?: boolean; onClick?: () => void; to?: string }) {
  const content = (
    <div className={`
      p-3.5 rounded-lg border transition-all duration-160 flex flex-col gap-2 h-full
      ${to || onClick ? "cursor-pointer bg-obsidian/60 border-graphite/50 hover:bg-obsidian hover:border-cyan/40 group" : "bg-obsidian/40 border-graphite/30"}
    `}>
      <div className="flex justify-between items-center">
        <Icon className="w-4 h-4 text-cyan-glow" />
        {enabled != null && (
          <span className={`
            text-[9px] font-bold tracking-[0.12em] uppercase px-1.5 py-0.5 rounded-[3px]
            ${enabled ? "bg-success/15 text-success" : "bg-graphite text-text-3"}
          `}>
            {enabled ? "Enabled" : "Disabled"}
          </span>
        )}
      </div>
      <div className="flex-1">
        <div className="text-[13px] font-medium text-text-1 group-hover:text-cyan-glow transition-colors">{title}</div>
        <div className="text-[11px] text-text-3 mt-1 leading-relaxed">{desc}</div>
      </div>
      {env && (
        <code className="font-mono text-[10px] text-text-4 bg-void px-1.5 py-0.5 rounded-[3px] mt-auto truncate w-full text-center" title={env}>
          {env}
        </code>
      )}
    </div>
  );

  if (to) {
    return (
      <Link to={to} className="block outline-none focus-visible:ring-2 focus-visible:ring-cyan-glow rounded-lg">
        {content}
      </Link>
    );
  }

  return (
    <div onClick={onClick} role="button" tabIndex={0} className="block outline-none focus-visible:ring-2 focus-visible:ring-cyan-glow rounded-lg">
      {content}
    </div>
  );
}
