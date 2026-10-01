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

it("deals seven voters 3/2/2, retains an unreachable voter, and marks quorum loss", () => {
  const voters = Array.from({ length: 7 }, (_, i) => ({ id: String(i), leader: i === 0, reachable: i !== 4 }));
  const { container } = render(<AtomLogo voters={voters} quorum="lost" />);
  expect([...container.querySelectorAll('.atom-orbit')].map(orbit => orbit.querySelectorAll('[data-voter]').length)).toEqual([3, 2, 2]);
  expect(container.querySelector('[data-voter="4"]')).toHaveAttribute('data-reachable', 'false');
  expect(container.querySelectorAll('ellipse[stroke-dasharray="14 12"]')).toHaveLength(3);
  expect(container.querySelector('.atom-nucleus')).toBeNull();
});

it("caps the drawing at nine without losing the actual voter count", () => {
  const voters = Array.from({ length: 12 }, (_, i) => ({ id: String(i), leader: false, reachable: null }));
  const { container } = render(<AtomLogo voters={voters} quorum="unknown" />);
  expect(container.querySelectorAll('[data-voter]')).toHaveLength(9);
  expect(container.querySelector('desc')).toHaveTextContent('12 voters. Cluster health unknown.');
  expect(container.querySelector('.atom-nucleus')).toBeNull();
});
