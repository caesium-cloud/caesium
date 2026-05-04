import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

interface LogBadgeProps {
  children: ReactNode;
  className?: string;
}

export function LogBadge({ children, className }: LogBadgeProps) {
  return (
    <span
      className={cn(
        "rounded-md border border-graphite/60 bg-midnight px-2 py-1 text-[10px] font-semibold uppercase tracking-wide text-text-2",
        className,
      )}
    >
      {children}
    </span>
  );
}
