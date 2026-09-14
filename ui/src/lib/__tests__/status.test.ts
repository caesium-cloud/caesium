import { describe, expect, it } from "vitest";
import {
  ALL_AGENT_ACTION_STATUSES,
  ALL_AGENT_SESSION_STATUSES,
  ALL_INCIDENT_STATUSES,
  ALL_RUN_STATUSES,
  statusMeta,
  statusMetaForDomain,
} from "../status";

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

  it("keeps pending and skipped visually distinct", () => {
    const pending = statusMeta("pending");
    const queued = statusMeta("queued");
    const skipped = statusMeta("skipped");

    expect(pending.fg).toBe(queued.fg);
    expect(queued.fg).not.toBe(skipped.fg);
    expect(queued.bg).not.toBe(skipped.bg);
    expect(queued.border).not.toBe(skipped.border);
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

  it("preserves every incident lifecycle label", () => {
    for (const status of ALL_INCIDENT_STATUSES) {
      expect(statusMetaForDomain(status, "incident").label).toBe(status.replaceAll("_", " "));
    }
    expect(statusMetaForDomain("open", "incident").label).toBe("open");
    expect(statusMeta("open").label).toBe("unknown");
  });

  it("keeps agent lifecycles distinct from run aliases", () => {
    for (const status of ALL_AGENT_ACTION_STATUSES) {
      expect(statusMetaForDomain(status, "agent-action").label).toBe(status.replaceAll("_", " "));
    }
    for (const status of ALL_AGENT_SESSION_STATUSES) {
      expect(statusMetaForDomain(status, "agent-session").label).toBe(status.replaceAll("_", " "));
    }
    expect(statusMetaForDomain("cancelled", "agent-session").label).toBe("cancelled");
    expect(statusMeta("cancelled").label).toBe("failed");
  });

  it("falls back for inherited property names in every explicit domain", () => {
    expect(statusMetaForDomain("constructor", "incident").label).toBe("unknown");
    expect(statusMetaForDomain("__proto__", "agent-action").label).toBe("unknown");
    expect(statusMetaForDomain("toString", "agent-session").label).toBe("unknown");
  });
});
