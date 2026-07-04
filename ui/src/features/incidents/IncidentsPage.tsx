import { useEffect, useMemo } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Inbox, ListFilter, RefreshCw, ShieldAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge } from "@/components/ui/status-badge";
import { api, type Incident, type IncidentListParams } from "@/lib/api";
import { events } from "@/lib/events";
import { cn, shortId } from "@/lib/utils";
import {
  buildJobAliasMap,
  formatIncidentClass,
  incidentAge,
  incidentSummary,
  isAwaitingApproval,
  isResolvedIncident,
  jobLabel,
  INCIDENT_EVENT_TYPES,
} from "./incident-utils";

type IncidentSearch = {
  status?: string;
  class?: string;
  job_id?: string;
  needs_approval?: boolean;
};

const STATUS_OPTIONS = [
  "open",
  "triaging",
  "awaiting_approval",
  "remediated",
  "escalated",
  "closed",
  "suppressed",
  "abandoned",
];

const CLASS_OPTIONS = [
  "transient_infra",
  "schema_violation",
  "sla_risk",
  "data_unavailable",
  "auth_failure",
  "oom",
  "quota",
  "unknown",
];

const pageLimit = 50;
const fieldClassName =
  "h-9 rounded-md border border-border bg-background/70 px-3 text-sm text-text-1 outline-none transition focus:border-cyan-glow";

