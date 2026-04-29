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

  it("supports filled, soft, and dot variants", () => {
    const { container, rerender } = render(<StatusBadge status="running" variant="filled" />);
    expect(container.querySelector('[data-variant="filled"]')).not.toBeNull();

    rerender(<StatusBadge status="running" variant="soft" />);
    expect(container.querySelector('[data-variant="soft"]')).not.toBeNull();

    rerender(<StatusBadge status="running" variant="dot" />);
    // dot variant has no [data-variant] attribute on the wrapper
    expect(container.querySelector('[data-variant]')).toBeNull();
    expect(container.querySelector('[data-status="running"]')).not.toBeNull();
  });

  it("falls back to 'unknown' for unrecognized statuses", () => {
    render(<StatusBadge status="nonsense" />);
    expect(screen.getByText("unknown")).toBeInTheDocument();
  });

  it("respects an explicit label override", () => {
    render(<StatusBadge status="succeeded" label="Done" />);
    expect(screen.getByText("Done")).toBeInTheDocument();
  });
});
