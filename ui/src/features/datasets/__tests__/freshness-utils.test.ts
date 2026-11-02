import { describe, expect, it } from "vitest";
import {
  DATASET_STATUS_FILTERS,
  freshnessTone,
  isStaleLike,
  normalizeStatusFilter,
} from "../freshness-utils";

describe("held dataset status", () => {
  it("offers a Held filter and round-trips it through the URL normaliser", () => {
    expect(DATASET_STATUS_FILTERS.some((entry) => entry.key === "held" && entry.label === "Held")).toBe(true);
    expect(normalizeStatusFilter("held")).toBe("held");
  });

  it("renders held as its own danger-toned label, never as fresh", () => {
    const tone = freshnessTone("held");
    expect(tone.label).toBe("held");
    expect(tone.label).not.toBe("fresh");
    expect(tone.textClass).toContain("danger");
  });

  it("makes a held dataset stale-like so lineage marks its downstream", () => {
    expect(isStaleLike("held")).toBe(true);
    expect(isStaleLike("fresh")).toBe(false);
  });
});
