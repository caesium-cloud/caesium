import { type ReactNode, useMemo } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { AlertTriangle, Bot, Clock, DollarSign, ShieldAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type Incident, type IncidentDetail } from "@/lib/api";
import {
  actionHasAgent,
  actionHasHuman,
  costFromSession,
  formatDurationMs,
  formatIncidentClass,
  formatMoney,
  resolutionMs,
  sessionProfileLabel,
} from "@/features/incidents/incident-utils";

const detailLimit = 40;

export function IncidentAnalytics() {
  const featuresQuery = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });
  const incidentsEnabled = featuresQuery.data?.agent_remediation_enabled === true;

  const incidentsQuery = useQuery({
    queryKey: ["incidents", "analytics"],
    queryFn: () => api.getIncidents({ limit: 200 }),
    enabled: incidentsEnabled,
    staleTime: 30_000,
  });

  const incidentIds = (incidentsQuery.data?.incidents ?? []).slice(0, detailLimit).map((incident) => incident.id);
  const detailQueries = useQueries({
    queries: incidentIds.map((id) => ({
      queryKey: ["incident", id],
      queryFn: () => api.getIncident(id),
      enabled: incidentsEnabled,
      staleTime: 30_000,
    })),
  });

  const details = detailQueries
    .map((query) => query.data)
    .filter((detail): detail is IncidentDetail => Boolean(detail));
  const analytics = useMemo(
    () => buildIncidentAnalytics(incidentsQuery.data?.incidents ?? [], details),
    [details, incidentsQuery.data?.incidents],
  );

  if (featuresQuery.isLoading || incidentsQuery.isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-32 w-full bg-graphite/10" />
        <Skeleton className="h-72 w-full bg-graphite/10" />
      </div>
    );
  }

  if (!incidentsEnabled) {
    return null;
  }

  if (incidentsQuery.error) {
    return (
      <Card className="border-danger/30 bg-danger/5">
        <CardContent className="flex items-center gap-3 p-4 text-sm text-danger">
          <AlertTriangle className="h-4 w-4" />
          {incidentsQuery.error.message}
        </CardContent>
      </Card>
    );
  }

  return (
    <section className="space-y-4" data-testid="incident-analytics">
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <MetricCard icon={<ShieldAlert className="h-4 w-4" />} title="Incidents" value={analytics.total} />
        <MetricCard icon={<Bot className="h-4 w-4" />} title="Autonomous rate" value={`${analytics.autonomousRate}%`} />
        <MetricCard icon={<Clock className="h-4 w-4" />} title="MTTR with agent" value={analytics.mttrWithAgent} />
        <MetricCard icon={<DollarSign className="h-4 w-4" />} title="Pages avoided" value={analytics.pagesAvoided} />
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_380px]">
        <Card className="border-graphite/30 bg-midnight/30">
          <CardHeader className="border-b border-border/50 pb-3">
            <CardTitle className="text-xs font-semibold uppercase tracking-wider text-text-3">
              Incidents by class over time
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 p-4">
            {analytics.byDay.length === 0 ? (
              <div className="text-sm text-text-3">No incidents recorded.</div>
            ) : (
              analytics.byDay.map((day) => (
                <div key={day.date} className="space-y-2">
                  <div className="flex items-center justify-between gap-3 text-xs">
                    <span className="font-mono text-text-3">{day.date}</span>
                    <span className="font-mono text-text-4">{day.total}</span>
                  </div>
                  <div className="flex h-3 overflow-hidden rounded-full bg-graphite/30">
                    {day.classes.map((entry) => (
                      <span
                        key={`${day.date}-${entry.className}`}
                        className="h-full border-r border-background/30 bg-cyan-glow/70 last:border-r-0"
                        style={{ width: `${Math.max(8, (entry.count / day.total) * 100)}%` }}
                        title={`${formatIncidentClass(entry.className)}: ${entry.count}`}
                      />
                    ))}
                  </div>
                  <div className="flex flex-wrap gap-1.5">
                    {day.classes.map((entry) => (
                      <Badge key={entry.className} variant="outline" className="text-[10px]">
                        {formatIncidentClass(entry.className)} {entry.count}
                      </Badge>
                    ))}
                  </div>
                </div>
              ))
            )}
          </CardContent>
        </Card>

        <div className="space-y-4">
          <Card className="border-graphite/30 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="text-xs font-semibold uppercase tracking-wider text-text-3">
                Top recurring
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 p-4">
              {analytics.topRecurring.length === 0 ? (
                <div className="text-sm text-text-3">No recurring incident keys.</div>
              ) : (
                analytics.topRecurring.map((incident) => (
                  <div key={incident.id} className="rounded-md border border-border/50 bg-background/50 p-3">
                    <div className="flex items-center justify-between gap-3">
                      <span className="truncate text-sm text-text-1">{formatIncidentClass(incident.class)}</span>
                      <Badge variant="outline" className="font-mono text-[10px]">
                        {incident.occurrence_count}x
                      </Badge>
                    </div>
                    <div className="mt-1 truncate font-mono text-[10px] text-text-4">{incident.dedupe_key}</div>
                  </div>
                ))
              )}
            </CardContent>
          </Card>

          <Card className="border-graphite/30 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="text-xs font-semibold uppercase tracking-wider text-text-3">
                Token and cost by profile
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 p-4">
              {analytics.profileCosts.length === 0 ? (
                <div className="text-sm text-text-3">No agent usage reported.</div>
              ) : (
                analytics.profileCosts.map((profile) => (
                  <div key={profile.profile} className="grid grid-cols-[minmax(0,1fr)_80px_90px] gap-2 text-xs">
                    <span className="truncate text-text-2">{profile.profile}</span>
                    <span className="text-right font-mono text-text-3">{profile.tokens}</span>
                    <span className="text-right font-mono text-text-3">{formatMoney(profile.cost)}</span>
                  </div>
                ))
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <MetricCard title="MTTR without agent" value={analytics.mttrWithoutAgent} />
        <MetricCard title="Analyzed incident details" value={details.length} />
      </div>
    </section>
  );
}

