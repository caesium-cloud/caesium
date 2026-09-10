import { describe, expect, it } from "vitest";
import { formatCommandForDisplay, formatUTCTimestamp, normalizeCommand } from "../utils";

describe("command formatting", () => {
  it("decodes JSON-array command strings before joining for display", () => {
    expect(formatCommandForDisplay('["sh","-c","echo \\u003e /out/files.json \\u0026\\u0026 echo ok"]')).toBe(
      "sh -c echo > /out/files.json && echo ok",
    );
  });

  it("handles already decoded arrays and raw command strings", () => {
    expect(formatCommandForDisplay(["python", "-m", "pytest"])).toBe("python -m pytest");
    expect(formatCommandForDisplay("echo already > /tmp/out")).toBe("echo already > /tmp/out");
    expect(formatCommandForDisplay()).toBe("N/A");
  });

  it("normalizes each accepted command shape into argv", () => {
    expect(normalizeCommand('["sh","-c","echo ok"]')).toEqual(["sh", "-c", "echo ok"]);
    expect(normalizeCommand(["sh", "-c"])).toEqual(["sh", "-c"]);
    expect(normalizeCommand("plain string")).toEqual(["plain string"]);
    expect(normalizeCommand("   ")).toEqual([]);
    expect(normalizeCommand()).toEqual([]);
  });
});

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
