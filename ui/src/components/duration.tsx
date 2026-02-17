import { useEffect, useState } from "react";

interface DurationProps {
  start: string;
  end?: string | null;
}

export function Duration({ start, end }: DurationProps) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!end) {
      const timer = setInterval(() => {
        setNow(Date.now());
      }, 100);
      return () => clearInterval(timer);
    }
  }, [end]);

  const calculate = () => {
    const startTime = new Date(start).getTime();
    const endTime = end ? new Date(end).getTime() : now;
    const diff = endTime - startTime;

    if (diff <= 0) return "-";

    const seconds = diff / 1000;
    if (seconds < 1) return `${diff}ms`;
    if (seconds < 60) return `${seconds.toFixed(1)}s`;
    
    const minutes = seconds / 60;
    if (minutes < 60) return `${minutes.toFixed(1)}m`;
    
    const hours = minutes / 60;
    return `${hours.toFixed(1)}h`;
  };

  return <span>{calculate()}</span>;
}
