import { Profiler } from "react";
import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { RunStrip } from "../run-strip";
import { UTCClock, UTCClockProvider } from "../utc-clock";

beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-01T12:00:00Z")); });
afterEach(() => vi.useRealTimers());

it("keeps a running mark live and at the now edge when the server clock is ahead", () => {
  const { container } = render(<RunStrip runs={[{ status: "running", startedAt: "2026-10-01T12:00:05Z" }]} />);
  expect(screen.getByRole("img")).not.toHaveAccessibleName(/outside the current window/);
  const mark = container.querySelector<HTMLElement>('[data-status="running"]')!;
  expect(mark.style.opacity).toBe("1");
  expect(mark.style.transform).toBe("translateX(100cqw)");
  expect(mark.style.left).toBe("");
  expect(mark).toHaveTextContent("0s");
});

it("does not rerender archived rows on shared ticks and stops subscribing when recent history ages out", () => {
  const renders = { archive: 0, recent: 0 };
  const { unmount } = render(<UTCClockProvider>
    <UTCClock />
    <Profiler id="archive" onRender={() => renders.archive++}><RunStrip runs={[{ status: "succeeded", startedAt: "2026-09-30T12:00:00Z" }]} /></Profiler>
    <Profiler id="recent" onRender={() => renders.recent++}><RunStrip windowSeconds={2} runs={[{ status: "succeeded", startedAt: "2026-10-01T12:00:00Z" }]} /></Profiler>
  </UTCClockProvider>);
  const before = { ...renders };
  act(() => { vi.advanceTimersByTime(3000); });
  expect(renders.archive).toBe(before.archive);
  expect(renders.recent).toBeGreaterThan(before.recent);
  const stopped = renders.recent;
  act(() => { vi.advanceTimersByTime(3000); });
  expect(renders.recent).toBe(stopped);
  unmount();
  expect(vi.getTimerCount()).toBe(0);
});

it("separates archived history from time coordinates and gives missing dates an honest caption", () => {
  const { container, rerender } = render(<RunStrip runs={[{ status: "succeeded" }]} />);
  expect(screen.getByText(/Time unknown/)).toBeInTheDocument();
  expect(container.querySelector(".cs-time-mark")).toBeNull();
  rerender(<RunStrip runs={[]} />);
  expect(screen.getByText("No runs yet")).toBeInTheDocument();
  expect(container.querySelector(".cs-now-head")).toBeNull();
});

it("preserves duration height in the archive without assigning it a recent time position", () => {
  const { container } = render(<RunStrip runs={[
    { status: "succeeded", duration: .5, startedAt: "2026-09-30T12:00:00Z" },
    { status: "failed", duration: 10, startedAt: "2026-09-30T12:01:00Z" },
    { status: "skipped", startedAt: "2026-09-30T12:02:00Z" },
  ]} />);
  const marks = [...container.querySelectorAll<HTMLElement>("[data-status]")];
  expect(marks.map(mark => mark.style.height)).toEqual(["7px", "26px", "6px"]);
  expect(container.querySelector("[data-history-grid]")).toBeNull();
  expect(container.querySelector(".cs-live-bar")).toBeNull();
});

it("never presents an unknown terminal duration as elapsed live work", () => {
  const { container } = render(<RunStrip runs={[{ status: "succeeded", startedAt: "2026-10-01T11:59:00Z" }]} />);
  expect(container.querySelector<HTMLElement>('[data-status="succeeded"]')!.style.height).toBe("6px");
  expect(screen.getByRole("img")).toHaveAccessibleName(/duration unknown/);
});
