import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { AtomLogo } from "../atom-logo";

describe("<AtomLogo />", () => {
  it("renders a labelled SVG with three orbits, a nucleus, and three satellites", () => {
    const { container } = render(<AtomLogo size={64} />);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg?.getAttribute("aria-label")).toBe("Caesium");
    expect(svg?.getAttribute("width")).toBe("64");

    expect(container.querySelectorAll("ellipse").length).toBe(3);
    // 1 glow halo + 1 nucleus + 3 gold satellites = 5 circles
    expect(container.querySelectorAll("circle").length).toBe(5);
  });

  it("animates by default", () => {
    const { container } = render(<AtomLogo />);
    expect(container.querySelectorAll(".atom-orbit").length).toBe(3);
    expect(container.querySelector(".atom-nucleus")).not.toBeNull();
    expect(container.querySelector("svg")?.getAttribute("data-reduced-motion")).toBe(
      "false",
    );
  });

  it("renders static when animation is disabled via prop", () => {
    const { container } = render(<AtomLogo animated={false} />);
    expect(container.querySelectorAll(".atom-orbit").length).toBe(0);
    expect(container.querySelector(".atom-nucleus")).toBeNull();
    expect(container.querySelector("svg")?.getAttribute("data-reduced-motion")).toBe(
      "true",
    );
  });

  it("renders static when reduced motion is preferred", () => {
    const { container } = render(<AtomLogo forceReducedMotion />);
    expect(container.querySelectorAll(".atom-orbit").length).toBe(0);
    expect(container.querySelector(".atom-nucleus")).toBeNull();
    expect(container.querySelector("svg")?.getAttribute("data-reduced-motion")).toBe(
      "true",
    );
  });
});
