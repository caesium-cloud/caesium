import { Link, useRouterState } from "@tanstack/react-router";
import { Bell, ChevronRight } from "lucide-react";
import { ModeToggle } from "../mode-toggle";
import { CommandMenu } from "../command-menu";
import { Button } from "@/components/ui/button";
import { UTCClock } from "@/components/ui/utc-clock";

interface Crumb {
  label: string;
  to?: string;
}

const ROOT_CRUMB: Crumb = { label: "Caesium", to: "/jobs" };

const ROUTE_LABELS: Record<string, string> = {
  jobs: "Jobs",
  triggers: "Triggers",
  atoms: "Atoms",
  stats: "Stats",
  system: "System",
  jobdefs: "JobDefs",
  database: "Database",
  logs: "Logs",
  runs: "Runs",
};

function buildCrumbs(pathname: string): Crumb[] {
  const segments = pathname.split("/").filter(Boolean);
  if (segments.length === 0) return [ROOT_CRUMB];
  const crumbs: Crumb[] = [ROOT_CRUMB];
  let acc = "";
  segments.forEach((segment, idx) => {
    acc += `/${segment}`;
    const isLast = idx === segments.length - 1;
    const known = ROUTE_LABELS[segment.toLowerCase()];
    const label = known ?? (segment.length > 12 ? `${segment.slice(0, 8)}…` : segment);
    crumbs.push({ label, to: isLast ? undefined : acc });
  });
  return crumbs;
}

function Breadcrumb() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const crumbs = buildCrumbs(pathname);
  return (
    <nav aria-label="Breadcrumb" className="hidden items-center gap-1.5 text-xs lg:flex">
      {crumbs.map((crumb, idx) => {
        const isLast = idx === crumbs.length - 1;
        return (
          <span key={`${crumb.label}-${idx}`} className="flex items-center gap-1.5">
            {idx > 0 ? (
              <ChevronRight aria-hidden="true" className="h-3 w-3 text-text-4" />
            ) : null}
            {crumb.to && !isLast ? (
              <Link
                to={crumb.to}
                className="font-medium uppercase tracking-[0.18em] text-text-3 hover:text-text-1"
              >
                {crumb.label}
              </Link>
            ) : (
              <span
                aria-current={isLast ? "page" : undefined}
                className="font-medium uppercase tracking-[0.18em] text-text-1"
              >
                {crumb.label}
              </span>
            )}
          </span>
        );
      })}
    </nav>
  );
}

export function Header() {
  return (
    <header className="sticky top-0 z-30 flex h-14 items-center justify-between border-b border-border/70 bg-background/70 px-6 backdrop-blur supports-[backdrop-filter]:bg-background/60">
      <div className="flex items-center gap-4">
        <div className="hidden items-center gap-2 lg:flex">
          <span className="h-2 w-2 rounded-full bg-gold animate-gold-pulse shadow-[0_0_18px_hsl(var(--gold)/0.45)]" />
          <span className="text-[0.62rem] font-medium uppercase tracking-[0.34em] text-text-3">
            Operator Console
          </span>
        </div>
        <Breadcrumb />
      </div>
      <div className="flex items-center gap-2">
        <CommandMenu />
        <UTCClock className="hidden md:flex" />
        <Button
          variant="ghost"
          size="icon"
          aria-label="Notifications"
          className="text-text-2 hover:text-text-1"
        >
          <Bell className="h-4 w-4" />
        </Button>
        <ModeToggle />
      </div>
    </header>
  );
}
