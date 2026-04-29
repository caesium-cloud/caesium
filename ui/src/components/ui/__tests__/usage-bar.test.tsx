import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { UsageBar } from "../usage-bar";
import { USAGE_THRESHOLDS } from "@/lib/thresholds";

describe("<UsageBar />", () => {
  it("tints below the warn threshold as 'ok'", () => {
    const { container } = render(<UsageBar value={USAGE_THRESHOLDS.warn - 1} label="CPU" />);
    expect(container.querySelector("[data-level='ok']")).not.toBeNull();
  });

  it("tints between warn and danger as 'warn'", () => {
    const { container } = render(<UsageBar value={USAGE_THRESHOLDS.warn} />);
    expect(container.querySelector("[data-level='warn']")).not.toBeNull();
    const { container: c2 } = render(<UsageBar value={USAGE_THRESHOLDS.danger - 1} />);
    expect(c2.querySelector("[data-level='warn']")).not.toBeNull();
  });

  it("tints at or above danger as 'danger'", () => {
    const { container } = render(<UsageBar value={USAGE_THRESHOLDS.danger} />);
    expect(container.querySelector("[data-level='danger']")).not.toBeNull();
    const { container: c2 } = render(<UsageBar value={120} />);
    expect(c2.querySelector("[data-level='danger']")).not.toBeNull();
  });

  it("clamps out-of-range values into [0, 100]", () => {
    render(<UsageBar value={-50} label="CPU" />);
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("0");

    render(<UsageBar value={500} label="MEM" />);
    const bars = screen.getAllByRole("progressbar");
    expect(bars[bars.length - 1].getAttribute("aria-valuenow")).toBe("100");
  });

  it("exposes label + value to assistive tech", () => {
    render(<UsageBar value={42.7} label="CPU" />);
    const bar = screen.getByRole("progressbar", { name: "CPU" });
    expect(bar.getAttribute("aria-valuenow")).toBe("42.7");
    expect(bar.getAttribute("aria-valuetext")).toBe("43 percent");
  });
});
