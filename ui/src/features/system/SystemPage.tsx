import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Activity, Database, CheckCircle2, XCircle, Clock, Server, Zap, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";

function StatusDot({ ok }: { ok: boolean }) {
  return ok
    ? <span className="inline-block h-2 w-2 rounded-full bg-green-500 animate-pulse" />
    : <span className="inline-block h-2 w-2 rounded-full bg-destructive" />;
}

export function SystemPage() {
  const {
    data: health,
    isLoading: isLoadingHealth,
    error: healthError,
    refetch,
    dataUpdatedAt,
  } = useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 15000,
    retry: false,
  });

  const checks = health?.checks ?? {};

  const isDbOk = (() => {
    const db = checks["database"] as Record<string, unknown> | undefined;
    return db?.status === "ok" || db?.status === "pass";
  })();

  const activeRuns = (() => {
    const ar = checks["active_runs"] as Record<string, unknown> | undefined;
    return typeof ar?.count === "number" ? ar.count : (ar?.value ?? "—");
  })();

  const triggerCount = (() => {
    const t = checks["triggers"] as Record<string, unknown> | undefined;
    return typeof t?.count === "number" ? t.count : (t?.value ?? "—");
  })();

  const dbLatency = (() => {
    const db = checks["database"] as Record<string, unknown> | undefined;
    return db?.latency_ms ?? db?.latency ?? null;
  })();

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
      {!isLoadingHealth && (
        <div className={`rounded-lg border px-4 py-3 flex items-center gap-3 ${
          healthError ? "border-destructive bg-destructive/10" :
          health?.status === "ok" || health?.status === "pass" ? "border-green-500/30 bg-green-500/5" : "border-yellow-500/30 bg-yellow-500/5"
        }`}>
          {healthError
            ? <XCircle className="h-5 w-5 text-destructive shrink-0" />
            : health?.status === "ok" || health?.status === "pass"
              ? <CheckCircle2 className="h-5 w-5 text-green-500 shrink-0" />
              : <Activity className="h-5 w-5 text-yellow-500 shrink-0" />
          }
          <div>
            <p className="font-medium text-sm">
              {healthError ? "Health check failed" :
               health?.status === "ok" || health?.status === "pass" ? "All systems operational" : `Degraded: ${health?.status}`}
            </p>
            {health?.uptime && (
              <p className="text-xs text-muted-foreground">Uptime: {health.uptime}</p>
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
            {isLoadingHealth ? <Skeleton className="h-7 w-20" /> : (
              <div className="flex items-center gap-2">
                <StatusDot ok={isDbOk} />
                <span className="text-lg font-semibold">{isDbOk ? "Connected" : "Error"}</span>
              </div>
            )}
            {dbLatency != null && (
              <p className="text-xs text-muted-foreground mt-1">{String(dbLatency)} ms latency</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2 flex flex-row items-center justify-between space-y-0">
            <CardTitle className="text-sm font-medium text-muted-foreground">Active Runs</CardTitle>
            <Activity className="h-4 w-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {isLoadingHealth ? <Skeleton className="h-7 w-16" /> : (
              <span className="text-2xl font-bold">{String(activeRuns)}</span>
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
            {isLoadingHealth ? <Skeleton className="h-7 w-16" /> : (
              <span className="text-2xl font-bold">{String(triggerCount)}</span>
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
            {isLoadingHealth ? <Skeleton className="h-7 w-20" /> : (
              <Badge variant={healthError ? "destructive" : health?.status === "ok" ? "success" : "secondary"}>
                {healthError ? "unreachable" : health?.status ?? "unknown"}
              </Badge>
            )}
            <p className="text-xs text-muted-foreground mt-1">API health</p>
          </CardContent>
        </Card>
      </div>

      {/* Raw health checks breakdown */}
      {!isLoadingHealth && health && Object.keys(checks).length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Health Checks</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Check</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {Object.entries(checks).map(([key, value]) => {
                  const v = value as Record<string, unknown>;
                  const ok = v?.status === "ok" || v?.status === "pass";
                  return (
                    <TableRow key={key}>
                      <TableCell className="font-medium capitalize">{key.replace(/_/g, " ")}</TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <StatusDot ok={ok} />
                          <span className="text-sm">{String(v?.status ?? "unknown")}</span>
                        </div>
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground font-mono">
                        {Object.entries(v || {})
                          .filter(([k]) => k !== "status")
                          .map(([k, val]) => `${k}: ${String(val)}`)
                          .join(", ") || "—"}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Prometheus metrics link */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base flex items-center gap-2">
            <Clock className="h-4 w-4" />
            Prometheus Metrics
          </CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground mb-3">
            Detailed metrics are exposed in Prometheus format. Available metrics include job run counts, task durations, worker claims, and trigger fires.
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
          <p className="text-xs text-muted-foreground mt-3">
            Scrape endpoint: <code className="bg-muted px-1 rounded">/metrics</code>
          </p>
        </CardContent>
      </Card>

      {healthError && (
        <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
          <p className="font-medium">Unable to reach health endpoint</p>
          <p className="text-xs mt-1 text-muted-foreground">
            Ensure the Caesium API is running and the <code>/health</code> endpoint is accessible.
          </p>
        </div>
      )}
    </div>
  );
}
