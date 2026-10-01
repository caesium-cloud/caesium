import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { IdChip } from "../id-chip";

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

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

it("copies the full identifier through the fallback when the Clipboard API is absent", async () => {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  const copied: string[] = [];
  Object.defineProperty(document, "execCommand", { configurable: true, value: vi.fn(() => {
    copied.push((document.activeElement as HTMLTextAreaElement).value);
    return true;
  }) });
  render(<IdChip value="Complete-CaseSensitive-Identifier" label="run id" />);
  const chip = screen.getByRole("button", { name: /Copy run id/ });
  chip.focus();
  await act(async () => { fireEvent.click(chip); });
  expect(copied).toEqual(["Complete-CaseSensitive-Identifier"]);
  expect(screen.getByRole("status")).toHaveTextContent("Copied");
  expect(chip).toHaveFocus();
  expect(document.querySelector("textarea")).toBeNull();
});

it("keeps the full value selectable if both copy mechanisms are denied", async () => {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) } });
  Object.defineProperty(document, "execCommand", { configurable: true, value: vi.fn(() => false) });
  const value = "complete-digest-not-only-eight-characters";
  render(<IdChip value={value} label="digest" />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: /Copy digest/ })); });
  const text = screen.getByRole("textbox", { name: "Full digest" }) as HTMLInputElement;
  expect(text).toHaveValue(value);
  expect(text).toHaveFocus();
  expect(text.selectionEnd).toBe(value.length);
  expect(screen.queryByText("Copied")).not.toBeInTheDocument();
});
