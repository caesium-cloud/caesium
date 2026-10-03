import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { observePhase } from "@/lib/phase";
import { cn, formatUTCTime } from "@/lib/utils";

function createClock(intervalMs: number) {
  let now = Date.now();
  let timer: number | undefined;
  const listeners = new Set<() => void>();
  const tick = () => {
    now = Date.now();
    listeners.forEach(listener => listener());
    if (listeners.size) timer = window.setTimeout(tick, intervalMs - Date.now() % intervalMs);
  };
  return {
    getSnapshot: () => now,
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      if (listeners.size === 1) {
        tick();
        document.addEventListener("visibilitychange", resync);
      }
      return () => {
        listeners.delete(listener);
        if (!listeners.size) {
          window.clearTimeout(timer);
          document.removeEventListener("visibilitychange", resync);
        }
      };
    },
  };
  function resync() {
    if (document.visibilityState === "visible") {
      window.clearTimeout(timer);
      tick();
    }
  }
}

const TickContext = createContext<ReturnType<typeof createClock> | null>(null);
const noSubscription = () => () => {};

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
  const clock = useMemo(() => createClock(intervalMs), [intervalMs]);
  useEffect(() => observePhase(), []);
  return <TickContext.Provider value={clock}>{children}</TickContext.Provider>;
}

interface UTCClockProps {
  className?: string;
}

// eslint-disable-next-line react-refresh/only-export-components
export function useUTCTick(enabled: boolean | ((now: number) => boolean) = true) {
  const sharedClock = useContext(TickContext);
  const localClock = useMemo(() => createClock(1000), []);
  const clock = sharedClock ?? localClock;
  const active = typeof enabled === "function" ? enabled(clock.getSnapshot()) : enabled;
  const getSnapshot = useMemo(() => {
    if (active) return clock.getSnapshot;
    const frozen = clock.getSnapshot();
    return () => frozen;
  }, [clock, active]);
  const now = useSyncExternalStore(active ? clock.subscribe : noSubscription, getSnapshot);
  return new Date(now);
}

export function UTCClock({ className }: UTCClockProps) {
  const text = formatUTCTime(useUTCTick());
  return (
    <div className={cn("flex items-center gap-2", className)}>
      <span className="text-base font-bold tabular-nums text-text-1">{text}</span>{" "}
      <span className="text-[11px] text-text-3">UTC</span>
    </div>
  );
}
