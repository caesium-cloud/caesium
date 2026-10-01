import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { StatusGlyph } from "@/components/ui/status-badge";
import { statusMeta } from "@/lib/status";
import { clusterAtomProps } from "./cluster-atom";
import { Skeleton } from "@/components/ui/skeleton";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useClusterHealth } from "./useClusterHealth";
import { AtomLogo } from "@/components/brand/atom-logo";
import { PROMETHEUS_METRICS } from "./metrics";
import {
  checkTone,
  deriveQuorumView,
  deriveSystemBanner,
  mergeNodeRows,
  nodeLivenessLabel,
  reachabilityLabel,
  reachabilityTone,
  reachableNodeCount,
  type Tone,
} from "./quorum";
import React from "react";

function StatusDot({ tone, title }: { tone: Tone; title?: string }) {
  return <span title={title} data-tone={tone}><StatusGlyph meta={statusMeta(tone === "ok" ? "succeeded" : tone === "danger" ? "failed" : tone === "warn" ? "paused" : "unknown")} /></span>;
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
  const clusterCheck = rawHealth?.checks?.cluster;
  // Membership and liveness are read from separate fields on purpose: the
  // quorum numerator is what answered a probe, never the length of this list.
  const derive = { stale: health.stale };
  const quorum = deriveQuorumView(clusterCheck, derive);
  const banner = deriveSystemBanner(health.state, rawHealth, derive);
  // Liveness is merged from the CURRENT health observation; the node query is
  // only supplementary. It is authenticated, and its auth key lookup is itself
  // a leader-dependent read, so it is the request most likely to stall during
  // the very outage this page has to describe.
  const nodeRows = mergeNodeRows(rawHealth, nodes, derive);
  const reachableNodes = reachableNodeCount(nodeRows);
  // Node liveness spans every member, voters and non-voters alike.
  const nodeLiveness = health.stale ? null : nodeLivenessLabel(clusterCheck);
  const currentStatus = (status?: string) => health.stale ? "unknown" : status;

  return (
    <div className="space-y-6 pb-12">
      <div className="flex flex-col items-start gap-3 sm:flex-row sm:items-end sm:justify-between sm:gap-4">
        <div>

          <h1 className="text-2xl font-bold lowercase text-text-1">System</h1>

        </div>
        <div className="flex items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => queryClient.invalidateQueries()} className="bg-transparent border-graphite/50 text-text-2 hover:bg-graphite/20 hover:text-text-1">

            Refresh
          </Button>
        </div>
      </div>

      {/* Health banner */}
      <div
        data-testid="system-health-banner"
        data-tone={banner.tone}
        className={`px-4 py-3 flex items-center gap-3 ${
          banner.tone === "danger"
            ? "bg-danger/[.06] shadow-[inset_2px_0_0_hsl(var(--danger))]"
            : banner.tone === "ok"
              ? "border-y border-border"
              : "bg-gold/[.07]"
        }`}
      >
        <StatusDot tone={banner.tone} title={banner.badge} />
        <div className="flex-1 min-w-0">
          <p className="font-normal text-sm text-text-1 truncate">{banner.headline}</p>
          {health.uptimeSeconds != null && (
            <p className="text-[11px] text-text-3 mt-0.5 truncate">
              Uptime <span className="text-text-2">{formatUptime(health.uptimeSeconds)}</span>
            </p>
          )}
        </div>
        <span
          data-testid="system-health-badge"
          className={`inline-flex items-center px-2.5 py-1 rounded text-[11px] font-bold lowercase flex-shrink-0 ${
            banner.tone === "danger"
              ? "bg-danger/15 text-danger"
              : banner.tone === "ok"
                ? "bg-success/15 text-success"
                : "bg-gold/15 text-gold"
          }`}
        >
          {banner.badge}
        </span>
      </div>

      <div className="grid min-w-0 items-start gap-6 xl:grid-cols-[340px_minmax(0,1fr)]">
        <section className="flex flex-col items-center" aria-label="Cluster atom">
          <div className="relative"><AtomLogo size={300} {...clusterAtomProps(health)} /><span data-testid="quorum-count" className="absolute left-0 right-0 top-[58%] text-center text-[22px] text-text-2">{quorum.label}</span></div>
          <div className="space-y-2 text-xs text-text-3"><p>nucleus = leader</p><p>electron = reachable voter</p><p>hollow = voter not answering</p></div>
          <p className="mt-4 text-xs text-text-3" data-testid="quorum-detail">{quorum.detail}</p>
        </section>
        <div className="min-w-0 space-y-5">
      {/* KPI strips */}
      <div data-testid="system-kpis" className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <SysKpi label="Database" value={<span className="capitalize">{currentStatus(db?.status) || "unknown"}</span>} sub={!health.stale && db?.latency_ms != null ? `${db.latency_ms}ms latency` : "--"} tone={checkTone(currentStatus(db?.status))} />
        <SysKpi label="Active runs" value={<span className="text-cyan-glow">{health.stale ? "?" : activeRuns?.count ?? 0}</span>} sub={health.stale ? "Observation stale" : "Currently executing"} />
        <SysKpi label="Triggers" value={<span className="">{health.stale ? "?" : triggers?.count ?? 0}</span>} sub={health.stale ? "Observation stale" : "Registered"} />
        <SysKpi
          label="Nodes"
          value={
            <span className="" data-testid="system-nodes-kpi">
              {reachableNodes ?? "?"}
              <span className="text-text-3">/{nodeRows.length}</span>
            </span>
          }
          sub="Reachable / tracked"
          tone={health.stale ? "warn" : nodesCheck?.status ? checkTone(nodesCheck.status) : reachableNodes === null ? "warn" : "ok"}
        />
      </div>

      <div className="min-w-0">
        {/* Nodes Table */}
        <section className="min-w-0 bg-transparent border-border overflow-x-auto" tabIndex={0} aria-label="Cluster nodes">
          <div className="px-4 py-3 border-b border-graphite/50 flex justify-between items-center bg-transparent">
            <div>
              <div className="text-[13px] font-normal text-text-1">Cluster nodes</div>
              <div className="text-[11px] text-text-3 mt-0.5">dqlite membership and probed liveness</div>
            </div>
            <span className="text-[11px] text-text-3" data-testid="cluster-nodes-total">
              {reachableNodes ?? "?"}/{nodeRows.length} reachable
            </span>
          </div>
          <div className="grid min-w-[500px] grid-cols-[minmax(0,1.4fr)_90px_70px_70px] px-4 py-2 bg-transparent border-b border-graphite/50 text-[11px] font-bold lowercase text-text-3">
            <span>Address</span><span>Liveness</span><span>Role</span><span>Workers</span>
          </div>
          <div>
            {isLoadingNodes && nodeRows.length === 0 ? (
              <div className="p-4 space-y-3">
                <Skeleton className="h-6 w-full bg-graphite/10" />
                <Skeleton className="h-6 w-full bg-graphite/10" />
              </div>
            ) : nodeRows.length === 0 ? (
              <div className="p-8 text-center text-sm text-text-3">No nodes detected</div>
            ) : (
              nodeRows.map((n, i) => (
                <div
                  key={n.address}
                  data-testid="cluster-node-row"
                  data-address={n.address}
                  data-reachability={n.reachability}
                  data-liveness-current={n.livenessCurrent}
                  className={`grid min-w-[500px] grid-cols-[minmax(0,1.4fr)_90px_70px_70px] px-4 py-3 items-center ${i !== nodeRows.length - 1 ? "border-b border-graphite/30" : ""}`}
                >
                  <div className="flex items-center gap-2.5 min-w-0 pr-2">
                    <StatusDot tone={reachabilityTone(n.reachability)} title={reachabilityLabel(n.reachability)} />
                    <div className="min-w-0">
                      <div className="text-xs text-text-1 truncate">
                        {n.address}
                        {n.leader && <span className="ml-1.5 text-[11px] font-bold lowercase text-gold">leader</span>}
                      </div>
                      {n.latencyMs != null && (
                        <div className="text-[11px] text-text-3 truncate">{n.latencyMs} ms</div>
                      )}
                    </div>
                  </div>
                  <span className="text-[11px] text-text-2 truncate">{reachabilityLabel(n.reachability)}</span>
                  <span className="text-[11px] text-text-2 truncate">{n.role ?? "unknown"}</span>
                  <span className="text-xs text-text-2 truncate">
                    {n.workersBusy ?? "?"}/{n.workersTotal ?? "?"}
                  </span>
                </div>
              ))
            )}
          </div>
        </section>

      </div>
      <div className="border-t border-border pt-3 text-xs text-text-3">Leader resources, raft index, term and build SHA are not reported by this server.</div>
      {/* Operator tools */}
      <section className="border-t border-border pt-3">
        <div className="grid min-w-0 grid-cols-1 gap-1">
          <ToolCard
            title="Database console"
            desc="Schema-aware SQL with read-only safeguards."
            env="CAESIUM_DATABASE_CONSOLE_ENABLED"
            enabled={features?.database_console_enabled}
            to="/system/database"
          />
          <ToolCard
            title="Log console"
            desc="Stream server logs in real time with level filtering."
            env="CAESIUM_LOG_CONSOLE_ENABLED"
            enabled={features?.log_console_enabled}
            to="/system/logs"
          />
          <ToolCard
            title="Cache maintenance"
            desc="Prune expired task cache entries across all jobs."
            onClick={() => setPruneDialogOpen(true)}
            enabled={true} // Always available
          />
        </div>
      </section>

        </div>
      </div>
      <div className="grid md:grid-cols-2 gap-4">
        {/* Health Checks */}
        <section className="min-w-0 bg-transparent border-border overflow-x-auto" tabIndex={0} aria-label="Health checks">
          <div className="px-4 py-3 border-b border-graphite/50">
            <div className="text-[13px] font-normal text-text-1">Health checks</div>
            <div className="text-[11px] text-text-3 mt-0.5">Polled every 15s. {banner.tone === "ok" ? "all green" : "review degraded items"}</div>
          </div>
          <div className="flex flex-col">
            {[
              { key: "Database", status: db?.status, detail: db?.latency_ms != null ? `${db.latency_ms} ms latency` : undefined },
              { key: "Active Runs", status: activeRuns?.status, detail: activeRuns?.count != null ? `${activeRuns.count} running` : undefined },
              { key: "Triggers", status: triggers?.status, detail: triggers?.count != null ? `${triggers.count} registered` : undefined },
              {
                key: "Nodes",
                status: nodesCheck?.status,
                // Node liveness spans every member; quorum counts voters only.
                detail: nodeLiveness
                  ? `${nodeLiveness} nodes reachable`
                  : (nodesCheck?.count != null ? `${nodesCheck.count} tracking` : undefined),
              },
              ...(clusterCheck
                // The Quorum row reports the VOTER assessment specifically, not
                // the combined cluster status, so a dead standby degrades the
                // Nodes row above without misreporting the voter majority.
                ? [{ key: "Quorum", status: clusterCheck.quorum?.status, detail: quorum.detail }]
                : []),
            ].map((c, i, arr) => (
              <div
                key={c.key}
                data-testid="health-check-row"
                data-check={c.key}
                data-tone={health.stale ? "warn" : checkTone(c.status)}
                className={`flex justify-between items-center px-4 py-3 ${i !== arr.length - 1 ? "border-b border-graphite/30" : ""}`}
              >
                <div className="flex items-center gap-2.5">
                  <StatusDot tone={health.stale ? "warn" : checkTone(c.status)} title={health.stale ? "stale" : c.status ?? "unknown"} />
                  <span className="text-[13px] text-text-1">{c.key}</span>
                </div>
                <span className="text-[11px] text-text-3">{health.stale ? "Observation stale" : c.detail || "--"}</span>
              </div>
            ))}
          </div>
        </section>

        {/* Prometheus Metrics */}
        <section className="min-w-0 bg-transparent border-border overflow-x-auto" tabIndex={0} aria-label="Prometheus metrics">
          <div className="px-4 py-3 border-b border-graphite/50 flex justify-between items-center">
            <div>
              <div className="text-[13px] font-normal text-text-1 flex items-center gap-2">

                Prometheus metrics
              </div>
              <div className="text-[11px] text-text-3 mt-1">
                Exposed at <code className="bg-obsidian px-1.5 py-0.5 rounded text-cyan-glow">/metrics</code>
              </div>
            </div>
            <Button variant="ghost" size="sm" asChild className="h-7 text-xs text-text-3 hover:text-text-1 border border-graphite/50">
              <a href="/metrics" target="_blank" rel="noreferrer">Open</a>
            </Button>
          </div>
          <div className="p-4 flex flex-wrap gap-1.5 max-h-[250px] overflow-y-auto custom-scrollbar">
            {PROMETHEUS_METRICS.map((m) => (
              <code key={m.name} className="text-[11px] px-2 py-1 rounded bg-obsidian border border-graphite/50 text-text-2 hover:border-cyan/50 hover:text-cyan-glow transition-colors cursor-default" title={m.help}>
                {m.name}
              </code>
            ))}
          </div>
        </section>
      </div>

      <Dialog open={pruneDialogOpen} onOpenChange={setPruneDialogOpen}>
        <DialogContent className="bg-midnight border-graphite/50 p-4 text-text-1 sm:max-w-[425px] sm:rounded-md sm:p-6">
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
            <Button onClick={() => pruneCacheMutation.mutate()} disabled={pruneCacheMutation.isPending} className="bg-primary text-primary-foreground hover:bg-primary/90">
              {pruneCacheMutation.isPending ? "Pruning..." : "Confirm prune"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function SysKpi({ label, value, sub, tone }: { icon?: React.ComponentType<{ className?: string }>; label: string; value: React.ReactNode; sub: string; tone?: Tone }) {
  return (
    <section data-testid="system-kpi" className="border-b border-border py-3">
      <div className="flex justify-between items-center mb-2">
        <span className="text-[11px] font-bold lowercase text-text-3">{label}</span>
      </div>
      <div className="flex items-center gap-2">
        {tone && <StatusDot tone={tone} />}
        <div className="text-[22px] font-normal text-text-1 leading-none">{value}</div>
      </div>
      <div className="text-[11px] text-text-3 mt-2 truncate">{sub}</div>
    </section>
  );
}

function ToolCard({ title, desc, env, enabled, onClick, to }: { icon?: React.ComponentType<{ className?: string }>; title: string; desc: string; env?: string; enabled?: boolean; onClick?: () => void; to?: string }) {
  const content = <span className="flex min-w-0 min-h-8 w-full items-center gap-3 text-left text-[13px]">
    <span aria-hidden="true" className="text-cyan">❯</span><span className="shrink-0 text-text-1">{title === "Database console" ? "db" : title === "Log console" ? "logs" : "cache"}</span>
    <span className="min-w-0 flex-1 truncate text-xs text-text-3">{desc}</span>
    <span className="text-xs text-text-3">{enabled === true ? "enabled" : "disabled"}</span>
  </span>;
  if (to) return <Link to={to} aria-label={title} title={env} className="block min-w-0 max-w-full hover:bg-obsidian">{content}</Link>;
  return <button type="button" aria-label={title} onClick={onClick} className="block min-w-0 w-full max-w-full hover:bg-obsidian">{content}</button>;
}
