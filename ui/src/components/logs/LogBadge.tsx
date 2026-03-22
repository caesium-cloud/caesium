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
        "rounded-md border border-slate-700 bg-slate-900 px-2 py-1 text-[10px] font-semibold uppercase tracking-wide text-slate-300",
        className,
      )}
    >
      {children}
    </span>
  );
}
