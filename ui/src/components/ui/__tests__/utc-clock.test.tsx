import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { UTCClock, UTCClockProvider } from "../utc-clock";

describe("<UTCClock />", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(Date.UTC(2025, 0, 1, 12, 34, 56)));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders the current UTC time padded to HH:MM:SS", () => {
    const { container } = render(<UTCClock />);
    expect(container.textContent).toContain("12:34:56 UTC");
  });

  it("ticks once per second when running standalone", () => {
    const { container } = render(<UTCClock />);
    expect(container.textContent).toContain("12:34:56");
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(container.textContent).toContain("12:34:57");
  });

  it("multiple clocks under one provider read from the same tick", () => {
    const { container } = render(
      <UTCClockProvider>
        <UTCClock />
        <UTCClock />
        <UTCClock />
      </UTCClockProvider>,
    );
    const texts = Array.from(container.querySelectorAll("span.font-mono")).map(
      (n) => n.textContent,
    );
    expect(new Set(texts).size).toBe(1);
  });

  it("hides the gold pulse dot when hideDot is set", () => {
    const { container } = render(<UTCClock hideDot />);
    expect(container.querySelector(".animate-gold-pulse")).toBeNull();
  });
});
