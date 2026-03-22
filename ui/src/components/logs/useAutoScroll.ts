import { useCallback, useEffect, useState, type RefObject } from "react";

interface UseAutoScrollOptions {
  /** Element with overflow-auto that receives the scroll events. */
  scrollRef: RefObject<HTMLElement | null>;
  /** Sentinel element at the bottom of the scrollable content. */
  bottomRef: RefObject<HTMLElement | null>;
  /** Scroll is re-evaluated whenever this value changes (e.g. item count). */
  dependency: unknown;
  /** Distance (px) from bottom to still count as "at the bottom". Default 60. */
  threshold?: number;
}

interface UseAutoScrollResult {
  autoScroll: boolean;
  /** Attach to the scroll container's onScroll prop. */
  handleScroll: () => void;
  /** Programmatically jump to the bottom and re-enable auto-scroll. */
  jumpToBottom: () => void;
}

export function useAutoScroll({
  scrollRef,
  bottomRef,
  dependency,
  threshold = 60,
}: UseAutoScrollOptions): UseAutoScrollResult {
  const [autoScroll, setAutoScroll] = useState(true);

  useEffect(() => {
    if (autoScroll && bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: "smooth" });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dependency, autoScroll]);

  const handleScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < threshold;
    setAutoScroll(atBottom);
  }, [scrollRef, threshold]);

  const jumpToBottom = useCallback(() => {
    setAutoScroll(true);
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [bottomRef]);

  return { autoScroll, handleScroll, jumpToBottom };
}
