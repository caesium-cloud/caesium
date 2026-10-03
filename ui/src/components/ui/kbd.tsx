import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";

/** A decorative shortcut hint; its parent remains the interactive control. */
export function Kbd({ className, ...props }: ComponentProps<"kbd">) {
  return (
    <kbd
      aria-hidden="true"
      className={cn(
        "inline-flex h-5 min-w-5 shrink-0 items-center justify-center gap-1 whitespace-nowrap rounded border border-border bg-midnight px-1 font-mono text-[11px] font-normal normal-case leading-none text-text-2",
        className,
      )}
      {...props}
    />
  );
}
