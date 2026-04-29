import { describe, expect, it } from "vitest";
import { act, render } from "@testing-library/react";
import { Sparkline } from "../sparkline";

function flushAnimationFrame() {
  return act(async () => {
    await new Promise((r) => requestAnimationFrame(() => r(undefined)));
  });
}

describe("<Sparkline />", () => {
  it("renders a placeholder dash for an empty run list", () => {
    const { container } = render(<Sparkline runs={[]} />);
    expect(container.querySelector("svg")).toBeNull();
    expect(container.textContent).toContain("—");
  });

  it("lazy-renders after the first animation frame", async () => {
    const runs = [
      { status: "succeeded", duration: 30 },
      { status: "failed", duration: 12 },
      { status: "running" },
    ];
    const { container } = render(<Sparkline runs={runs} />);
    expect(container.querySelector("svg")).toBeNull();
    await flushAnimationFrame();
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg?.querySelectorAll("rect").length).toBe(3);
  });

  it("animates running bars only", async () => {
    const runs = [
      { status: "succeeded", duration: 10 },
      { status: "running" },
    ];
    const { container } = render(<Sparkline runs={runs} />);
    await flushAnimationFrame();
    const animates = container.querySelectorAll("animate");
    expect(animates.length).toBe(1);
  });
});
