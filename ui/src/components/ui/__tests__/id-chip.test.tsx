import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { IdChip } from "../id-chip";

afterEach(() => vi.useRealTimers());

it("copies the full identifier without following its enclosing link, then clears confirmation", async () => {
  vi.useFakeTimers();
  const value = "sha256:0123456789abcdef";
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const navigate = vi.fn();
  render(<a href="/another-run" onClick={navigate}><IdChip value={value} label="digest" /></a>);
  const chip = screen.getByRole("button", { name: `Copy digest: ${value}` });
  expect(chip).toHaveAttribute("title", value);
  expect(chip).toHaveTextContent("01234567");
  await act(async () => { fireEvent.click(chip); });
  expect(writeText).toHaveBeenCalledWith(value);
  expect(navigate).not.toHaveBeenCalled();
  expect(screen.getByRole("status")).toHaveTextContent("Copied");
  act(() => { vi.advanceTimersByTime(1000); });
  expect(screen.queryByRole("status")).toBeNull();
});
