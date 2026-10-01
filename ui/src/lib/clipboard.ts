/** Preserve copy access on plain HTTP deployments without the Clipboard API. */
export async function copyText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      // Browser policy can deny the modern API even when it is present.
    }
  }
  const focused = document.activeElement;
  const selection = document.getSelection();
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange()) : [];
  const text = document.createElement("textarea");
  text.value = value;
  text.readOnly = true;
  text.style.cssText = "position:fixed;left:-9999px;top:0";
  document.body.append(text);
  try {
    text.focus({ preventScroll: true });
    text.select();
    if (!document.execCommand?.("copy")) throw new Error("Clipboard unavailable");
  } finally {
    text.remove();
    if (focused instanceof HTMLElement) focused.focus({ preventScroll: true });
    selection?.removeAllRanges();
    ranges.forEach(range => selection?.addRange(range));
  }
}
