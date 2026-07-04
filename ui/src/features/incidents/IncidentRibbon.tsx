import { Link } from "@tanstack/react-router";
import { AlertTriangle, Bot, ExternalLink } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StatusBadge } from "@/components/ui/status-badge";
import type { Incident } from "@/lib/api";
import { cn, shortId } from "@/lib/utils";
import {
  formatIncidentClass,
  incidentAge,
  incidentSummary,
  isAwaitingApproval,
  isResolvedIncident,
} from "./incident-utils";

interface IncidentRibbonProps {
  incidents: Incident[];
  label?: string;
  className?: string;
  testId?: string;
}

export function IncidentRibbon({
  incidents,
  label = "Incident",
  className,
  testId = "incident-ribbon",
}: IncidentRibbonProps) {
  if (incidents.length === 0) {
    return null;
  }

  const primary = [...incidents].sort(sortActiveFirst)[0];
  const activeCount = incidents.filter((incident) => !isResolvedIncident(incident)).length;

  return (
    <div
      data-testid={testId}
      className={cn(
        "rounded-md border border-warning/30 bg-warning/10 px-4 py-3 text-sm",
        className,
      )}
    >
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <AlertTriangle className="h-4 w-4 text-warning" />
            <span className="font-semibold text-text-1">{label}</span>
            <StatusBadge status={primary.status} size="sm" />
            <Badge variant="outline" className="border-warning/30 bg-warning/10 text-[10px] text-warning">
              {formatIncidentClass(primary.class)}
            </Badge>
            {isAwaitingApproval(primary) ? (
              <Badge variant="outline" className="border-gold/30 bg-gold/10 text-[10px] text-gold">
                approval
              </Badge>
            ) : null}
            {activeCount > 1 ? (
              <Badge variant="outline" className="text-[10px]">
                {activeCount} active
              </Badge>
            ) : null}
          </div>
          <div className="line-clamp-2 text-xs text-text-2">{incidentSummary(primary)}</div>
          <div className="flex flex-wrap items-center gap-2 font-mono text-[10px] text-text-4">
            <span>#{shortId(primary.id)}</span>
            <span>{incidentAge(primary)}</span>
            {primary.task_name ? <span>{primary.task_name}</span> : null}
            {primary.remediation_target_run_id === primary.run_id ? null : primary.remediation_target_run_id ? (
              <span className="inline-flex items-center gap-1 text-cyan-glow">
                <Bot className="h-3 w-3" />
                retry by incident
              </span>
            ) : null}
          </div>
        </div>
        <Button asChild variant="outline" size="sm" className="w-fit shrink-0">
          <Link to="/incidents/$incidentId" params={{ incidentId: primary.id }}>
            Timeline
            <ExternalLink className="h-3.5 w-3.5" />
          </Link>
        </Button>
      </div>
    </div>
  );
}

function sortActiveFirst(a: Incident, b: Incident): number {
  const activeDelta = Number(isResolvedIncident(a)) - Number(isResolvedIncident(b));
  if (activeDelta !== 0) return activeDelta;
  return new Date(b.opened_at).getTime() - new Date(a.opened_at).getTime();
}
