import { IdChip } from "./id-chip";

/** Image names remain readable; immutable digests use the diagnostic copy chip. */
export function ImageReference({ image }: { image: string }) {
  const at = image.indexOf("@");
  return at < 0 ? <span title={image}>{image}</span> : <span className="inline-flex max-w-full items-center gap-2">
    <span className="truncate" title={image.slice(0, at)}>{image.slice(0, at)}</span>
    <IdChip value={image.slice(at + 1)} label="image digest" />
  </span>;
}
