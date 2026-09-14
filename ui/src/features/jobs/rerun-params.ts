/** Re-run copies business inputs; scheduler provenance belongs to the new run. */
export function rerunParams(params?: Record<string, string>): Record<string, string> | undefined {
  const entries = Object.entries(params ?? {}).filter(
    ([key]) => !key.startsWith("_") && key !== "logical_date",
  );
  return entries.length > 0 ? Object.fromEntries(entries) : undefined;
}
