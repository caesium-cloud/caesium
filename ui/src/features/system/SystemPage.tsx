import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";
import { Activity, Database, CheckCircle2, XCircle, Clock, Server, Zap, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";

function StatusDot({ ok }: { ok: boolean }) {
  return ok
    ? <span className="inline-block h-2 w-2 rounded-full bg-green-500" />
    : <span className="inline-block h-2 w-2 rounded-full bg-destructive animate-pulse" />;
}

function formatUptime(ns: number): string {
  const seconds = Math.floor(ns / 1e9);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
}

export function SystemPage() {
  const {
    data: health,
    isLoading,
    error,
    refetch,
    dataUpdatedAt,
  } = useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 15000,
    retry: false,
  });

  const isHealthy = health?.status === "healthy";
  const db = health?.checks?.database;
  const activeRuns = health?.checks?.active_runs;
  const triggers = health?.checks?.triggers;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">System</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Cluster health and observability</p>
        </div>
        <div className="flex items-center gap-3">
          {dataUpdatedAt > 0 && (
            <span className="text-xs text-muted-foreground">
              Updated <RelativeTime date={new Date(dataUpdatedAt).toISOString()} />
            </span>
          )}
          <Button variant="outline" size="sm" onClick={() => refetch()}>
            <RefreshCw className="h-3.5 w-3.5 mr-1.5" />
            Refresh
          </Button>
        </div>
      </div>

      {/* Health status banner */}
      {!isLoading && (
        <div className={`rounded-lg border px-4 py-3 flex items-center gap-3 ${
          error
            ? "border-destructive bg-destructive/10"
            : isHealthy
              ? "border-green-500/30 bg-green-500/5"
              : "border-yellow-500/30 bg-yellow-500/5"
        }`}>
          {error
            ? <XCircle className="h-5 w-5 text-destructive shrink-0" />
            : isHealthy
              ? <CheckCircle2 className="h-5 w-5 text-green-500 shrink-0" />
              : <Activity className="h-5 w-5 text-yellow-500 shrink-0" />
          }
          <div>
            <p className="font-medium text-sm">
              {error
                ? "Health check failed — API unreachable"
                : isHealthy
                  ? "All systems operational"
                  : "System degraded"}
            </p>
            {health?.uptime != null && (
              <p className="text-xs text-muted-foreground">
                Uptime: {formatUptime(health.uptime)}
              </p>
            )}
          </div>
        </div>
      )}

      {/* KPI cards */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        <Card>
          <CardHeader className="pb-2 flex flex-row items-center justify-between space-y-0">
            <CardTitle className="text-sm font-medium text-muted-foreground">Database</CardTitle>
            <Database className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-7 w-24" /> : (
              <div className="flex items-center gap-2">
                <StatusDot ok={db?.status === "healthy"} />
                <span className="text-lg font-semibold capitalize">{db?.status ?? "unknown"}</span>
              </div>
            )}
            {db?.latency_ms != null && (
              <p className="text-xs text-muted-foreground mt-1">{db.latency_ms} ms latency</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2 flex flex-row items-center justify-between space-y-0">
            <CardTitle className="text-sm font-medium text-muted-foreground">Active Runs</CardTitle>
            <Activity className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-7 w-16" /> : (
              <span className="text-2xl font-bold">{activeRuns?.count ?? 0}</span>
            )}
            <p className="text-xs text-muted-foreground mt-1">Currently executing</p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2 flex flex-row items-center justify-between space-y-0">
            <CardTitle className="text-sm font-medium text-muted-foreground">Triggers</CardTitle>
            <Zap className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-7 w-16" /> : (
              <span className="text-2xl font-bold">{triggers?.count ?? 0}</span>
            )}
            <p className="text-xs text-muted-foreground mt-1">Registered</p>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2 flex flex-row items-center justify-between space-y-0">
            <CardTitle className="text-sm font-medium text-muted-foreground">Status</CardTitle>
            <Server className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-7 w-20" /> : (
              <Badge variant={error ? "destructive" : isHealthy ? "success" : "secondary"}>
                {error ? "unreachable" : health?.status ?? "unknown"}
              </Badge>
            )}
            <p className="text-xs text-muted-foreground mt-1">API health</p>
          </CardContent>
        </Card>
      </div>

      {/* Checks breakdown */}
      {!isLoading && health?.checks && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Health Checks</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {[
              {
                key: "Database",
                result: db,
                detail: db?.latency_ms != null ? `${db.latency_ms} ms latency` : undefined,
              },
              {
                key: "Active Runs",
                result: activeRuns,
                detail: activeRuns?.count != null ? `${activeRuns.count} running` : undefined,
                alwaysOk: true,
              },
              {
                key: "Triggers",
                result: triggers,
                detail: triggers?.count != null ? `${triggers.count} registered` : undefined,
                alwaysOk: true,
              },
            ].map(({ key, result, detail, alwaysOk }) => (
              <div key={key} className="flex items-center justify-between py-2 border-b last:border-0">
                <div className="flex items-center gap-3">
                  <StatusDot ok={alwaysOk ? true : result?.status === "healthy"} />
                  <span className="text-sm font-medium">{key}</span>
                </div>
                <div className="flex items-center gap-3 text-sm text-muted-foreground">
                  {detail && <span className="font-mono text-xs">{detail}</span>}
                  {!alwaysOk && result?.status && (
                    <Badge variant={result.status === "healthy" ? "success" : "destructive"} className="text-[10px]">
                      {result.status}
                    </Badge>
                  )}
                </div>
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      {/* Prometheus metrics reference */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Clock className="h-4 w-4" />
            Prometheus Metrics
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground mb-3">
            Detailed metrics are exposed in Prometheus format at <code className="bg-muted px-1 rounded">/metrics</code>.
          </p>
          <div className="flex flex-wrap gap-2">
            {[
              "caesium_job_runs_total",
              "caesium_job_run_duration_seconds",
              "caesium_task_runs_total",
              "caesium_task_run_duration_seconds",
              "caesium_jobs_active",
              "caesium_worker_claims_total",
              "caesium_trigger_fires_total",
            ].map(m => (
              <code key={m} className="text-xs bg-muted px-2 py-1 rounded font-mono">{m}</code>
            ))}
          </div>
        </CardContent>
      </Card>

      {error && (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm">
          <p className="font-medium text-destructive">Unable to reach health endpoint</p>
          <p className="text-xs mt-1 text-muted-foreground">
            Ensure the Caesium API is running and the <code className="bg-muted px-1 rounded">/health</code> endpoint is accessible.
          </p>
        </div>
      )}
    </div>
  );
}
