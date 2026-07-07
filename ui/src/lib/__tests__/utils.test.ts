import { describe, expect, it } from "vitest";
import { formatUTCTimestamp } from "../utils";

describe("formatUTCTimestamp", () => {
  it("formats a fixed instant as a labelled UTC wall-clock timestamp", () => {
    expect(formatUTCTimestamp(Date.UTC(2026, 6, 2, 10, 0, 5))).toBe(
      "2026-07-02 10:00:05 UTC",
    );
  });

  it("returns the fallback for invalid timestamps", () => {
    expect(formatUTCTimestamp("not-a-date", "unavailable")).toBe("unavailable");
  });

  it("returns the fallback for null, undefined, or empty input", () => {
    expect(formatUTCTimestamp(null, "unavailable")).toBe("unavailable");
    expect(formatUTCTimestamp(undefined, "unavailable")).toBe("unavailable");
    expect(formatUTCTimestamp("", "unavailable")).toBe("unavailable");
  });
});
