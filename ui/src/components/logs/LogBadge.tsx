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
        "inline-flex items-center gap-1.5 text-[11px] font-bold lowercase text-text-2",
        className,
        "rounded-none border-0 bg-transparent",
      )}
    >
      {children}
    </span>
  );
}
