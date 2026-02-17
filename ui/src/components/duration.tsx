import { useEffect, useState } from "react";

interface DurationProps {
  start: string;
  end?: string | null;
}

export function Duration({ start, end }: DurationProps) {
  const [duration, setDuration] = useState<string>("-");

  useEffect(() => {
    const calculate = () => {
      const startTime = new Date(start).getTime();
      const endTime = end ? new Date(end).getTime() : Date.now();
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

    setDuration(calculate());

    if (!end) {
      const timer = setInterval(() => {
        setDuration(calculate());
      }, 100);
      return () => clearInterval(timer);
    }
  }, [start, end]);

  return <span>{duration}</span>;
}
