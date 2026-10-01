import { Outlet, useNavigate, useRouter, useRouterState } from "@tanstack/react-router";
import { Sidebar } from "./Sidebar";
import { Header } from "./Header";
import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { useClusterHealth } from "@/features/system/useClusterHealth";
import { clusterAtomProps } from "@/features/system/cluster-atom";
import { CommandMenu } from "@/components/command-menu";
import { events } from "@/lib/events";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

export function AppShell() {
  const navigate = useNavigate();
  const router = useRouter();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const segments = pathname.split("/").filter(Boolean);
  const jobId = segments[0] === "jobs" ? segments[1] : undefined;
  const runId = segments[2] === "runs" ? segments[3] : undefined;
  const { data: job } = useQuery({ queryKey: ["job", jobId], queryFn: () => api.getJob(jobId!), enabled: !!jobId });
  const { data: run } = useQuery({ queryKey: ["job", jobId, "runs", runId], queryFn: () => api.getJobRun(jobId!, runId!), enabled: !!jobId && !!runId });
  const health = useClusterHealth();

  const [navigationOpen, setNavigationOpen] = useState(false);
  const navigationButtonRef = useRef<HTMLButtonElement>(null);



  useEffect(() => {
    // Initialize global SSE connection
    events.connect();
    return () => events.disconnect();
  }, []);

  useEffect(() => {
    return router.subscribe("onBeforeNavigate", () => setNavigationOpen(false));
  }, [router]);

  useEffect(() => {
    const mediaQuery = window.matchMedia("(min-width: 1024px)");
    const closeDrawerAtDesktop = () => {
      if (mediaQuery.matches) setNavigationOpen(false);
    };
    mediaQuery.addEventListener("change", closeDrawerAtDesktop);
    return () => mediaQuery.removeEventListener("change", closeDrawerAtDesktop);
  }, []);

  useEffect(() => {
    let pending = false;
    let timer: ReturnType<typeof setTimeout>;
    const destinations = { j: "/jobs", t: "/triggers", a: "/atoms", s: "/stats", y: "/system", d: "/jobdefs", l: "/system/logs" } as const;
    const handleKeyDown = (e: KeyboardEvent) => {
      // Don't trigger if user is typing in an input
      if (
        e.target instanceof HTMLInputElement ||
        e.target instanceof HTMLTextAreaElement ||
        (e.target as HTMLElement).closest('select, [contenteditable="true"], [role="dialog"]:not([data-navigation-drawer]), [role="menu"]') ||
        e.ctrlKey || e.metaKey || e.altKey || e.repeat
      ) {
        pending = false;
        clearTimeout(timer);
        return;
      }

      if (pending) {
        pending = false;
        clearTimeout(timer);
        // Consume the chord before page-local single-key handlers see it.
        e.preventDefault();
        e.stopPropagation();
        const to = destinations[e.key as keyof typeof destinations];
        if (to) void navigate({ to });
        return;
      }
      if (e.key === "g") {
        pending = true;
        e.preventDefault();
        timer = setTimeout(() => { pending = false; }, 1000);
      }
    };

    window.addEventListener("keydown", handleKeyDown, true);
    return () => { clearTimeout(timer); window.removeEventListener("keydown", handleKeyDown, true); };
  }, [navigate]);

  return (
    <>
      <div className="flex h-dvh w-full overflow-hidden bg-transparent text-foreground">
        <Sidebar className="hidden lg:flex" />
        <Dialog open={navigationOpen} onOpenChange={setNavigationOpen}>
          <DialogContent
            data-navigation-drawer
            aria-describedby={undefined}
            onCloseAutoFocus={(event) => {
              event.preventDefault();
              if (window.matchMedia("(min-width: 1024px)").matches) {
                document.querySelector<HTMLElement>("aside a")?.focus();
              } else {
                navigationButtonRef.current?.focus();
              }
            }}
            className="left-0 top-0 h-dvh max-h-none w-[min(20rem,calc(100vw-2rem))] max-w-none translate-x-0 translate-y-0 gap-0 overflow-hidden rounded-none border-y-0 border-l-0 p-0 data-[state=closed]:slide-out-to-left data-[state=closed]:slide-out-to-top-0 data-[state=open]:slide-in-from-left data-[state=open]:slide-in-from-top-0 lg:hidden"
          >
            <DialogTitle className="sr-only">Navigation</DialogTitle>
            <Sidebar className="h-full w-full border-0" onNavigate={() => setNavigationOpen(false)} />
          </DialogContent>
        </Dialog>
        <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
          <Header
            atomProps={clusterAtomProps(health)}
            jobAlias={job?.alias}
            runStartedAt={run?.started_at}
            navigationButtonRef={navigationButtonRef}
            onOpenNavigation={() => setNavigationOpen(true)}
          />
          <main className="min-w-0 flex-1 overflow-auto p-4 lg:p-6">
            <Outlet />
          </main>
          <CommandMenu />
        </div>
      </div>
    </>
  );
}
