import { cn } from "@/lib/utils";
import { useId } from "react";

export function Oscillator({ className, ambient = false }: { className?: string; ambient?: boolean }) {
  const patternId = useId();
  if (ambient) return <svg aria-hidden="true" width="100%" height="60" data-testid="ambient-oscillator" className={cn("cs-oscillator cs-animated text-running", className)}>
    <defs><pattern id={patternId} width="40" height="60" patternUnits="userSpaceOnUse"><path d="M0 30 Q10 2 20 30 T40 30" fill="none" stroke="currentColor" /></pattern></defs>
    <g className="cs-wave"><rect width="100%" height="60" style={{ width: "calc(100% + 40px)" }} fill={`url(#${patternId})`} /></g>
    <path className="cs-wave-flat" d="M0 30 H10000" fill="none" stroke="currentColor" />
  </svg>;
  return <svg aria-hidden="true" width="120" height="20" viewBox="0 0 120 20" className={cn("cs-oscillator cs-animated shrink-0 text-running", className)}>
    <path className="cs-wave" d="M0 10 Q10 -6 20 10 T40 10 T60 10 T80 10 T100 10 T120 10 T140 10 T160 10" fill="none" stroke="currentColor" strokeWidth="1.5" />
    <path className="cs-wave-flat" d="M0 10 H120" fill="none" stroke="currentColor" />
  </svg>;
}
