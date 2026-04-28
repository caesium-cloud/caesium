import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { cn } from "@/lib/utils";

const TickContext = createContext<Date | null>(null);

/**
 * Provides a shared "ticking now()" to every `<UTCClock />` and any future
 * clock-driven primitive — so we never fan out a `setInterval` per consumer.
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
    const id = window.setInterval(() => setNow(new Date()), intervalMs);
    return () => window.clearInterval(id);
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

export function UTCClock({ className, hideDot = false }: UTCClockProps) {
  const ctxNow = useContext(TickContext);
  const hasProvider = ctxNow !== null;
  const [localNow, setLocalNow] = useState<Date>(() => new Date());

  // When a provider is mounted above us we let it drive the tick; otherwise
  // we run our own interval.
  useEffect(() => {
    if (hasProvider) return;
    const id = window.setInterval(() => setLocalNow(new Date()), 1000);
    return () => window.clearInterval(id);
  }, [hasProvider]);

  const now = ctxNow ?? localNow;
  const text = useMemo(() => formatUTC(now), [now]);

  return (
    <div className={cn("flex items-center gap-2", className)}>
      {hideDot ? null : (
        <span
          aria-hidden="true"
          className="inline-block h-[7px] w-[7px] rounded-full bg-gold animate-gold-pulse shadow-[0_0_12px_hsl(var(--gold)/0.7)]"
        />
      )}
      <span className="font-mono tabular-nums text-[11px] tracking-[0.12em] text-text-2">
        {text} UTC
      </span>
    </div>
  );
}
