import { describe, expect, it } from "vitest";
import type { RunQueueItem } from "@/lib/api";
import { isNamespaceQuotaHeld, namespaceQuotaHeldLabel, queuePendingReason } from "../queue-utils";

function row(overrides: Partial<RunQueueItem> = {}): RunQueueItem {
  return {
    id: "1e2a3b4c-0000-0000-0000-000000000000",
    position: 2,
    priority: 2,
    enqueued_at: "2026-09-09T12:00:00Z",
    ...overrides,
  };
}

describe("namespace quota hold label (identity-and-access F4)", () => {
  it("labels a row the namespace quota holds", () => {
    const held = row({ held_reason: "namespace_quota" });
    expect(isNamespaceQuotaHeld(held)).toBe(true);
    expect(queuePendingReason(held)).toBe("Held by namespace quota");
    expect(namespaceQuotaHeldLabel).toBe("Held by namespace quota");
  });

  it("names an unknown reason instead of hiding it", () => {
    expect(queuePendingReason(row({ held_reason: "future_reason" }))).toBe("Held: future_reason");
    expect(isNamespaceQuotaHeld(row({ held_reason: "future_reason" }))).toBe(false);
  });

  it("keeps a stale claim as the explanation: that is the stuck state", () => {
    expect(queuePendingReason(row({ held_reason: "namespace_quota", claim_state: "stale", claimed_by: "n1" }))).toMatch(
      /Claim by n1 expired/,
    );
  });

  it("leaves a row without a held_reason unchanged", () => {
    expect(isNamespaceQuotaHeld(row())).toBe(false);
    expect(queuePendingReason(row())).toBe("Waiting for a run slot; normal priority at position #2");
  });
});
