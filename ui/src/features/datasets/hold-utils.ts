import { api, type DatasetHold } from "@/lib/api";

export function holdSearch(
  hold: Pick<DatasetHold, "namespace" | "name" | "id">,
) {
  return { namespace: hold.namespace, name: hold.name, hold: hold.id };
}

export function parseHoldSkipReason(reason?: string) {
  if (!reason?.startsWith("dataset_hold:")) return null;
  const [identity, hold] = reason.slice("dataset_hold:".length).split(" hold=");
  const slash = identity.indexOf("/");
  if (slash < 0 || !identity.slice(slash + 1)) return null;
  return {
    namespace: identity.slice(0, slash),
    name: identity.slice(slash + 1),
    hold,
  };
}

// There is no hold-by-id read route. Restrict the paged history to its exact
// identity, then find the requested historical ID. Never fall back to whichever
// active hold happens to replace it while the operator is reviewing evidence.
export async function readIdentityHold(
  namespace: string,
  name: string,
  id?: string,
) {
  let offset = 0;
  while (true) {
    const page = await api.getDatasetHolds({
      namespace,
      name,
      status: id ? "all" : "active",
      limit: 200,
      offset,
    });
    const found = page.holds.find(
      (hold) =>
        hold.namespace === namespace &&
        hold.name === name &&
        (!id || hold.id === id),
    );
    if (found) return found;
    if (page.holds.length === 0 || offset + page.holds.length >= page.total)
      return null;
    offset += page.holds.length;
  }
}
