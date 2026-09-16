import { useEffect, useState } from "react";
import { formatRelativeTime } from "./relative-time-format";

interface RelativeTimeProps {
  date: string;
  /** A scheduled or expiry instant that should count down instead of absorbing clock skew. */
  future?: boolean;
}

export function RelativeTime({ date, future = false }: RelativeTimeProps) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => {
      setNow(Date.now());
    }, 10000); // Update every 10s is sufficient for "m ago"

    return () => clearInterval(timer);
  }, []);

  return <span>{formatRelativeTime(date, now, future)}</span>;
}
