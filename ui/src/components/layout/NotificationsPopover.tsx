import { Bell } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { Button } from "@/components/ui/button";

export function NotificationsPopover() {
  const [open, setOpen] = useState(false);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const panelId = useId();

  useEffect(() => {
    if (!open) return;

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      setOpen(false);
      buttonRef.current?.focus();
    };

    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (panelRef.current?.contains(target) || buttonRef.current?.contains(target)) {
        return;
      }
      setOpen(false);
    };

    document.addEventListener("keydown", onKeyDown);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [open]);

  return (
    <div className="relative">
      <Button
        ref={buttonRef}
        type="button"
        variant="ghost"
        size="icon"
        aria-label="Notifications"
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-controls={panelId}
        className="text-text-2 hover:text-text-1"
        onClick={() => setOpen((current) => !current)}
      >
        <Bell className="h-4 w-4" />
      </Button>
      {open ? (
        <div
          ref={panelRef}
          id={panelId}
          role="dialog"
          aria-label="Notifications"
          data-testid="notifications-panel"
          tabIndex={-1}
          className="absolute right-0 top-full z-50 mt-2 w-[min(20rem,calc(100vw-2rem))] rounded-md border border-border/70 bg-popover p-4 text-popover-foreground shadow-md"
        >
          <div className="text-sm font-medium text-text-1">Notifications</div>
          <p className="mt-2 text-xs leading-relaxed text-text-3">
            There is no in-console alert inbox yet. Notification channels and policies are configured via the API{" "}
            <code className="font-mono text-[11px] text-cyan-glow">/v1/notifications/channels</code>
            {" "}and{" "}
            <code className="font-mono text-[11px] text-cyan-glow">/v1/notifications/policies</code>.
          </p>
        </div>
      ) : null}
    </div>
  );
}
