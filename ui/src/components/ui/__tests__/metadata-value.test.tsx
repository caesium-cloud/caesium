import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { MetadataValue } from "../metadata-value";

it("keeps chip rendering independent of display labels", () => {
  const { rerender } = render(<MetadataValue label="identity" value="ABCDEF012345" idChip />);
  expect(screen.getByRole("button", { name: "Copy identity: ABCDEF012345" })).toBeInTheDocument();
  rerender(<MetadataValue label="renamed" value="ABCDEF012345" idChip />);
  expect(screen.getByRole("button", { name: "Copy renamed: ABCDEF012345" })).toBeInTheDocument();
  rerender(<MetadataValue label="Task ID" value="plain-value" />);
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.getByText("plain-value")).toBeInTheDocument();
});

it.each(["", "   "])("keeps missing value %s as plain text", value => {
  render(<MetadataValue label="identity" value={value} idChip />);
  expect(screen.getByText("None")).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it.each(["none", "None", " NONE "])("preserves literal value %s", value => {
  const { rerender } = render(<MetadataValue label="expected" value={value} />);
  expect(screen.getByText(value.trim())).toBeInTheDocument();
  rerender(<MetadataValue label="identity" value={value} idChip />);
  expect(screen.getByRole("button", { name: `Copy identity: ${value.trim()}` })).toHaveAttribute("title", value);
});
