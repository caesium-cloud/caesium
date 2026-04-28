import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { EmptyState } from "../empty-state";

describe("<EmptyState />", () => {
  it("renders title + atom motif with no subtitle/action by default", () => {
    const { container } = render(<EmptyState title="Nothing here" />);
    expect(screen.getByText("Nothing here")).toBeInTheDocument();
    expect(container.querySelector("svg[aria-label='Caesium']")).not.toBeNull();
    expect(screen.getByRole("status")).toBeInTheDocument();
  });

  it("renders subtitle and action when provided", () => {
    render(
      <EmptyState
        title="No jobs"
        subtitle="Apply a job definition to get started."
        action={<button>Apply</button>}
      />,
    );
    expect(screen.getByText("Apply a job definition to get started.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply" })).toBeInTheDocument();
  });

  it("uses a static atom motif (no animation) so empty states stay calm", () => {
    const { container } = render(<EmptyState title="Empty" />);
    const svg = container.querySelector("svg[aria-label='Caesium']");
    expect(svg?.getAttribute("data-reduced-motion")).toBe("true");
  });

  it("accepts a custom icon override", () => {
    render(<EmptyState title="No data" icon={<span data-testid="custom" />} />);
    expect(screen.getByTestId("custom")).toBeInTheDocument();
  });
});
