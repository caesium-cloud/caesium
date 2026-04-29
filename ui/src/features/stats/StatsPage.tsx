import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { TrendChart } from "./components/TrendChart";
import { FailureAtomsChart } from "./components/FailureAtomsChart";
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

  if (error) {
    return (
      <div className="flex flex-col items-center justify-center p-12 text-center">
        <div className="text-destructive mb-2 font-bold">Error loading statistics</div>
        <div className="text-text-3 text-sm">{error.message}</div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <p className="text-xs font-medium text-text-3 uppercase tracking-widest mb-1">System Intelligence</p>
          <h1 className="text-2xl font-bold tracking-tight">Operator Statistics</h1>
        </div>
        <Tabs value={window} onValueChange={setWindow} className="w-full sm:w-[300px]">
          <TabsList className="grid w-full grid-cols-3 bg-midnight border border-graphite/50">
            <TabsTrigger value="24h" className="data-[state=active]:bg-graphite/50">24h</TabsTrigger>
            <TabsTrigger value="7d" className="data-[state=active]:bg-graphite/50">7d</TabsTrigger>
            <TabsTrigger value="30d" className="data-[state=active]:bg-graphite/50">30d</TabsTrigger>
          </TabsList>
        </Tabs>
      </div>
      
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <KPIItem title="Total Jobs" value={stats?.jobs.total} isLoading={isLoading} />
        <KPIItem title="Recent Runs (24h)" value={stats?.jobs.recent_runs} isLoading={isLoading} />
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
        <Card className="bg-midnight/30 border-graphite/30 backdrop-blur-sm">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-semibold text-text-3 uppercase tracking-wider">Performance Trend</CardTitle>
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-[350px] w-full bg-graphite/10" /> : <TrendChart data={stats?.success_rate_trend ?? []} />}
          </CardContent>
        </Card>

        <Card className="bg-midnight/30 border-graphite/30 backdrop-blur-sm">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-semibold text-text-3 uppercase tracking-wider">Top Failing Atoms</CardTitle>
          </CardHeader>
          <CardContent>
            {isLoading ? <Skeleton className="h-[350px] w-full bg-graphite/10" /> : <FailureAtomsChart data={stats?.top_failing_atoms ?? []} />}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Card className="bg-midnight/30 border-graphite/30">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-semibold text-text-3 uppercase tracking-wider">Top Failing Jobs</CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent border-graphite/30">
                  <TableHead className="text-[10px] uppercase tracking-widest text-text-4">Job Alias</TableHead>
                  <TableHead className="text-right text-[10px] uppercase tracking-widest text-text-4">Failures</TableHead>
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
                      <TableCell className="font-mono text-sm text-cyan-dim group-hover:text-cyan-glow transition-colors">
                        {job.alias || job.job_id}
                      </TableCell>
                      <TableCell className="text-right font-mono text-sm text-text-2">{job.failure_count}</TableCell>
                    </TableRow>
                  ))
                )}
                {!isLoading && stats?.top_failing.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={2} className="text-center text-text-4 h-24 italic text-sm">No failures recorded</TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </CardContent>
        </Card>

        <Card className="bg-midnight/30 border-graphite/30">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-semibold text-text-3 uppercase tracking-wider">Slowest Jobs</CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent border-graphite/30">
                  <TableHead className="text-[10px] uppercase tracking-widest text-text-4">Job Alias</TableHead>
                  <TableHead className="text-right text-[10px] uppercase tracking-widest text-text-4">Avg Duration</TableHead>
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
                      <TableCell className="font-mono text-sm text-text-2 group-hover:text-text-1 transition-colors">
                        {job.alias || job.job_id}
                      </TableCell>
                      <TableCell className="text-right font-mono text-sm text-text-2">{job.avg_duration_seconds.toFixed(2)}s</TableCell>
                    </TableRow>
                  ))
                )}
                {!isLoading && stats?.slowest_jobs.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={2} className="text-center text-text-4 h-24 italic text-sm">No data available</TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function KPIItem({ title, value, isLoading }: { title: string; value: string | number | undefined; isLoading: boolean }) {
  return (
    <Card className="bg-midnight/50 border-graphite/50 overflow-hidden relative group">
      <div className="absolute inset-x-0 bottom-0 h-0.5 bg-gradient-to-r from-transparent via-cyan-glow/20 to-transparent opacity-0 group-hover:opacity-100 transition-opacity" />
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-1">
        <CardTitle className="text-[10px] font-bold text-text-3 uppercase tracking-widest">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-8 w-24 bg-graphite/10" />
        ) : (
          <div className="text-2xl font-bold text-text-1 tracking-tight">{value ?? "--"}</div>
        )}
      </CardContent>
    </Card>
  );
}
