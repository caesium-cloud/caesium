import { useEffect } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { clusterAtomProps } from "@/features/system/cluster-atom";
import type { AtomLogoProps } from "@/components/brand/atom-logo";
import { StatusBadge } from "@/components/ui/status-badge";
import { AtomLogo } from "@/components/brand/atom-logo";
import { INCIDENT_EVENT_TYPES, isAwaitingApproval } from "@/features/incidents/incident-utils";
import { useNavCounts } from "@/features/jobs/useNavCounts";
import { deriveQuorumView, type QuorumView } from "@/features/system/quorum";
import { useClusterHealth, type ClusterHealthState } from "@/features/system/useClusterHealth";
import { api } from "@/lib/api";
import { events } from "@/lib/events";
import { cn } from "@/lib/utils";

interface NavItem {
  to: string;
  label: string;
  hint?: string;
  count: number | null;
}

const STATE_META: Record<
  ClusterHealthState,
  { label: string; dot: string; copy: string }
> = {
  operational: {
    label: "Operational",
    dot: "bg-success",
    copy: "All systems nominal",
  },
  degraded: {
    label: "Degraded",
    dot: "bg-warning animate-gold-pulse",
    copy: "Investigating one or more checks",
  },
  unavailable: {
    label: "Unavailable",
    dot: "bg-danger",
    copy: "Quorum lost. Cluster cannot serve writes",
  },
  incident: {
    label: "Incident",
    dot: "bg-danger",
    copy: "One or more checks failing",
  },
  unknown: {
    label: "Unknown",
    dot: "bg-text-4",
    copy: "Health unavailable",
  },
};

function formatUptime(seconds: number | null): string {
  if (seconds == null) return "—";
  if (seconds < 60) return `${Math.floor(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86_400)}d`;
}

function CountBadge({ value }: { value: number | null }) {
  return value == null ? null : <span className="ml-auto text-xs text-text-3">{value}</span>;
}

interface SidebarProps {
  className?: string;
  onNavigate?: () => void;
}

export function Sidebar({ className, onNavigate }: SidebarProps) {
  const queryClient = useQueryClient();
  const counts = useNavCounts();
  const health = useClusterHealth();
  const quorum = deriveQuorumView(health.raw?.checks?.cluster, { stale: health.stale });
  // The cluster's own liveness is the most specific thing the footer can say.
  // "All systems nominal" beside a dead replica is what issue #494 reported.
  const stateMeta =
    health.stale
      ? { ...STATE_META.unknown, copy: "Health data is stale" }
      : quorum.status === "available" || quorum.status === "unreported"
      ? STATE_META[health.state]
      : { ...STATE_META[health.state], copy: quorum.detail };
  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });
  const incidentsEnabled = features?.agent_remediation_enabled === true;
  const freshnessEnabled = features?.freshness_enabled === true;
  const contractEnforcementEnabled = features?.contract_enforcement_enabled === true;

  const { data: incidentNav } = useQuery({
    queryKey: ["incidents", "nav"],
    queryFn: () => api.getIncidents({ limit: 200 }),
    enabled: incidentsEnabled,
    placeholderData: (previous) => previous,
    refetchInterval: 30_000,
  });

  useEffect(() => {
    if (!incidentsEnabled) return;
    const onIncidentEvent = () => {
      queryClient.invalidateQueries({ queryKey: ["incidents", "nav"] });
      queryClient.invalidateQueries({ queryKey: ["incidents", "summary"] });
    };
    INCIDENT_EVENT_TYPES.forEach((type) => events.subscribe(type, onIncidentEvent));
    return () => {
      INCIDENT_EVENT_TYPES.forEach((type) => events.unsubscribe(type, onIncidentEvent));
    };
  }, [incidentsEnabled, queryClient]);

  const activeIncidentCount =
    incidentNav?.incidents.filter(
      (incident) =>
        incident.status === "open" ||
        incident.status === "triaging" ||
        isAwaitingApproval(incident),
    ).length ?? null;

  const navItems: NavItem[] = [
    { to: "/jobs", label: "Jobs", hint: "j", count: counts.jobs },
    { to: "/triggers", label: "Triggers", hint: "t", count: counts.triggers },
    { to: "/atoms", label: "Atoms", hint: "a", count: counts.atoms },
    ...(freshnessEnabled
      ? [{ to: "/datasets", label: "Datasets", count: null }]
      : []),
    ...(features?.data_assertions_enabled === true
      ? [{ to: "/datasets/holds", label: "Holds", count: counts.holds }]
      : []),
    ...(contractEnforcementEnabled
      ? [{ to: "/contracts", label: "Contracts", count: null }]
      : []),
    ...(incidentsEnabled
      ? [{ to: "/incidents", label: "Incidents", count: activeIncidentCount }]
      : []),
    { to: "/stats", label: "Stats", hint: "s", count: null },
    { to: "/system", label: "System", hint: "y", count: null },
    { to: "/jobdefs", label: "JobDefs", hint: "d", count: null },
  ];

  return (
    <aside className={cn("relative flex w-[208px] shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground", className)}>
      <nav className="min-h-0 flex-1 space-y-0.5 overflow-y-auto px-3 py-4">
        {navItems.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            activeProps={{
              className:
                "bg-sidebar-accent text-sidebar-foreground font-bold [&_.nav-caret]:visible",
            }}
            inactiveProps={{
              className: "text-sidebar-muted hover:bg-sidebar-accent/50 hover:text-sidebar-foreground",
            }}
            onClick={onNavigate}
            className="flex h-[30px] items-center gap-2 rounded-md px-2 text-[13px] lowercase transition-colors"
          >
            <span aria-hidden="true" className="nav-caret invisible text-cyan">❯</span>
            <span>{item.label}</span>
            <CountBadge value={item.count} />
            {item.hint ? <span aria-hidden="true" className="ml-auto w-6 text-right text-[11px] font-normal text-text-3">g{item.hint}</span> : null}
          </Link>
        ))}
      </nav>

      <ClusterFooter
        state={health.state}
        reported={health.raw !== null}
        uptimeSeconds={health.uptimeSeconds}
        stateMeta={stateMeta}
        quorum={quorum}
        atomProps={clusterAtomProps(health)}
        leader={health.raw?.checks?.cluster?.quorum?.leader_address}
        silentNode={health.raw?.checks?.cluster?.members.find((member) => member.reachability === "unreachable")?.address}
      />
    </aside>
  );
}

