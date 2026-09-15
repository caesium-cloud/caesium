import { describe, expect, it } from "vitest";
import { formatRelativeTime } from "../relative-time-format";

const now = Date.UTC(2026, 8, 14, 12, 0, 0);

describe("formatRelativeTime", () => {
  it("keeps existing past labels", () => {
    expect(formatRelativeTime(new Date(now - 30_000).toISOString(), now)).toBe("30s ago");
    expect(formatRelativeTime(new Date(now - 60 * 60_000).toISOString(), now)).toBe("1h ago");
    expect(formatRelativeTime(new Date(now - 3 * 24 * 60 * 60_000).toISOString(), now)).toBe("3d ago");
  });

  it("labels imminent, minute, day, and distant future instants as upcoming", () => {
    expect(formatRelativeTime(new Date(now + 500).toISOString(), now)).toBe("just now");
    expect(formatRelativeTime(new Date(now + 60_000).toISOString(), now)).toBe("in 1m");
    expect(formatRelativeTime(new Date(now + 24 * 60 * 60_000).toISOString(), now)).toBe("in 1d");
    expect(formatRelativeTime(new Date(now + 109 * 24 * 60 * 60_000).toISOString(), now)).toBe("in 109d");
  });

  it("does not present malformed timestamps as immediately due", () => {
    expect(formatRelativeTime("not-a-date", now)).toBe("Unknown time");
  });
});
