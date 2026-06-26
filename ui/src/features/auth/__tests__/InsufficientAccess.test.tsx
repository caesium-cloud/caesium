import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { InsufficientAccess } from "../InsufficientAccess";

describe("InsufficientAccess", () => {
  it("renders an explicit message without an error object", () => {
    render(<InsufficientAccess message="This view requires a runner key." />);

    expect(screen.getByRole("alert")).toHaveTextContent(
      "This view requires a runner key.",
    );
  });

  it("renders the default message for insufficient-access errors", () => {
    render(
      <InsufficientAccess error={{ status: 403, kind: "insufficient_access" }} />,
    );

    expect(screen.getByRole("alert")).toHaveTextContent(
      "Your key does not have permission for this action.",
    );
  });

  it("renders nothing for unrelated errors without an explicit message", () => {
    const { container } = render(
      <InsufficientAccess error={{ status: 403, kind: "other" }} />,
    );

    expect(container).toBeEmptyDOMElement();
  });
});
