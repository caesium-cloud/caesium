import { Link } from "@tanstack/react-router";
import { parseHoldSkipReason } from "./hold-utils";
import { useDataAssertionsEnabled } from "./useDataAssertions";

export function HoldSkipReason({ reason }: { reason?: string }) {
  const enabled = useDataAssertionsEnabled();
  const identity = parseHoldSkipReason(reason);
  if (!enabled || !identity) return null;
  return (
    <p
      className="rounded border border-fuchsia-400/40 bg-fuchsia-400/10 px-3 py-2 text-xs text-text-2"
      data-testid="hold-skip-reason"
    >
      Skipped because dataset{" "}
      <Link
        to="/datasets/holds"
        search={identity}
        className="font-mono text-fuchsia-300 hover:underline"
      >
        {identity.namespace ? `${identity.namespace}/` : ""}
        {identity.name}
      </Link>{" "}
      was held at admission. Releasing a hold permits the next trigger; it does
      not restart this skipped run.
    </p>
  );
}
