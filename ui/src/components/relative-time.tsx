import { useEffect, useState } from "react";

interface RelativeTimeProps {
  date: string;
}

export function RelativeTime({ date }: RelativeTimeProps) {
  const [relative, setRelative] = useState<string>("-");

  useEffect(() => {
    const calculate = () => {
      const time = new Date(date).getTime();
      const diff = Date.now() - time;

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

    setRelative(calculate());

    const timer = setInterval(() => {
      setRelative(calculate());
    }, 10000); // Update every 10s is sufficient for "m ago"

    return () => clearInterval(timer);
  }, [date]);

  return <span>{relative}</span>;
}
