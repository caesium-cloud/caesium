import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

/**
 * Measures the remaining vertical space from a container element to the bottom
 * of the viewport, re-measuring on window resize and main-panel scroll so the
 * DAG always fills the visible area even after the user scrolls.
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
    setDagHeight(Math.max(minHeight, window.innerHeight - rect.top - bottomPadding));
  }, [bottomPadding, minHeight]);

  // Re-measure whenever the window resizes.
  useEffect(() => {
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [measure]);

  // Re-measure when the AppShell <main> scroll container scrolls, because
  // scrolling changes getBoundingClientRect().top for elements above the fold.
  useEffect(() => {
    const main = document.querySelector("main");
    if (!main) return;
    main.addEventListener("scroll", measure, { passive: true });
    return () => main.removeEventListener("scroll", measure);
  }, [measure]);

  // Measure once loading finishes so the layout has settled.
  useLayoutEffect(() => {
    if (!isLoading) measure();
  }, [isLoading, measure]);

  return [containerRef, dagHeight];
}
