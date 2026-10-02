/** Decode serialized argv, retaining argument boundaries with JSON quoting.
 * This is a display representation; the stored command remains available verbatim.
 */
export function formatCommand(command: string): string {
  try {
    const value: unknown = JSON.parse(command);
    if (Array.isArray(value) && value.every(arg => typeof arg === "string")) {
      return value.map(arg => /^[A-Za-z0-9_./:@%+=,-]+$/.test(arg) ? arg : JSON.stringify(arg)).join(" ");
    }
  } catch { /* Plain command strings are already readable. */ }
  return command;
}