export function IncidentsPage() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as IncidentSearch;

  const { data: features, isLoading: isLoadingFeatures } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });

  const incidentsEnabled = features?.agent_remediation_enabled === true;
  const params = useMemo<IncidentListParams>(
    () => ({
      status: search.status,
      class: search.class,
      job_id: search.job_id,
      needs_approval: search.needs_approval,
      limit: pageLimit,
    }),
    [search.class, search.job_id, search.needs_approval, search.status],
  );

  const incidentsQuery = useQuery({
    queryKey: ["incidents", params],
    queryFn: () => api.getIncidents(params),
    enabled: incidentsEnabled,
    refetchInterval: 15_000,
  });

  const pendingQuery = useQuery({
    queryKey: ["incidents", "pending-approvals"],
    queryFn: () => api.getIncidents({ needs_approval: true, limit: 20 }),
    enabled: incidentsEnabled,
    refetchInterval: 15_000,
  });

  const jobsQuery = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    enabled: incidentsEnabled,
    staleTime: 30_000,
  });

  useEffect(() => {
    if (!incidentsEnabled) return;
    const onIncidentEvent = () => {
      queryClient.invalidateQueries({ queryKey: ["incidents"] });
    };
    INCIDENT_EVENT_TYPES.forEach((type) => events.subscribe(type, onIncidentEvent));
    return () => {
      INCIDENT_EVENT_TYPES.forEach((type) => events.unsubscribe(type, onIncidentEvent));
    };
  }, [incidentsEnabled, queryClient]);

  const jobAliases = useMemo(() => buildJobAliasMap(jobsQuery.data), [jobsQuery.data]);
  const incidents = incidentsQuery.data?.incidents ?? [];
  const activeCount = incidents.filter((incident) => !isResolvedIncident(incident)).length;
  const approvalCount = pendingQuery.data?.total ?? 0;

  function updateFilters(patch: Partial<IncidentSearch>) {
    void navigate({
      to: "/incidents",
      search: cleanSearch({ ...search, ...patch }),
    });
  }

  if (isLoadingFeatures) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-80 w-full" />
      </div>
    );
  }

  if (!incidentsEnabled) {
    return <IncidentsUnavailable />;
  }

  return (
    <div className="space-y-6" data-testid="incidents-page">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <p className="mb-1 text-xs font-medium uppercase tracking-widest text-text-3">
            Agent remediation
          </p>
          <h1 className="text-2xl font-bold tracking-tight">Incidents</h1>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-text-3">
            <Badge variant="outline">{incidentsQuery.data?.total ?? 0} visible</Badge>
            <Badge variant="outline">{activeCount} active on page</Badge>
            <Badge variant={approvalCount > 0 ? "destructive" : "outline"}>
              {approvalCount} pending approvals
            </Badge>
          </div>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => queryClient.invalidateQueries({ queryKey: ["incidents"] })}
        >
          <RefreshCw className="h-3.5 w-3.5" />
          Refresh
        </Button>
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="space-y-4">
          <Card className="border-graphite/40 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="flex items-center gap-2 text-sm">
                <ListFilter className="h-4 w-4 text-cyan-glow" />
                Feed filters
              </CardTitle>
            </CardHeader>
            <CardContent className="grid gap-3 p-4 md:grid-cols-4">
              <label className="space-y-1.5">
                <span className="text-[10px] font-semibold uppercase tracking-wide text-text-3">
                  Status
                </span>
                <select
                  value={search.status ?? ""}
                  onChange={(event) => updateFilters({ status: event.target.value || undefined })}
                  className={fieldClassName}
                  data-testid="incident-status-filter"
                >
                  <option value="">Any status</option>
                  {STATUS_OPTIONS.map((status) => (
                    <option key={status} value={status}>
                      {formatIncidentClass(status)}
                    </option>
                  ))}
                </select>
              </label>
              <label className="space-y-1.5">
                <span className="text-[10px] font-semibold uppercase tracking-wide text-text-3">
                  Class
                </span>
                <select
                  value={search.class ?? ""}
                  onChange={(event) => updateFilters({ class: event.target.value || undefined })}
                  className={fieldClassName}
                  data-testid="incident-class-filter"
                >
                  <option value="">Any class</option>
                  {CLASS_OPTIONS.map((className) => (
                    <option key={className} value={className}>
                      {formatIncidentClass(className)}
                    </option>
                  ))}
                </select>
              </label>
              <label className="space-y-1.5">
                <span className="text-[10px] font-semibold uppercase tracking-wide text-text-3">
                  Job
                </span>
                <select
                  value={search.job_id ?? ""}
                  onChange={(event) => updateFilters({ job_id: event.target.value || undefined })}
                  className={fieldClassName}
                  data-testid="incident-job-filter"
                >
                  <option value="">Any job</option>
                  {(jobsQuery.data ?? []).map((job) => (
                    <option key={job.id} value={job.id}>
                      {job.alias || shortId(job.id)}
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex items-end gap-2 pb-2 text-sm text-text-2">
                <input
                  type="checkbox"
                  checked={search.needs_approval === true}
                  onChange={(event) =>
                    updateFilters({ needs_approval: event.target.checked ? true : undefined })
                  }
                  className="h-4 w-4 rounded border-border bg-background"
                  data-testid="incident-needs-approval-filter"
                />
                Needs approval
              </label>
            </CardContent>
          </Card>

          <Card className="overflow-hidden border-graphite/40 bg-midnight/30">
            <CardHeader className="border-b border-border/50 pb-3">
              <CardTitle className="flex items-center gap-2 text-sm">
                <ShieldAlert className="h-4 w-4 text-warning" />
                Incident feed
              </CardTitle>
            </CardHeader>
            <CardContent className="p-0">
              {incidentsQuery.isLoading ? (
                <div className="space-y-2 p-4">
                  {Array.from({ length: 4 }).map((_, index) => (
                    <Skeleton key={index} className="h-24 w-full" />
                  ))}
                </div>
              ) : incidentsQuery.error ? (
                <EmptyState
                  title="Incidents unavailable"
                  subtitle={incidentsQuery.error.message}
                  icon={<AlertTriangle className="h-12 w-12 text-danger" />}
                />
              ) : incidents.length === 0 ? (
                <EmptyState
                  title="No incidents match"
                  subtitle="Adjust the filters or wait for a failing run to open an incident."
                  icon={<Inbox className="h-12 w-12 text-text-3" />}
                />
              ) : (
                <div className="divide-y divide-border/50">
                  {incidents.map((incident) => (
                    <IncidentFeedRow
                      key={incident.id}
                      incident={incident}
                      jobAlias={jobLabel(incident, jobAliases)}
                    />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </div>

        <PendingApprovalsList
          incidents={pendingQuery.data?.incidents ?? []}
          jobAliases={jobAliases}
          isLoading={pendingQuery.isLoading}
        />
      </div>
    </div>
  );
}

function IncidentFeedRow({ incident, jobAlias }: { incident: Incident; jobAlias: string }) {
  return (
    <Link
      to="/incidents/$incidentId"
      params={{ incidentId: incident.id }}
      data-testid="incident-row"
      className={cn(
        "block px-4 py-3 transition hover:bg-graphite/10",
        isAwaitingApproval(incident) && "bg-gold/5",
      )}
    >
      <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium text-text-1">{jobAlias}</span>
            <StatusBadge status={incident.status} size="sm" />
            <Badge variant="outline" className="text-[10px]">
              {formatIncidentClass(incident.class)}
            </Badge>
            {isAwaitingApproval(incident) ? (
              <Badge variant="outline" className="border-gold/30 bg-gold/10 text-[10px] text-gold">
                approval
              </Badge>
            ) : null}
          </div>
          <div className="mt-1 line-clamp-2 text-sm text-text-2">{incidentSummary(incident)}</div>
          <div className="mt-2 flex flex-wrap items-center gap-2 font-mono text-[10px] text-text-4">
            <span>#{shortId(incident.id)}</span>
            <span>run {shortId(incident.run_id)}</span>
            <span>task {incident.task_name || shortId(incident.task_id)}</span>
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2 text-xs text-text-3">
          <Badge variant="outline" className="font-mono text-[10px]">
            {incidentAge(incident)}
          </Badge>
          {incident.occurrence_count > 1 ? (
            <Badge variant="outline" className="font-mono text-[10px]">
              {incident.occurrence_count}x
            </Badge>
          ) : null}
        </div>
      </div>
    </Link>
  );
}

function PendingApprovalsList({
  incidents,
  jobAliases,
  isLoading,
}: {
  incidents: Incident[];
  jobAliases: Record<string, string>;
  isLoading: boolean;
}) {
  return (
    <Card className="border-gold/30 bg-gold/5" data-testid="pending-approvals-list">
      <CardHeader className="border-b border-gold/20 pb-3">
        <CardTitle className="flex items-center gap-2 text-sm">
          <Inbox className="h-4 w-4 text-gold" />
          Pending approvals
        </CardTitle>
      </CardHeader>
      <CardContent className="p-0">
        {isLoading ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : incidents.length === 0 ? (
          <div className="px-4 py-6 text-sm text-text-3">No pending approvals.</div>
        ) : (
          <div className="divide-y divide-gold/15">
            {incidents.map((incident) => (
              <Link
                key={incident.id}
                to="/incidents/$incidentId"
                params={{ incidentId: incident.id }}
                className="block px-4 py-3 transition hover:bg-gold/10"
                data-testid="pending-approval-row"
              >
                <div className="flex items-center justify-between gap-3">
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium text-text-1">
                      {jobLabel(incident, jobAliases)}
                    </div>
                    <div className="mt-1 truncate text-xs text-text-3">
                      {incident.task_name || formatIncidentClass(incident.class)}
                    </div>
                  </div>
                  <Badge variant="outline" className="shrink-0 font-mono text-[10px]">
                    {incidentAge(incident)}
                  </Badge>
                </div>
              </Link>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function IncidentsUnavailable() {
  return (
    <EmptyState
      title="Incidents disabled"
      subtitle="Agent remediation is not enabled on this Caesium server."
      icon={<ShieldAlert className="h-12 w-12 text-text-3" />}
      className="py-16"
    />
  );
}

function cleanSearch(search: IncidentSearch): IncidentSearch {
  return {
    status: search.status || undefined,
    class: search.class || undefined,
    job_id: search.job_id || undefined,
    needs_approval: search.needs_approval === true ? true : undefined,
  };
}
