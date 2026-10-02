import type { ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export function FilterChip({ active, className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { active: boolean }) {
  return <button type="button" aria-pressed={active} className={cn("inline-flex min-h-8 items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs transition-colors", active ? "border-cyan/60 bg-cyan/10 text-text-1" : "border-border text-text-3 hover:border-input hover:text-text-1", className)} {...props} />;
}
