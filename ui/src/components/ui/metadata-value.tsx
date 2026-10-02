import { IdChip } from "./id-chip";

/** Chip rendering is an explicit field choice, independent of its label. */
export function MetadataValue({ value, label, idChip = false }: { value: string; label: string; idChip?: boolean }) {
  const display = value.trim() && value.trim().toLowerCase() !== "none" ? value : "None";
  return idChip && display !== "None" ? <IdChip value={display} label={label} /> : display;
}
