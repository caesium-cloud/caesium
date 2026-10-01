import { expect, it } from "vitest";
import { syncPhase } from "../phase";

it("locks every loop to the same UTC epoch, including longer orbit periods", () => {
  const now = 1790874556789;
  syncPhase(now);
  for (const seconds of [1, 2, 6, 22, 30, 38]) {
    expect(document.documentElement.style.getPropertyValue(seconds === 1 ? "--cs-phase" : `--cs-phase-${seconds}`))
      .toBe(`${-(now % (seconds * 1000))}ms`);
  }
});
