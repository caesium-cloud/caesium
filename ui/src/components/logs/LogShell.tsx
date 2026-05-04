import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import { LogEmptyState } from "./LogEmptyState";

interface LogShellProps {
  /** Toolbar slot rendered above the content area. */
  toolbar?: ReactNode;
  /** The log renderer (xterm div, table, etc.). */
  children: ReactNode;
  /** Banner rendered above the toolbar (e.g. error/skip notices). */
  banner?: ReactNode;
  /** Shown as a centered overlay when hasVisibleOutput is false. */
  emptyState?: { title: string; body: string } | null;
  /** Whether the content area has visible output. Controls the empty overlay. */
  hasVisibleOutput?: boolean;
  /** Additional classes on the outer container. */
  className?: string;
}

export function LogShell({
  toolbar,
  children,
  banner,
  emptyState,
  hasVisibleOutput = true,
  className,
}: LogShellProps) {
  return (
    <div className={cn("flex h-full min-h-0 flex-col bg-obsidian", className)}>
      {banner}
      {toolbar}
      <div className="relative flex-1 overflow-hidden">
        {children}
        {!hasVisibleOutput && emptyState && (
          <LogEmptyState title={emptyState.title} body={emptyState.body} />
        )}
      </div>
    </div>
  );
}
