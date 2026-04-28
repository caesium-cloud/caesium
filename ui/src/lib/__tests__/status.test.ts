import { describe, expect, it } from "vitest";
import { ALL_RUN_STATUSES, statusMeta } from "../status";

describe("statusMeta", () => {
  it("returns a stable shape for every canonical status", () => {
    for (const status of ALL_RUN_STATUSES) {
      const meta = statusMeta(status);
      expect(meta.label).toBe(status);
      expect(meta.fg).toMatch(/^hsl\(/);
      expect(meta.bg).toMatch(/^hsl\(/);
      expect(meta.border).toMatch(/^hsl\(/);
      expect(typeof meta.dotClass).toBe("string");
    }
  });

  it("animates only running and paused dots", () => {
    expect(statusMeta("running").dotClass).toContain("cyan-pulse");
    expect(statusMeta("paused").dotClass).toContain("gold-pulse");
    expect(statusMeta("succeeded").dotClass).toBe("");
    expect(statusMeta("failed").dotClass).toBe("");
    expect(statusMeta("queued").dotClass).toBe("");
    expect(statusMeta("cached").dotClass).toBe("");
    expect(statusMeta("skipped").dotClass).toBe("");
  });

  it("normalizes common aliases to canonical statuses", () => {
    expect(statusMeta("success").label).toBe("succeeded");
    expect(statusMeta("ERROR").label).toBe("failed");
    expect(statusMeta("Cancelled").label).toBe("failed");
    expect(statusMeta("pending").label).toBe("queued");
    expect(statusMeta("active").label).toBe("running");
  });

  it("falls back to a neutral 'unknown' meta", () => {
    const fallback = statusMeta("definitely-not-a-status");
    expect(fallback.label).toBe("unknown");
    expect(fallback.fg).toMatch(/^hsl\(/);
  });

  it("handles null / undefined / empty input", () => {
    expect(statusMeta(null).label).toBe("unknown");
    expect(statusMeta(undefined).label).toBe("unknown");
    expect(statusMeta("").label).toBe("unknown");
  });
});
