import { describe, expect, it } from "vitest";
import { formatRelativeTime } from "../relative-time-format";

const now = Date.UTC(2026, 8, 14, 12, 0, 0);

describe("formatRelativeTime", () => {
  it("keeps existing past labels", () => {
    expect(formatRelativeTime(new Date(now - 30_000).toISOString(), now)).toBe("30s ago");
    expect(formatRelativeTime(new Date(now - 60 * 60_000).toISOString(), now)).toBe("1h ago");
    expect(formatRelativeTime(new Date(now - 3 * 24 * 60 * 60_000).toISOString(), now)).toBe("3d ago");
  });

  it("absorbs small future clock skew for historical timestamps", () => {
    expect(formatRelativeTime(new Date(now + 30_000).toISOString(), now)).toBe("just now");
  });

  it("labels imminent, minute, day, and distant future instants as upcoming", () => {
    expect(formatRelativeTime(new Date(now + 500).toISOString(), now, true)).toBe("just now");
    expect(formatRelativeTime(new Date(now + 60_000).toISOString(), now, true)).toBe("in 1m");
    expect(formatRelativeTime(new Date(now + 24 * 60 * 60_000).toISOString(), now, true)).toBe("in 1d");
    expect(formatRelativeTime(new Date(now + 109 * 24 * 60 * 60_000).toISOString(), now, true)).toBe("in 109d");
  });

  it("rounds countdowns up without changing elapsed-time rounding", () => {
    expect(formatRelativeTime(new Date(now + 61_000).toISOString(), now, true)).toBe("in 2m");
    expect(formatRelativeTime(new Date(now + 25 * 60 * 60_000).toISOString(), now, true)).toBe("in 2d");
    expect(formatRelativeTime(new Date(now - 61_000).toISOString(), now)).toBe("1m ago");
  });

  it("does not present malformed timestamps as immediately due", () => {
    expect(formatRelativeTime("not-a-date", now)).toBe("Unknown time");
  });
});
