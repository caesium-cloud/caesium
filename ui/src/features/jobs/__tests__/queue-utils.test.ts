import { describe, expect, it } from "vitest";
import type { RunQueueItem } from "@/lib/api";
import { formatPriority, formatQueueParams, isStaleQueueRow, queuePendingReason } from "../queue-utils";

function row(overrides: Partial<RunQueueItem> = {}): RunQueueItem {
  return {
    id: "1e2a3b4c-0000-0000-0000-000000000000",
    position: 1,
    priority: 2,
    enqueued_at: "2026-09-09T12:00:00Z",
    ...overrides,
  };
}

describe("isStaleQueueRow", () => {
  it("reads either half of the server's annotation", () => {
    expect(isStaleQueueRow(row({ stale: true }))).toBe(true);
    expect(isStaleQueueRow(row({ claim_state: "stale" }))).toBe(true);
  });

  it("does not call a live claim stuck", () => {
    expect(isStaleQueueRow(row({ claim_state: "claimed", claimed_by: "node-a/44f" }))).toBe(false);
    expect(isStaleQueueRow(row({ claim_state: "pending" }))).toBe(false);
  });

  it("does not call an un-annotated row stuck", () => {
    expect(isStaleQueueRow(row())).toBe(false);
  });
});

describe("queuePendingReason", () => {
  it("says a stale row waits on the reaper, and names the dead dequeuer", () => {
    const reason = queuePendingReason(row({ stale: true, claim_state: "stale", claimed_by: "node-a/dead" }));
    expect(reason).toContain("node-a/dead");
    expect(reason).toContain("died mid-drain");
  });

  it("still explains a stale row with no named holder", () => {
    expect(queuePendingReason(row({ claim_state: "stale" }))).toContain("expired");
  });

  it("says a live claim is starting", () => {
    expect(queuePendingReason(row({ claim_state: "claimed", claimed_by: "node-b/live" }))).toBe(
      "Starting on node-b/live",
    );
    expect(queuePendingReason(row({ claim_state: "claimed" }))).toContain("Claimed by a dequeuer");
  });

  it("falls back to the waiting-for-capacity reason for a pending row", () => {
    expect(queuePendingReason(row({ claim_state: "pending", priority: 3, position: 2 }))).toBe(
      "Waiting for a run slot; high priority at position #2",
    );
  });

  it("keeps the pre-existing blocked and explicit-reason paths", () => {
    expect(queuePendingReason(row({ blocked: true }))).toBe("Blocked by scheduler policy");
    expect(queuePendingReason(row({ blocked: true, reason: "quota" }))).toBe("Blocked: quota");
    expect(queuePendingReason(row({ pending_reason: "waiting on upstream" }))).toBe("waiting on upstream");
  });

  it("prefers the claim state over a stale explicit reason", () => {
    expect(queuePendingReason(row({ claim_state: "stale", pending_reason: "waiting on upstream" }))).toContain(
      "expired",
    );
  });
});

describe("formatPriority", () => {
  it("maps the three priority values", () => {
    expect(formatPriority(1)).toBe("low");
    expect(formatPriority(2)).toBe("normal");
    expect(formatPriority(3)).toBe("high");
  });
});

describe("formatQueueParams", () => {
  it("sorts params and reports an empty map", () => {
    expect(formatQueueParams({ b: "2", a: "1" })).toBe("a=1, b=2");
    expect(formatQueueParams({})).toBe("no params");
    expect(formatQueueParams(undefined)).toBe("no params");
  });
});
