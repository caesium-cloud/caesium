import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

/**
 * Measures the remaining vertical space from a container element to the bottom
 * of the main panel in unscrolled layout coordinates. Scrolling must not grow
 * the graph and push the content below it out of reach.
 *
 * @param isLoading Pass `true` while data is still loading; the measurement is
 *   deferred until this becomes `false` so the layout has stabilised.
 * @param bottomPadding Pixels to subtract from the bottom (default 32).
 * @param minHeight Minimum height in pixels (default 400).
 */
export function useDagHeight(
  isLoading: boolean,
  bottomPadding = 32,
  minHeight = 400,
): [React.RefObject<HTMLDivElement | null>, number | null] {
  const containerRef = useRef<HTMLDivElement>(null);
  const [dagHeight, setDagHeight] = useState<number | null>(null);

  const measure = useCallback(() => {
    const el = containerRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const main = el.closest("main");
    const mainBottom = main?.getBoundingClientRect().bottom ?? window.innerHeight;
    const layoutTop = rect.top + (main?.scrollTop ?? window.scrollY);
    setDagHeight(Math.max(minHeight, Math.min(window.innerHeight, mainBottom) - layoutTop - bottomPadding));
  }, [bottomPadding, minHeight]);

  // Re-measure whenever the window resizes.
  useEffect(() => {
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [measure]);

  // Measure once loading finishes so the layout has settled.
  useLayoutEffect(() => {
    if (!isLoading) measure();
  }, [isLoading, measure]);

  return [containerRef, dagHeight];
}