interface ClusterFooterProps {
  atomProps: AtomLogoProps;
  leader?: string;
  silentNode?: string;
  state: ClusterHealthState;
  /** A health response arrived, whatever it said. */
  reported: boolean;
  uptimeSeconds: number | null;
  stateMeta: (typeof STATE_META)[ClusterHealthState];
  quorum: QuorumView;
}

function ClusterFooter({ state, reported, uptimeSeconds, stateMeta, quorum, atomProps, leader, silentNode }: ClusterFooterProps) {
  // Hide the footer only when no health response arrived at all. The server
  // also reports an overall status of "unknown": meaning it is answering but
  // could not determine cluster liveness: and that must stay visible rather
  // than silently removing the cluster panel.
  if (state === "unknown" && !reported) {
    return null;
  }
  return (
    <div role="region" aria-label="Cluster health" className="border-t border-sidebar-border px-4 py-3">
      <AtomLogo size={112} {...atomProps} className="mx-auto mb-2" />
      <dl className="mt-2 space-y-1.5 text-xs">
        <div className="flex items-center justify-between">
          <dt className="text-text-3">Status</dt>
          <dd className="flex items-center gap-1.5 text-text-1">
            <StatusBadge status={state === "operational" ? "succeeded" : state === "degraded" ? "paused" : state === "unknown" ? "unknown" : "failed"} label={stateMeta.label} size="sm" />
          </dd>
        </div>
        {quorum.status !== "unreported" && (
          <div className="flex items-center justify-between">
            <dt className="text-text-3">Quorum</dt>
            <dd
              className="flex items-center gap-1.5 tabular-nums text-text-2"
              data-testid="sidebar-quorum"
            >

              {quorum.label}
            </dd>
          </div>
        )}
        <div className="flex items-center justify-between gap-2">
          <dt className="text-text-3">leader</dt>
          <dd className="truncate text-text-2" title={leader}>{leader || "unknown"}</dd>
        </div>
        {quorum.total > 9 ? <div className="flex justify-between"><dt className="text-text-3">voters</dt><dd>{quorum.total}</dd></div> : null}
        {silentNode ? <div className="text-xs text-gold" title={silentNode}>silent node {silentNode}</div> : null}
        <div className="flex items-center justify-between">
          <dt className="text-text-3">Uptime</dt>
          <dd className="tabular-nums text-text-2">{formatUptime(uptimeSeconds)}</dd>
        </div>
        <div className="flex items-start justify-between gap-3">
          <dt className="text-text-3">Health</dt>
          <dd className="text-right text-[11px] text-text-3">{stateMeta.copy}</dd>
        </div>
      </dl>
      <div className="mt-3 text-xs text-text-3">build unavailable</div>
    </div>
  );
}
