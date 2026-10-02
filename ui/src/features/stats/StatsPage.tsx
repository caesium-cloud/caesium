import { PageHeader as ConsolePageHeader } from "@/components/ui/page-header";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { StatusBadge } from "@/components/ui/status-badge";
import { TrendChart } from "./components/TrendChart";
import { FailureAtomsChart } from "./components/FailureAtomsChart";
import { IncidentAnalytics } from "./components/IncidentAnalytics";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

export function StatsPage() {
  const [window, setWindow] = useState("7d");

  const { data: stats, isLoading, error } = useQuery({
    queryKey: ["stats", "summary", window],
    queryFn: () => api.getStatsSummary(window),
  });

  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });

  if (error) {
    return (
      <div className="flex flex-col items-center justify-center p-12 text-center">
        <div className="text-destructive mb-2 font-bold">Error loading statistics</div>
        <div className="text-text-3 text-sm">{error.message}</div>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <ConsolePageHeader title="Stats" description="Execution volume, reliability, and duration." />
      <div className="grid gap-4 md:grid-cols-2">
        <KPIItem title="Total jobs · current" value={stats?.jobs.total} isLoading={isLoading} />
        <KPIItem title="Recent runs · fixed 24h" value={stats?.jobs.recent_runs} isLoading={isLoading} />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border pt-4">
        <div><h2 className="text-sm font-bold">Execution analysis · {window}</h2><p className="mt-1 text-xs text-text-3">This range controls success rate, duration, charts, and job rankings below. Dates are UTC.</p></div>
        <Tabs value={window} onValueChange={setWindow}>
          <TabsList aria-label="Execution analysis range"><TabsTrigger value="24h">24h</TabsTrigger><TabsTrigger value="7d">7d</TabsTrigger><TabsTrigger value="30d">30d</TabsTrigger></TabsList>
        </Tabs>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <KPIItem
          title="Success Rate"
          value={stats ? `${(stats.jobs.success_rate * 100).toFixed(1)}%` : undefined}
          isLoading={isLoading}
        />
        <KPIItem
          title="Avg Duration"
          value={stats ? `${stats.jobs.avg_duration_seconds.toFixed(2)}s` : undefined}
          isLoading={isLoading}
        />
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Card className="bg-midnight border-border">
          <CardHeader className="pb-2">
            <div className="flex flex-wrap items-center justify-between gap-3"><CardTitle className="text-xs font-normal text-text-3 lowercase">Performance Trend · UTC</CardTitle><div className="flex gap-4"><span className="inline-flex items-center gap-2 text-xs text-cyan"><span aria-hidden="true" className="cs-status-glyph cs-status-solid" />run count</span><StatusBadge status="succeeded" label="success rate" size="sm" /></div></div>
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-[350px] w-full bg-graphite/10" /> : <TrendChart data={stats?.success_rate_trend ?? []} />}
          </CardContent>
        </Card>

        <Card className="bg-midnight border-border">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-bold text-text-3 lowercase">Top Failing Atoms</CardTitle>
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-[350px] w-full bg-graphite/10" /> : <FailureAtomsChart data={stats?.top_failing_atoms ?? []} />}
          </CardContent>
        </Card>
      </div>

      {features?.agent_remediation_enabled === true ? <IncidentAnalytics /> : null}

      <div className="grid gap-4 md:grid-cols-2">
        <section className="border-y border-border">
          <div className="pb-2">
            <h3 className="text-xs font-bold text-text-3 lowercase">Top Failing Jobs</h3>
          </div>
          <div>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent border-graphite/30">
                  <TableHead className="text-[11px] lowercase text-text-3">Job Alias</TableHead>
                  <TableHead className="text-right text-[11px] lowercase text-text-3">Failures</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {isLoading ? (
                  Array.from({ length: 5 }).map((_, i) => (
                    <TableRow key={i} className="border-graphite/10">
                      <TableCell><Skeleton className="h-4 w-32 bg-graphite/10" /></TableCell>
                      <TableCell className="text-right"><Skeleton className="h-4 w-8 ml-auto bg-graphite/10" /></TableCell>
                    </TableRow>
                  ))
                ) : (
                  stats?.top_failing.map((job) => (
                    <TableRow key={job.job_id} className="border-graphite/10 hover:bg-graphite/5 transition-colors group">
                      <TableCell className="text-sm text-cyan group-hover:text-cyan-glow transition-colors">
                        {job.alias || job.job_id}
                      </TableCell>
                      <TableCell className="text-right text-sm text-text-2">{job.failure_count}</TableCell>
                    </TableRow>
                  ))
                )}
                {!isLoading && stats?.top_failing.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={2} className="text-center text-text-3 h-24 italic text-sm">No failures recorded</TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
        </section>

        <section className="border-y border-border">
          <div className="pb-2">
            <h3 className="text-xs font-bold text-text-3 lowercase">Slowest Jobs</h3>
          </div>
          <div>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent border-graphite/30">
                  <TableHead className="text-[11px] lowercase text-text-3">Job Alias</TableHead>
                  <TableHead className="text-right text-[11px] lowercase text-text-3">Avg Duration</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {isLoading ? (
                  Array.from({ length: 5 }).map((_, i) => (
                    <TableRow key={i} className="border-graphite/10">
                      <TableCell><Skeleton className="h-4 w-32 bg-graphite/10" /></TableCell>
                      <TableCell className="text-right"><Skeleton className="h-4 w-8 ml-auto bg-graphite/10" /></TableCell>
                    </TableRow>
                  ))
                ) : (
                  stats?.slowest_jobs.map((job) => (
                    <TableRow key={job.job_id} className="border-graphite/10 hover:bg-graphite/5 transition-colors group">
                      <TableCell className="text-sm text-text-2 group-hover:text-text-1 transition-colors">
                        {job.alias || job.job_id}
                      </TableCell>
                      <TableCell className="text-right text-sm text-text-2">{job.avg_duration_seconds.toFixed(2)}s</TableCell>
                    </TableRow>
                  ))
                )}
                {!isLoading && stats?.slowest_jobs.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={2} className="text-center text-text-3 h-24 italic text-sm">No data available</TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
        </section>
      </div>
    </div>
  );
}

function KPIItem({ title, value, isLoading }: { title: string; value: string | number | undefined; isLoading: boolean }) {
  return (
    <div className="border-b border-border py-3">
      <p className="mb-2 text-xs lowercase text-text-3">{title}</p>
      {isLoading ? <Skeleton className="h-8 w-24" /> : <div className="text-[22px] text-text-1">{value ?? "--"}</div>}
    </div>
  );
}
