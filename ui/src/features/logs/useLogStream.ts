import { useCallback, useEffect, useRef, useState } from "react";

export interface LogEntry {
  sequence: number;
  ts: string;
  level: string;
  msg: string;
  caller?: string;
  fields?: Record<string, unknown>;
}

const MAX_CLIENT_ENTRIES = 5000;
const API_BASE_URL = import.meta.env.VITE_API_BASE_URL || "/v1";

interface UseLogStreamOptions {
  level?: string;
}

interface UseLogStreamResult {
  entries: LogEntry[];
  connected: boolean;
  paused: boolean;
  error: string | null;
  pause: () => void;
  resume: () => void;
  clear: () => void;
}

export function useLogStream(opts: UseLogStreamOptions = {}): UseLogStreamResult {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [connected, setConnected] = useState(false);
  const [paused, setPaused] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const eventSourceRef = useRef<EventSource | null>(null);
  const lastSeqRef = useRef(0);

  const connect = useCallback(() => {
    if (eventSourceRef.current) {
      eventSourceRef.current.close();
    }

    const params = new URLSearchParams();
    if (opts.level) params.set("level", opts.level);
    if (lastSeqRef.current > 0) params.set("since", String(lastSeqRef.current));

    const qs = params.toString();
    const url = `${API_BASE_URL}/logs/stream${qs ? `?${qs}` : ""}`;
    const es = new EventSource(url);
    eventSourceRef.current = es;

    es.addEventListener("open", () => {
      setConnected(true);
      setError(null);
    });

    es.addEventListener("log", (evt) => {
      try {
        const entry: LogEntry = JSON.parse(evt.data);
        lastSeqRef.current = entry.sequence;
        setEntries((prev) => {
          const next = [...prev, entry];
          return next.length > MAX_CLIENT_ENTRIES
            ? next.slice(next.length - MAX_CLIENT_ENTRIES)
            : next;
        });
      } catch {
        // skip malformed entries
      }
    });

    es.addEventListener("error", () => {
      setConnected(false);
      // EventSource auto-reconnects; only surface persistent failures.
      if (es.readyState === EventSource.CLOSED) {
        setError("Log stream disconnected");
      }
    });
  }, [opts.level]);

  const disconnect = useCallback(() => {
    if (eventSourceRef.current) {
      eventSourceRef.current.close();
      eventSourceRef.current = null;
    }
    setConnected(false);
  }, []);

  const pause = useCallback(() => {
    disconnect();
    setPaused(true);
  }, [disconnect]);

  const resume = useCallback(() => {
    setPaused(false);
    connect();
  }, [connect]);

  const clear = useCallback(() => {
    setEntries([]);
  }, []);

  useEffect(() => {
    if (!paused) {
      connect();
    }
    return () => disconnect();
  }, [connect, disconnect, paused]);

  return { entries, connected, paused, error, pause, resume, clear };
}
