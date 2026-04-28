import { Link } from "@tanstack/react-router";
import {
  BarChart,
  Database,
  FileCode2,
  LayoutDashboard,
  Radio,
  Server,
  type LucideIcon,
} from "lucide-react";
import { AtomLogo } from "@/components/brand/atom-logo";
import { useNavCounts } from "@/features/jobs/useNavCounts";
import { useClusterHealth, type ClusterHealthState } from "@/features/system/useClusterHealth";
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

export function Sidebar() {
  const counts = useNavCounts();
  const health = useClusterHealth();
  const stateMeta = STATE_META[health.state];

  const navItems: NavItem[] = [
    { to: "/jobs", label: "Jobs", icon: LayoutDashboard, count: counts.jobs },
    { to: "/triggers", label: "Triggers", icon: Radio, count: counts.triggers },
    { to: "/atoms", label: "Atoms", icon: Database, count: counts.atoms },
    { to: "/stats", label: "Stats", icon: BarChart, count: null },
    { to: "/system", label: "System", icon: Server, count: null },
    { to: "/jobdefs", label: "JobDefs", icon: FileCode2, count: null },
  ];

  return (
    <aside className="relative flex w-64 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground shadow-2xl shadow-sidebar/30">
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

      <nav className="flex-1 space-y-1.5 p-3">
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
        uptimeSeconds={health.uptimeSeconds}
        stateMeta={stateMeta}
      />
    </aside>
  );
}

interface ClusterFooterProps {
  state: ClusterHealthState;
  uptimeSeconds: number | null;
  stateMeta: (typeof STATE_META)[ClusterHealthState];
}

function ClusterFooter({ state, uptimeSeconds, stateMeta }: ClusterFooterProps) {
  if (state === "unknown") {
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