function buildIncidentAnalytics(incidents: Incident[], details: IncidentDetail[]) {
  const byDayMap = new Map<string, Map<string, number>>();
  incidents.forEach((incident) => {
    const date = safeDateKey(incident.opened_at);
    const classes = byDayMap.get(date) ?? new Map<string, number>();
    classes.set(incident.class, (classes.get(incident.class) ?? 0) + 1);
    byDayMap.set(date, classes);
  });

  const byDay = [...byDayMap.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .slice(-14)
    .map(([date, classes]) => {
      const entries = [...classes.entries()]
        .map(([className, count]) => ({ className, count }))
        .sort((a, b) => b.count - a.count);
      return {
        date,
        total: entries.reduce((sum, entry) => sum + entry.count, 0),
        classes: entries,
      };
    });

  const resolvedDetails = details.filter((detail) => resolutionMs(detail.incident) !== null);
  const autonomous = resolvedDetails.filter(
    (detail) => !actionHasHuman(detail.actions) && actionHasAgent(detail.actions, detail.sessions),
  );
  const withAgent = resolvedDetails.filter((detail) => actionHasAgent(detail.actions, detail.sessions));
  const withoutAgent = resolvedDetails.filter((detail) => !actionHasAgent(detail.actions, detail.sessions));
  const profileMap = new Map<string, { profile: string; tokens: number; cost: number }>();
  details.forEach((detail) => {
    detail.sessions.forEach((session) => {
      const profile = sessionProfileLabel(session);
      const current = profileMap.get(profile) ?? { profile, tokens: 0, cost: 0 };
      current.tokens += session.tokens_used ?? 0;
      current.cost += costFromSession(session);
      profileMap.set(profile, current);
    });
  });

  return {
    total: incidents.length,
    autonomousRate:
      resolvedDetails.length > 0 ? Math.round((autonomous.length / resolvedDetails.length) * 100) : 0,
    pagesAvoided: autonomous.length,
    mttrWithAgent: formatMeanResolution(withAgent.map((detail) => detail.incident)),
    mttrWithoutAgent: formatMeanResolution(withoutAgent.map((detail) => detail.incident)),
    byDay,
    topRecurring: [...incidents]
      .filter((incident) => incident.occurrence_count > 1)
      .sort((a, b) => b.occurrence_count - a.occurrence_count)
      .slice(0, 5),
    profileCosts: [...profileMap.values()].sort((a, b) => b.cost - a.cost || b.tokens - a.tokens),
  };
}

function formatMeanResolution(incidents: Incident[]): string {
  const durations = incidents
    .map((incident) => resolutionMs(incident))
    .filter((value): value is number => value !== null);
  if (durations.length === 0) return "n/a";
  return formatDurationMs(durations.reduce((sum, value) => sum + value, 0) / durations.length);
}

function safeDateKey(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "unknown";
  return date.toISOString().slice(0, 10);
}

function MetricCard({
  icon,
  title,
  value,
}: {
  icon?: ReactNode;
  title: string;
  value: string | number;
}) {
  return (
    <Card className="border-graphite/40 bg-midnight/40">
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-1">
        <CardTitle className="text-[10px] font-bold uppercase tracking-widest text-text-3">
          {title}
        </CardTitle>
        {icon ? <span className="text-cyan-glow">{icon}</span> : null}
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-bold tracking-tight text-text-1">{value}</div>
      </CardContent>
    </Card>
  );
}
