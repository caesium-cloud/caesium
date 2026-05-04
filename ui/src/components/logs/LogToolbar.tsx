import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

interface LogToolbarProps {
  children: ReactNode;
  status?: ReactNode;
  className?: string;
}

export function LogToolbar({ children, status, className }: LogToolbarProps) {
  return (
    <div
      className={cn(
        "flex flex-wrap items-center gap-2 border-b border-graphite/50 bg-obsidian/80 px-3 py-2",
        className,
      )}
    >
      {children}
      {status && (
        <div className="ml-auto flex flex-wrap items-center gap-2 text-[10px] font-semibold uppercase tracking-wide">
          {status}
        </div>
      )}
    </div>
  );
}
