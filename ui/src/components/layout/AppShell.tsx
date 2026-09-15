import { Outlet, useNavigate, useRouter } from "@tanstack/react-router";
import { Sidebar } from "./Sidebar";
import { Header } from "./Header";
import { useEffect, useRef, useState } from "react";
import { events } from "@/lib/events";
import { UTCClockProvider } from "@/components/ui/utc-clock";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

export function AppShell() {
  const navigate = useNavigate();
  const router = useRouter();
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
    const handleKeyDown = (e: KeyboardEvent) => {
      // Don't trigger if user is typing in an input
      if (
        e.target instanceof HTMLInputElement ||
        e.target instanceof HTMLTextAreaElement ||
        (e.target as HTMLElement).isContentEditable
      ) {
        return;
      }

      if (e.key === "g") {
        const nextKeyHandler = (nextEvent: KeyboardEvent) => {
          if (nextEvent.key === "j") navigate({ to: "/jobs" });
          if (nextEvent.key === "t") navigate({ to: "/triggers" });
          if (nextEvent.key === "a") navigate({ to: "/atoms" });
          if (nextEvent.key === "s") navigate({ to: "/stats" });
          if (nextEvent.key === "y") navigate({ to: "/system" });
          if (nextEvent.key === "d") navigate({ to: "/jobdefs" });
          if (nextEvent.key === "l") navigate({ to: "/system/logs" });
          window.removeEventListener("keydown", nextKeyHandler);
        };
        window.addEventListener("keydown", nextKeyHandler, { once: true });
        // Auto-remove listener after a short delay if no second key is pressed
        setTimeout(() => window.removeEventListener("keydown", nextKeyHandler), 1000);
      }
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [navigate]);

  return (
    <UTCClockProvider>
      <div className="flex h-dvh w-full overflow-hidden bg-transparent text-foreground">
        <Sidebar className="hidden lg:flex" />
        <Dialog open={navigationOpen} onOpenChange={setNavigationOpen}>
          <DialogContent
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
            navigationButtonRef={navigationButtonRef}
            onOpenNavigation={() => setNavigationOpen(true)}
          />
          <main className="min-w-0 flex-1 overflow-auto p-4 animate-in fade-in duration-500 sm:p-6 md:p-8">
            <Outlet />
          </main>
        </div>
      </div>
    </UTCClockProvider>
  );
}
