import { useCallback, useEffect, useMemo, useSyncExternalStore } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { CLUSTER_NAMESPACE, roleAtLeast } from "@/lib/roles";
import { usePrincipal, type PrincipalState } from "@/lib/auth";
import { isAnonymous } from "./access";

/**
 * The namespace view filter (identity-and-access F2, design section 14): the
 * namespace the person chose in the header switcher. It only NARROWS what the
 * server already returns for their grants; it never widens it, and it is never
 * a credential. Persisted in localStorage as a convenience (every access is
 * guarded: storage can be absent or throw), falling back to memory.
 */

const STORAGE_KEY = "caesium.namespaceView";
const DNS_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const DEFAULT_NAMESPACE = "default";

/** A stored or typed namespace is only trusted when it is a DNS label. */
export function parseNamespace(value: unknown): string | null {
  return typeof value === "string" && DNS_LABEL.test(value) ? value : null;
}

let current: string | null = null;
let loaded = false;
const listeners = new Set<() => void>();

function readStorage(): string | null {
  try {
    return parseNamespace(window.localStorage.getItem(STORAGE_KEY));
  } catch {
    return null;
  }
}

function ensureLoaded(): void {
  if (!loaded) {
    loaded = true;
    current = readStorage();
    if (typeof window !== "undefined") {
      window.addEventListener("storage", (event) => {
        if (event.key === STORAGE_KEY || event.key === null) {
          current = readStorage();
          listeners.forEach((fn) => fn());
        }
      });
    }
  }
}

function getSnapshot(): string | null {
  ensureLoaded();
  return current;
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Choose a namespace (null shows all). Exported for tests and the switcher. */
export function setStoredNamespace(namespace: string | null): void {
  ensureLoaded();
  current = parseNamespace(namespace);
  try {
    if (current) {
      window.localStorage.setItem(STORAGE_KEY, current);
    } else {
      window.localStorage.removeItem(STORAGE_KEY);
    }
  } catch {
    // Storage is a convenience: the in-memory selection still applies.
  }
  listeners.forEach((fn) => fn());
}

/** Test seam: forget the loaded selection so the next read hits storage again. */
export function resetNamespaceViewForTests(): void {
  loaded = false;
  current = null;
  listeners.clear();
}

function sortNamespaces(names: Iterable<string>): string[] {
  return [...new Set(names)].sort((a, b) => {
    if (a === DEFAULT_NAMESPACE) return b === DEFAULT_NAMESPACE ? 0 : -1;
    if (b === DEFAULT_NAMESPACE) return 1;
    return a.localeCompare(b);
  });
}

/**
 * The namespaces the switcher offers. A confined principal gets its own list;
 * a cluster-wide one (or authentication disabled) also gets the declared
 * namespaces and those of the jobs it can read.
 */
export function namespaceOptions(input: {
  principal: Pick<PrincipalState, "namespaces" | "kind" | "subject" | "role">;
  declared?: readonly string[];
  jobNamespaces?: readonly (string | undefined)[];
}): string[] {
  const { principal } = input;
  const explicit = principal.namespaces.filter((n) => n !== CLUSTER_NAMESPACE && parseNamespace(n));
  const wide = principal.namespaces.includes(CLUSTER_NAMESPACE) || isAnonymous(principal);
  const names = new Set(explicit);
  if (wide) {
    input.declared?.forEach((n) => parseNamespace(n) && names.add(n));
    input.jobNamespaces?.forEach((n) => n && parseNamespace(n) && names.add(n));
  }
  return sortNamespaces(names);
}

/**
 * The selection that applies: the stored one when the principal can see it.
 * While the options are still loading the stored value is used as is (the
 * server narrows within the grants, so a stale value only yields an empty
 * page until the options settle and it is dropped).
 */
export function effectiveSelection(stored: string | null, options: readonly string[], ready: boolean): string | null {
  if (!stored) return null;
  if (!ready) return stored;
  return options.includes(stored) ? stored : null;
}

/** Narrow items client-side (lists the server cannot narrow). */
export function filterByNamespace<T extends { namespace?: string }>(items: readonly T[], selection: string | null): T[] {
  if (!selection) return [...items];
  return items.filter((item) => (item.namespace?.trim() || DEFAULT_NAMESPACE) === selection);
}

/** The react-query key of the jobs list narrowed to `selection`. */
export function scopedKey(base: "jobs" | "triggers", selection: string | null): readonly string[] {
  return selection ? [base, "namespace", selection] : [base];
}

export interface NamespaceView {
  /** The applied selection; null shows every namespace the principal may read. */
  selection: string | null;
  /** Namespaces the switcher offers. */
  options: string[];
  /** The switcher is shown only when there is something to switch between. */
  showSwitcher: boolean;
  select: (namespace: string | null) => void;
}

export function useNamespaceView(): NamespaceView {
  const principal = usePrincipal();
  const stored = useSyncExternalStore(subscribe, getSnapshot, () => null);
  const anonymous = isAnonymous(principal);
  const clusterWide = principal.namespaces.includes(CLUSTER_NAMESPACE) || anonymous;
  // The declared-namespaces route is cluster-viewer only and denied to a
  // job-scoped key (AccessPage's rule): a scoped principal derives its options
  // from the jobs it can read and never requests it.
  const mayReadDeclared = !anonymous && !principal.isScoped && roleAtLeast(principal.clusterRole, "viewer");

  const declared = useQuery({
    queryKey: ["auth", "namespaces"],
    queryFn: api.getAuthNamespaces,
    enabled: clusterWide && mayReadDeclared,
    staleTime: 60_000,
    retry: false,
  });
  const jobs = useQuery({
    queryKey: ["jobs"],
    queryFn: () => api.getJobs(),
    enabled: clusterWide || principal.isScoped,
    staleTime: 30_000,
  });

  // Gate on the CURRENT principal: a disabled query still serves cached data
  // from a previous credential sharing this QueryClient.
  const declaredNames = clusterWide && mayReadDeclared ? declared.data?.namespaces.map((n) => n.name) : undefined;
  const jobNamespaces = clusterWide || principal.isScoped ? jobs.data?.map((job) => job.namespace) : undefined;
  const options = useMemo(
    () => namespaceOptions({ principal, declared: declaredNames, jobNamespaces }),
    [principal, declaredNames, jobNamespaces],
  );
  const ready =
    !(clusterWide || principal.isScoped) ||
    (!jobs.isPending && (!(clusterWide && mayReadDeclared) || !declared.isPending));
  const showSwitcher = options.length > 1;
  // The switcher is the only control that clears a selection, so a selection
  // must never outlive it: once the options settle at one namespace (or none)
  // nothing is applied and the stored value is forgotten. A failed options
  // query settles to an empty list, which is not evidence the grants shrank,
  // so the stored value is left alone in that case.
  const optionsFailed = jobs.isError || declared.isError;
  const hidden = ready && !showSwitcher;
  const selection = hidden ? null : effectiveSelection(stored, options, ready);
  const select = useCallback((namespace: string | null) => setStoredNamespace(namespace), []);

  useEffect(() => {
    if (hidden && !optionsFailed && stored) setStoredNamespace(null);
  }, [hidden, optionsFailed, stored]);

  return { selection, options, showSwitcher, select };
}
