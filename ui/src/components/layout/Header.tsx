import { Link, useRouterState } from "@tanstack/react-router";
import { LogOut, Menu } from "lucide-react";
import { type Ref, useState } from "react";
import { ModeToggle } from "../mode-toggle";
import { AtomLogo, type AtomLogoProps } from "../brand/atom-logo";
import { Oscillator } from "../ui/oscillator";
import { Button } from "@/components/ui/button";
import { UTCClock } from "@/components/ui/utc-clock";
import { logout } from "@/lib/auth";
import { formatUTCTime } from "@/lib/utils";
import { NotificationsPopover } from "./NotificationsPopover";

interface Crumb {
  label: string;
  to?: string;
}

function Breadcrumb({ jobAlias, runStartedAt }: { jobAlias?: string; runStartedAt?: string }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const segments = pathname.split("/").filter(Boolean);
  const crumbs: Crumb[] = [{ label: "~", to: "/jobs" }];
  let acc = "";
  segments.forEach((segment, i) => {
    acc += `/${segment}`;
    if (segments[2] === "runs" && segments[3] && i === 2) return;
    if (segments[0] === "jobs" && i === 1) segment = jobAlias ?? "pipeline";
    if (segments[2] === "runs" && i === 3) segment = `run ${formatUTCTime(runStartedAt, { seconds: false, fallback: "unknown" })}`;
    crumbs.push({ label: segment, to: i === segments.length - 1 ? undefined : acc });
  });
  return <nav aria-label="Breadcrumb" className="hidden min-w-0 items-center text-xs text-text-3 lg:flex">
    {crumbs.map((crumb, i) => <span key={i} className="flex min-w-0 items-center">
      {i > 0 ? <span aria-hidden="true">/</span> : null}
      {crumb.to ? <Link to={crumb.to} className="truncate hover:text-text-1">{crumb.label}</Link> : <span aria-current="page" className="truncate text-text-1">{crumb.label}</span>}
    </span>)}
  </nav>;
}

interface HeaderProps {
  atomProps?: AtomLogoProps;
  jobAlias?: string;
  runStartedAt?: string;
  onOpenNavigation?: () => void;
  navigationButtonRef?: Ref<HTMLButtonElement>;
}

export function Header({ onOpenNavigation, navigationButtonRef, atomProps = { voters: [], quorum: "unknown" }, jobAlias, runStartedAt }: HeaderProps) {
  const [isSigningOut, setIsSigningOut] = useState(false);

  const handleSignOut = async () => {
    if (isSigningOut) {
      return;
    }

    setIsSigningOut(true);
    try {
      await logout();
    } finally {
      setIsSigningOut(false);
    }
  };

  return (
    <header className="sticky top-0 z-30 flex h-11 shrink-0 items-center justify-between gap-3 border-b border-border bg-void px-4 lg:px-6">
      <div className="flex min-w-0 items-center gap-2 sm:gap-4">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Open navigation"
          className="text-text-2 hover:text-text-1 lg:hidden"
          onClick={onOpenNavigation}
          ref={navigationButtonRef}
        >
          <Menu className="h-4 w-4" />
        </Button>
        <div className="flex items-center gap-2">
          <AtomLogo size={22} {...atomProps} />
          <span className="hidden text-[13px] font-bold tracking-[.12em] sm:inline">CAESIUM</span>
        </div>
        <Breadcrumb jobAlias={jobAlias} runStartedAt={runStartedAt} />
      </div>
      <div className="flex items-center gap-2">
        <Oscillator className="hidden xl:block" />
        <UTCClock className="hidden md:flex" />
        <NotificationsPopover />
        <Button
          variant="ghost"
          size="icon"
          aria-label="Sign out"
          aria-busy={isSigningOut}
          className="text-text-2 hover:text-text-1"
          disabled={isSigningOut}
          onClick={() => void handleSignOut()}
        >
          <LogOut className="h-4 w-4" />
        </Button>
        <ModeToggle />
      </div>
    </header>
  );
}
