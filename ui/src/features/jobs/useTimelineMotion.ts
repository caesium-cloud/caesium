import { useLayoutEffect, useRef } from "react";
import { useReducedMotion } from "@/hooks/useReducedMotion";

/** Advance only the chart geometry each frame, not React or the DAG. */
export function useTimelineMotion(live: boolean, startedAt: number, elapsed: number, span: number) {
  const ref = useRef<HTMLDivElement>(null);
  const reducedMotion = useReducedMotion();
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    let frame = 0;
    const from = Number(element.style.getPropertyValue("--timeline-span")) || span;
    const began = performance.now();
    const paint = () => {
      const progress = Math.min(1, (performance.now() - began) / 400);
      // All marks, gridlines and the cursor share the same interpolated scale.
      const scale = live && !reducedMotion ? from + (span - from) * (1 - (1 - progress) ** 3) : span;
      element.style.setProperty("--timeline-span", String(scale));
      element.style.setProperty("--timeline-elapsed", String(live && !reducedMotion ? Math.max(0, Date.now() - startedAt) : elapsed));
      if (live && !reducedMotion && document.visibilityState === "visible") frame = requestAnimationFrame(paint);
    };
    const resume = () => { cancelAnimationFrame(frame); paint(); };
    paint();
    document.addEventListener("visibilitychange", resume);
    return () => { cancelAnimationFrame(frame); document.removeEventListener("visibilitychange", resume); };
  }, [live, startedAt, elapsed, span, reducedMotion]);
  return ref;
}
