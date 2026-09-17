import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { Button } from "@/components/ui/button";
import { TriggerDialog } from "../TriggerDialog";

function Harness({ onConfirm = vi.fn() }: { onConfirm?: (params: Record<string, string>) => void }) {
  const [open, setOpen] = useState(false);
  return (
    <TriggerDialog
      open={open}
      onOpenChange={setOpen}
      onConfirm={onConfirm}
      trigger={
        <Button size="sm" aria-label="Trigger job">
          Trigger
        </Button>
      }
    />
  );
}

describe("TriggerDialog", () => {
  it("restores focus to the trigger after Escape", async () => {
    render(<Harness />);

    const trigger = screen.getByRole("button", { name: "Trigger job" });
    trigger.focus();
    expect(trigger).toHaveFocus();
    fireEvent.click(trigger);

    const dialog = await screen.findByRole("dialog", { name: "Trigger Job" });
    expect(dialog).toBeInTheDocument();

    fireEvent.keyDown(dialog, { key: "Escape", code: "Escape" });

    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Trigger Job" })).not.toBeInTheDocument();
    });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Trigger job" })).toHaveFocus();
    });
  });
});
