import { IdChip } from "./id-chip";

/** Chip rendering is an explicit field choice, independent of its label. */
export function MetadataValue({ value, label, idChip = false }: { value: string; label: string; idChip?: boolean }) {
  if (!value.trim()) return "None";
  return idChip ? <IdChip value={value} label={label} /> : value;
}
