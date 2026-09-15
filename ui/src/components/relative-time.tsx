import { useEffect, useState } from "react";
import { formatRelativeTime } from "./relative-time-format";

interface RelativeTimeProps {
  date: string;
}

export function RelativeTime({ date }: RelativeTimeProps) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => {
      setNow(Date.now());
    }, 10000); // Update every 10s is sufficient for "m ago"

    return () => clearInterval(timer);
  }, []);

  return <span>{formatRelativeTime(date, now)}</span>;
}
