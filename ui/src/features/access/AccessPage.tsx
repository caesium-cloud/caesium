import { useState } from "react";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { PageHeader } from "@/components/ui/page-header";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api, type AuthNamespace, type AuthUser } from "@/lib/api";
import { usePrincipal, type PrincipalState } from "@/lib/auth";
import { CLUSTER_NAMESPACE, roleAtLeast } from "@/lib/roles";
import { formatUTCTimestamp } from "@/lib/utils";

/** Authentication is disabled when no principal was ever resolved. */
function isAuthDisabled(principal: PrincipalState): boolean {
  return principal.kind === null && principal.subject === null && principal.role === null;
}

function Section({ title, hint, testId, children }: { title: string; hint?: string; testId: string; children: React.ReactNode }) {
  return (
    <section data-testid={testId} className="space-y-3">
      <div>
        <h2 className="text-sm font-bold text-text-1">{title}</h2>
        {hint ? <p className="text-xs text-text-3">{hint}</p> : null}
      </div>
      {children}
    </section>
  );
}

export function AccessPage() {
  const principal = usePrincipal();
  const authDisabled = isAuthDisabled(principal);
  // A job-scoped key is denied the cluster routes, so it sees only its own access.
  const unscoped = !principal.isScoped;
  const canReadNamespaces = !authDisabled && unscoped && roleAtLeast(principal.clusterRole, "viewer");
  const isClusterAdmin = !authDisabled && unscoped && principal.clusterRole === "admin";

  return (
    <div className="space-y-8">
      <PageHeader
        title="access"
        description="Who you are signed in as, what you may do, and how this cluster is partitioned."
      />
      <MyAccess principal={principal} authDisabled={authDisabled} />
      {canReadNamespaces ? <NamespacesSection /> : null}
      {isClusterAdmin ? <PolicySection /> : null}
      {isClusterAdmin ? <UsersSection /> : null}
    </div>
  );
}

