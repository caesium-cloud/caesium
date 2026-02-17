import { useEffect, useState } from "react";

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

  const calculate = () => {
    const time = new Date(date).getTime();
    const diff = now - time;

    if (diff < 0) return "just now";

    const seconds = Math.floor(diff / 1000);
    if (seconds < 60) return `${seconds}s ago`;

    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;

    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;

    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  };

  return <span>{calculate()}</span>;
}
