import { Outlet, useNavigate } from "@tanstack/react-router";
import { Sidebar } from "./Sidebar";
import { Header } from "./Header";
import { useEffect } from "react";
import { events } from "@/lib/events";

export function AppShell() {
  const navigate = useNavigate();

  useEffect(() => {
    // Initialize global SSE connection
    events.connect();
    return () => events.disconnect();
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
          if (nextEvent.key === "s") navigate({ to: "/stats" });
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
    <div className="flex h-screen w-full bg-background">
      <Sidebar />
      <div className="flex flex-col flex-1 overflow-hidden">
        <Header />
        <main className="flex-1 overflow-auto p-6 animate-in fade-in duration-500">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
