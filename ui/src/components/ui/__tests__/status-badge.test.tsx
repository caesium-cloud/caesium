import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusBadge } from "../status-badge";
import { ALL_RUN_STATUSES } from "@/lib/status";

describe("<StatusBadge />", () => {
  it("renders every canonical status with the expected label", () => {
    for (const status of ALL_RUN_STATUSES) {
      const { container, unmount } = render(<StatusBadge status={status} />);
      const badge = container.querySelector(`[data-status="${status}"]`);
      expect(badge).not.toBeNull();
      expect(badge?.textContent).toContain(status);
      unmount();
    }
  });

  it("renders words by default and keeps glyph-only status accessible", () => {
    const { container, rerender } = render(<StatusBadge status="running" />);
    expect(container.querySelector('[data-variant="word"]')).not.toBeNull();
    expect(container.querySelector('[data-shape="ring"]')).not.toBeNull();
    rerender(<StatusBadge status="failed" variant="glyph" />);
    expect(container.querySelector('[data-variant="glyph"]')).toHaveAttribute("aria-label", "failed");
    expect(container.querySelector('[data-shape="failed"]')).not.toBeNull();
  });

  it("falls back to 'unknown' for unrecognized statuses", () => {
    render(<StatusBadge status="nonsense" />);
    expect(screen.getByText("unknown")).toBeInTheDocument();
  });

  it("respects an explicit label override", () => {
    render(<StatusBadge status="succeeded" label="Done" />);
    expect(screen.getByText("Done")).toBeInTheDocument();
  });

  it("renders an incident status in its own lifecycle", () => {
    render(<StatusBadge status="open" domain="incident" />);
    expect(screen.getByText("open")).toBeInTheDocument();
    expect(screen.getByText("open")).toHaveAttribute("data-status", "open");
  });

  it("keeps the canonical key when an incident label contains spaces", () => {
    render(<StatusBadge status="awaiting_approval" domain="incident" />);
    expect(screen.getByText("awaiting approval")).toHaveAttribute("data-status", "awaiting_approval");
  });
});
