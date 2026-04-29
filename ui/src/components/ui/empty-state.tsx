import { type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { AtomLogo } from "@/components/brand/atom-logo";

interface EmptyStateProps {
  title: string;
  subtitle?: string;
  action?: ReactNode;
  /** Override the default atom-motif graphic. */
  icon?: ReactNode;
  className?: string;
}

/**
 * Stock empty-state surface. Uses a static AtomLogo as the motif so the page
 * stays calm; no spinning logos for "nothing here yet."
 */
export function EmptyState({
  title,
  subtitle,
  action,
  icon,
  className,
}: EmptyStateProps) {
  return (
    <div
      role="status"
      className={cn(
        "flex flex-col items-center justify-center gap-3.5 px-5 py-16 text-center",
        className,
      )}
    >
      <div className="opacity-70">
        {icon ?? <AtomLogo size={80} animated={false} />}
      </div>
      <div className="text-base font-medium text-text-1">{title}</div>
      {subtitle ? (
        <div className="max-w-sm text-[13px] text-text-3">{subtitle}</div>
      ) : null}
      {action ? <div className="pt-1">{action}</div> : null}
    </div>
  );
}
