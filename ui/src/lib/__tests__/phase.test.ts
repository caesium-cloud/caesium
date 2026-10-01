import { afterEach, expect, it, vi } from "vitest";
import { observePhase, syncPhase } from "../phase";

afterEach(() => vi.restoreAllMocks());

function animation(duration: number, animationName = "cs-spin") {
  return { animationName, startTime: 0, effect: { getTiming: () => ({ duration }) } } as unknown as Animation;
}

it("aligns actual animation start times across all instrument periods and leaves other motion alone", () => {
  const now = 1790874556789;
  const animations = [1, 2, 6, 22, 30, 38].map(seconds => animation(seconds * 1000));
  const other = animation(1000, "fade-in");
  Object.defineProperty(document, "timeline", { configurable: true, value: { currentTime: 50000 } });
  Object.defineProperty(document, "getAnimations", { configurable: true, value: () => [...animations, other] });
  syncPhase(now);
  animations.forEach((a, i) => expect(a.startTime).toBe(50000 - now % ([1, 2, 6, 22, 30, 38][i] * 1000)));
  expect(other.startTime).toBe(0);
});

it("aligns later insertions when their animation starts and removes its observer on cleanup", () => {
  Object.defineProperty(document, "timeline", { configurable: true, value: { currentTime: 50000 } });
  Object.defineProperty(document, "getAnimations", { configurable: true, value: () => [] });
  const a = animation(1000);
  const node = document.createElement("span");
  Object.defineProperty(node, "getAnimations", { value: () => [a] });
  document.body.append(node);
  vi.spyOn(Date, "now").mockReturnValue(1790874556789);
  const stop = observePhase();
  node.dispatchEvent(new Event("animationstart", { bubbles: true }));
  expect(a.startTime).toBe(49211);
  stop();
  a.startTime = 0;
  node.dispatchEvent(new Event("animationstart", { bubbles: true }));
  expect(a.startTime).toBe(0);
  node.remove();
});