function MyAccess({ principal, authDisabled }: { principal: PrincipalState; authDisabled: boolean }) {
  if (authDisabled) {
    return (
      <Section title="My access" testId="access-mine">
        <p role="status" className="text-sm text-text-2" data-testid="access-auth-disabled">
          Authentication is disabled on this server, so every action is allowed and there are no grants to show.
        </p>
      </Section>
    );
  }
  const grants = Object.entries(principal.grants).sort(([a], [b]) => {
    if (a === CLUSTER_NAMESPACE) return -1;
    if (b === CLUSTER_NAMESPACE) return 1;
    return a.localeCompare(b);
  });
  return (
    <Section title="My access" testId="access-mine">
      <dl className="grid max-w-xl grid-cols-[120px_minmax(0,1fr)] gap-x-4 gap-y-1.5 text-sm">
        <dt className="text-text-3">subject</dt>
        <dd data-testid="access-subject" className="min-w-0 break-all text-text-1">{principal.subject ?? "unknown"}</dd>
        <dt className="text-text-3">kind</dt>
        <dd data-testid="access-kind" className="text-text-1">{principal.kind ?? "unknown"}</dd>
        <dt className="text-text-3">cluster role</dt>
        <dd data-testid="access-cluster-role" className="text-text-1">{principal.clusterRole ?? "none"}</dd>
      </dl>
      {principal.isScoped ? (
        <p className="text-xs text-text-3" data-testid="access-scoped-note">
          This key is scoped to specific jobs, so cluster pages are not available to it.
        </p>
      ) : null}
      {grants.length === 0 ? (
        <p className="text-sm text-text-2" data-testid="access-no-grants">You hold no grants in any namespace.</p>
      ) : (
        <Table data-testid="access-grants">
          <TableHeader>
            <TableRow>
              <TableHead>namespace</TableHead>
              <TableHead>role</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {grants.map(([namespace, role]) => (
              <TableRow key={namespace} data-testid="access-grant-row">
                <TableCell>{namespace === CLUSTER_NAMESPACE ? "all namespaces" : namespace}</TableCell>
                <TableCell>{role}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Section>
  );
}

function describeNamespace(ns: AuthNamespace): string[] {
  const parts: string[] = [];
  if (ns.kubernetes?.namespace || ns.kubernetes?.serviceAccountName) {
    parts.push(
      `kubernetes: ${[ns.kubernetes.namespace, ns.kubernetes.serviceAccountName].filter(Boolean).join(" / ")}`,
    );
  }
  if (ns.secrets) {
    parts.push(
      ns.secrets.allow.length === 0 ? "secrets: none allowed" : `secrets: ${ns.secrets.allow.join(", ")}`,
    );
  }
  if (ns.quotas?.maxConcurrentRuns) {
    parts.push(`max concurrent runs: ${ns.quotas.maxConcurrentRuns}`);
  }
  return parts;
}

function shortRevision(revision: string): string {
  return revision ? revision.slice(0, 12) : "none (no policy file)";
}

function NamespacesSection() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["auth", "namespaces"],
    queryFn: api.getAuthNamespaces,
  });
  return (
    <Section
      title="Namespaces"
      hint="Declared namespaces and their isolation settings."
      testId="access-namespaces"
    >
      {isLoading ? <Skeleton className="h-16 w-full" /> : null}
      {error ? <p role="alert" className="text-sm text-danger">Unable to load namespaces.</p> : null}
      {data ? (
        <>
          <p className="text-xs text-text-3" data-testid="access-namespaces-revision">
            policy source {data.source}, revision {shortRevision(data.revision)}
          </p>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>namespace</TableHead>
                <TableHead>settings</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.namespaces.map((ns) => {
                const settings = describeNamespace(ns);
                return (
                  <TableRow key={ns.name} data-testid="access-namespace-row">
                    <TableCell className="font-bold">{ns.name}</TableCell>
                    <TableCell className="text-text-2">
                      {settings.length === 0 ? "no settings" : settings.join("; ")}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </>
      ) : null}
    </Section>
  );
}

function PolicySection() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["auth", "policy"],
    queryFn: api.getAuthPolicy,
  });
  const last = data?.last_reload ?? null;
  return (
    <Section title="Policy" hint="The access policy this node is serving." testId="access-policy">
      {isLoading ? <Skeleton className="h-12 w-full" /> : null}
      {error ? <p role="alert" className="text-sm text-danger">Unable to load the access policy.</p> : null}
      {data ? (
        <dl className="grid max-w-xl grid-cols-[120px_minmax(0,1fr)] gap-x-4 gap-y-1.5 text-sm">
          <dt className="text-text-3">source</dt>
          <dd className="text-text-1">{data.source}</dd>
          <dt className="text-text-3">revision</dt>
          <dd data-testid="access-policy-revision" className="break-all font-mono text-text-1">
            {data.revision || "none (no policy file)"}
          </dd>
          <dt className="text-text-3">loaded</dt>
          <dd className="text-text-1">{formatUTCTimestamp(data.loaded_at)}</dd>
          <dt className="text-text-3">last reload</dt>
          <dd data-testid="access-policy-last-reload" className="text-text-1">
            {last
              ? `${last.ok ? (last.changed ? "reloaded" : "unchanged") : "failed"} (${last.trigger}) at ${formatUTCTimestamp(last.at)}${last.error ? `: ${last.error}` : ""}`
              : "no reload attempted"}
          </dd>
        </dl>
      ) : null}
    </Section>
  );
}

type UserAction = "disable" | "enable" | "revoke";

const actionCopy: Record<UserAction, { title: string; body: string; confirm: string }> = {
  disable: {
    title: "Disable user",
    body: "The user can no longer sign in, and every active session is revoked immediately.",
    confirm: "Disable user",
  },
  enable: {
    title: "Enable user",
    body: "The user can sign in again. Revoked sessions stay revoked.",
    confirm: "Enable user",
  },
  revoke: {
    title: "Log out everywhere",
    body: "Every active session of this user is revoked. The user can sign in again.",
    confirm: "Log out everywhere",
  },
};

function UsersSection() {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<{ user: AuthUser; action: UserAction } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const users = useInfiniteQuery({
    queryKey: ["auth", "users"],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.getAuthUsers(pageParam),
    getNextPageParam: (last) => last.nextOffset ?? undefined,
  });

  const mutation = useMutation({
    mutationFn: ({ user, action }: { user: AuthUser; action: UserAction }): Promise<unknown> => {
      switch (action) {
        case "disable":
          return api.disableAuthUser(user.id);
        case "enable":
          return api.enableAuthUser(user.id);
        default:
          return api.revokeAuthUserSessions(user.id);
      }
    },
    onSuccess: (_result, { user, action }) => {
      setPending(null);
      setActionError(null);
      toast.success(`${actionCopy[action].title}: ${user.email || user.subject}`);
      queryClient.invalidateQueries({ queryKey: ["auth", "users"] });
    },
    onError: (err: Error) => {
      setActionError(err.message);
    },
  });

  const rows = users.data?.pages.flatMap((page) => page.users) ?? [];
  const total = users.data?.pages[0]?.total ?? null;

  return (
    <Section
      title="Users"
      hint="People who signed in through SSO. Bindings are managed in the policy file."
      testId="access-users"
    >
      {users.isLoading ? <Skeleton className="h-16 w-full" /> : null}
      {users.error ? <p role="alert" className="text-sm text-danger">Unable to load users.</p> : null}
      {!users.isLoading && !users.error && rows.length === 0 ? (
        <p className="text-sm text-text-2" data-testid="access-users-empty">No users have signed in yet.</p>
      ) : null}
      {rows.length > 0 ? (
        <>
          {total !== null ? <p className="text-xs text-text-3">{total} total</p> : null}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>user</TableHead>
                <TableHead>last login</TableHead>
                <TableHead>status</TableHead>
                <TableHead className="text-right">actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((user) => {
                const disabled = user.disabled_at !== null;
                return (
                  <TableRow key={user.id} data-testid="access-user-row">
                    <TableCell>
                      <div className="font-bold">{user.email || user.subject}</div>
                      <div className="text-[11px] text-text-3">{user.issuer}</div>
                    </TableCell>
                    <TableCell className="text-text-2">
                      {user.last_login_at ? formatUTCTimestamp(user.last_login_at) : "never"}
                    </TableCell>
                    <TableCell>{disabled ? "disabled" : "active"}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          size="sm"
                          variant="outline"
                          className="h-7 px-2 text-xs"
                          onClick={() => {
                            setActionError(null);
                            setPending({ user, action: disabled ? "enable" : "disable" });
                          }}
                        >
                          {disabled ? "Enable" : "Disable"}
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          className="h-7 px-2 text-xs"
                          onClick={() => {
                            setActionError(null);
                            setPending({ user, action: "revoke" });
                          }}
                        >
                          Log out everywhere
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          {users.hasNextPage ? (
            <Button
              variant="outline"
              size="sm"
              onClick={() => users.fetchNextPage()}
              disabled={users.isFetchingNextPage}
            >
              {users.isFetchingNextPage ? "Loading..." : "Load more"}
            </Button>
          ) : null}
        </>
      ) : null}

      <Dialog
        open={pending !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setPending(null);
        }}
      >
        {pending ? (
          <DialogContent className="bg-midnight border-graphite/50 p-4 text-text-1 sm:max-w-[425px] sm:rounded-md sm:p-6">
            <DialogHeader>
              <DialogTitle className="text-lg">{actionCopy[pending.action].title}</DialogTitle>
              <DialogDescription className="mt-1.5 text-text-3">
                {pending.user.email || pending.user.subject}. {actionCopy[pending.action].body}
              </DialogDescription>
            </DialogHeader>
            {actionError ? <p role="alert" className="text-sm text-danger">{actionError}</p> : null}
            <DialogFooter className="mt-4 gap-2 sm:gap-0">
              <Button variant="outline" onClick={() => setPending(null)} disabled={mutation.isPending}>
                Cancel
              </Button>
              <Button
                data-testid="access-confirm"
                onClick={() => mutation.mutate(pending)}
                disabled={mutation.isPending}
              >
                {mutation.isPending ? "Working..." : actionCopy[pending.action].confirm}
              </Button>
            </DialogFooter>
          </DialogContent>
        ) : null}
      </Dialog>
    </Section>
  );
}
