import { describe, expect, it } from "vitest";
import { rerunParams } from "../rerun-params";

describe("rerunParams", () => {
  it("copies business inputs without inheriting scheduler provenance", () => {
    const source = {
      mode: "specific",
      customer_date: "2026-09-01",
      _trigger_depth: "31",
      _derived_from_dataset: "orders",
      _consumed_watermarks: "old-watermarks",
      _consumed_watermarks_start: "old-start",
      _future_scheduler_field: "internal",
      logical_date: "2026-09-01T00:00:00Z",
    };

    const selected = rerunParams(source);
    expect(selected).toEqual({ mode: "specific", customer_date: "2026-09-01" });
    selected!.mode = "changed";
    expect(source.mode).toBe("specific");
    expect(source._trigger_depth).toBe("31");
  });

  it("retains the parameterless trigger behavior when no business input remains", () => {
    expect(rerunParams()).toBeUndefined();
    expect(rerunParams({ _trigger_depth: "1", logical_date: "old-slot" })).toBeUndefined();
  });
});
