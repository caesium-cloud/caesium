import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { syncPhase } from "@/lib/phase";
import { cn } from "@/lib/utils";

const TickContext = createContext<Date | null>(null);

/**
 * Provides a shared "ticking now()" to every `<UTCClock />` and any future
 * clock-driven primitive: so we never fan out a `setInterval` per consumer.
 *
 * Mount once near the app root. Consumers that don't have the provider above
 * them will fall back to a local timer.
 */
export function UTCClockProvider({
  children,
  intervalMs = 1000,
}: {
  children: ReactNode;
  intervalMs?: number;
}) {
  const [now, setNow] = useState<Date>(() => new Date());
  useEffect(() => {
    syncPhase();
    let timer: number;
    const tick = () => {
      setNow(new Date());
      timer = window.setTimeout(tick, intervalMs - (Date.now() % intervalMs));
    };
    timer = window.setTimeout(tick, intervalMs - (Date.now() % intervalMs));
    const resync = () => { if (document.visibilityState === "visible") { clearTimeout(timer); syncPhase(); tick(); } };
    document.addEventListener("visibilitychange", resync);
    return () => { window.clearTimeout(timer); document.removeEventListener("visibilitychange", resync); };
  }, [intervalMs]);
  return <TickContext.Provider value={now}>{children}</TickContext.Provider>;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

function formatUTC(date: Date): string {
  return `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`;
}

interface UTCClockProps {
  className?: string;
  /** Suppress the gold pulse dot. Useful in dense layouts. */
  hideDot?: boolean;
}

// eslint-disable-next-line react-refresh/only-export-components
export function useUTCTick() {
  const ctxNow = useContext(TickContext);
  const hasProvider = ctxNow !== null;
  const [localNow, setLocalNow] = useState<Date>(() => new Date());

  // When a provider is mounted above us we let it drive the tick; otherwise
  // we run our own interval.
  useEffect(() => {
    if (hasProvider) return;
    let timer: number;
    const tick = () => { setLocalNow(new Date()); timer = window.setTimeout(tick, 1000 - Date.now() % 1000); };
    timer = window.setTimeout(tick, 1000 - Date.now() % 1000);
    return () => window.clearTimeout(timer);
  }, [hasProvider]);

  const now = ctxNow ?? localNow;
  return now;
}

export function UTCClock({ className }: UTCClockProps) {
  const text = formatUTC(useUTCTick());
  return (
    <div className={cn("flex items-center gap-2", className)}>
      <span className="text-base font-bold tabular-nums text-text-1">{text}</span>{" "}
      <span className="text-[11px] text-text-3">UTC</span>
    </div>
  );
}
