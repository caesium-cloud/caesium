import { useEffect } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  BarChart,
  Database,
  FileCode2,
  GitBranch,
  LayoutDashboard,
  Radio,
  Server,
  ShieldCheck,
  Hand,
  Siren,
  type LucideIcon,
} from "lucide-react";
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
  icon: LucideIcon;
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
    dot: "bg-danger animate-pulse",
    copy: "Quorum lost — cluster cannot serve writes",
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
  if (value == null) return null;
  return (
    <span className="ml-auto inline-flex h-5 min-w-[1.25rem] items-center justify-center rounded-full border border-graphite bg-obsidian/70 px-1.5 font-mono text-[10px] tabular-nums text-text-2">
      {value}
    </span>
  );
}

interface SidebarProps {
  className?: string;
  onNavigate?: () => void;
}

export function Sidebar({ className, onNavigate }: SidebarProps) {
  const queryClient = useQueryClient();
  const counts = useNavCounts();
  const health = useClusterHealth();
  const quorum = deriveQuorumView(health.raw?.checks?.cluster);
  // The cluster's own liveness is the most specific thing the footer can say.
  // "All systems nominal" beside a dead replica is what issue #494 reported.
  const stateMeta =
    quorum.status === "available" || quorum.status === "unreported"
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
    { to: "/jobs", label: "Jobs", icon: LayoutDashboard, count: counts.jobs },
    { to: "/triggers", label: "Triggers", icon: Radio, count: counts.triggers },
    { to: "/atoms", label: "Atoms", icon: Database, count: counts.atoms },
    ...(freshnessEnabled
      ? [{ to: "/datasets", label: "Datasets", icon: GitBranch, count: null }]
      : []),
    ...(features?.data_assertions_enabled === true
      ? [{ to: "/datasets/holds", label: "Holds", icon: Hand, count: counts.holds }]
      : []),
    ...(contractEnforcementEnabled
      ? [{ to: "/contracts", label: "Contracts", icon: ShieldCheck, count: null }]
      : []),
    ...(incidentsEnabled
      ? [{ to: "/incidents", label: "Incidents", icon: Siren, count: activeIncidentCount }]
      : []),
    { to: "/stats", label: "Stats", icon: BarChart, count: null },
    { to: "/system", label: "System", icon: Server, count: null },
    { to: "/jobdefs", label: "JobDefs", icon: FileCode2, count: null },
  ];

  return (
    <aside className={cn("relative flex w-64 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground shadow-2xl shadow-sidebar/30", className)}>
      {/* Gold accent rail on the left edge */}
      <span
        aria-hidden="true"
        className="absolute inset-y-0 left-0 w-[2px] bg-gradient-to-b from-gold/0 via-gold/60 to-gold/0"
      />

      <div className="border-b border-sidebar-border px-5 py-5">
        <div className="flex items-center gap-3">
          <AtomLogo size={40} className="shrink-0 drop-shadow-[0_0_24px_hsl(var(--cyan)/0.35)]" />
          <div className="min-w-0">
            <div className="text-[0.62rem] font-medium uppercase tracking-[0.38em] text-gold/80">
              Control Plane
            </div>
            <div className="truncate text-lg font-semibold uppercase tracking-[0.34em] text-sidebar-foreground">
              Caesium
            </div>
          </div>
        </div>
      </div>

      <nav className="min-h-0 flex-1 space-y-1.5 overflow-y-auto p-3">
        {navItems.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            activeProps={{
              className:
                "bg-sidebar-accent text-sidebar-foreground shadow-[inset_2px_0_0_hsl(var(--gold))]",
            }}
            inactiveProps={{
              className: "text-sidebar-muted hover:bg-sidebar-accent/50 hover:text-sidebar-foreground",
            }}
            onClick={onNavigate}
            className="flex items-center gap-3 rounded-lg px-3 py-2.5 text-sm font-medium transition-colors"
          >
            <item.icon className="h-4 w-4 text-gold" />
            <span>{item.label}</span>
            <CountBadge value={item.count} />
          </Link>
        ))}
      </nav>

      <ClusterFooter
        state={health.state}
        reported={health.raw !== null}
        uptimeSeconds={health.uptimeSeconds}
        stateMeta={stateMeta}
        quorum={quorum}
      />
    </aside>
  );
}

interface ClusterFooterProps {
  state: ClusterHealthState;
  /** A health response arrived, whatever it said. */
  reported: boolean;
  uptimeSeconds: number | null;
  stateMeta: (typeof STATE_META)[ClusterHealthState];
  quorum: QuorumView;
}

const QUORUM_DOT: Record<QuorumView["tone"], string> = {
  ok: "bg-success",
  warn: "bg-warning",
  danger: "bg-danger",
  muted: "bg-text-4",
};

function ClusterFooter({ state, reported, uptimeSeconds, stateMeta, quorum }: ClusterFooterProps) {
  // Hide the footer only when no health response arrived at all. The server
  // also reports an overall status of "unknown" — meaning it is answering but
  // could not determine cluster liveness — and that must stay visible rather
  // than silently removing the cluster panel.
  if (state === "unknown" && !reported) {
    return null;
  }
  return (
    <div className="border-t border-sidebar-border px-4 py-3">
      <div className="text-[10px] font-medium uppercase tracking-[0.32em] text-text-3">
        Cluster
      </div>
      <dl className="mt-2 space-y-1.5 text-xs">
        <div className="flex items-center justify-between">
          <dt className="text-text-3">Status</dt>
          <dd className="flex items-center gap-1.5 text-text-1">
            <span className={cn("inline-block h-2 w-2 rounded-full", stateMeta.dot)} />
            {stateMeta.label}
          </dd>
        </div>
        {quorum.status !== "unreported" && (
          <div className="flex items-center justify-between">
            <dt className="text-text-3">Quorum</dt>
            <dd
              className="flex items-center gap-1.5 font-mono tabular-nums text-text-2"
              data-testid="sidebar-quorum"
            >
              <span className={cn("inline-block h-2 w-2 rounded-full", QUORUM_DOT[quorum.tone])} />
              {quorum.label}
            </dd>
          </div>
        )}
        <div className="flex items-center justify-between">
          <dt className="text-text-3">Uptime</dt>
          <dd className="font-mono tabular-nums text-text-2">{formatUptime(uptimeSeconds)}</dd>
        </div>
        <div className="flex items-start justify-between gap-3">
          <dt className="text-text-3">Health</dt>
          <dd className="text-right text-[11px] text-text-3">{stateMeta.copy}</dd>
        </div>
      </dl>
    </div>
  );
}
