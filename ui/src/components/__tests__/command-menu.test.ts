import { describe, expect, it } from "vitest";
import { commandPaletteFilter } from "../command-filter";

describe("commandPaletteFilter", () => {
  it("does not match scattered subsequence letters", () => {
    expect(commandPaletteFilter("job process-production abc12345", "cron", ["process-production", "abc12345"])).toBe(0);
  });

  it("matches alias substrings and word prefixes", () => {
    expect(commandPaletteFilter("job cron-success-fast abc12345", "cron", ["cron-success-fast", "abc12345"])).toBeGreaterThan(0);
    expect(commandPaletteFilter("job process-production abc12345", "prod", ["process-production", "abc12345"])).toBeGreaterThan(0);
  });

  it("matches short ids", () => {
    expect(commandPaletteFilter("trigger nightly abc12345", "abc123", ["nightly", "abc12345"])).toBeGreaterThan(0);
  });
});
