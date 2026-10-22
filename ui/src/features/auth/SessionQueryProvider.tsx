import { useState, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { usePrincipal, type PrincipalState } from "@/lib/auth";

function identityKey(principal: PrincipalState): string {
  if (principal.kind === null && principal.subject === null && principal.role === null) {
    return "anonymous";
  }
  return `${principal.kind}:${principal.stableId ?? principal.subject}`;
}

function SessionBoundary({ children, create }: { children: ReactNode; create: () => QueryClient }) {
  const [client] = useState(create);
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/**
 * One QueryClient per signed-in identity, and a fresh subtree with it. A shared
 * client outlives the credential, so a list, a declared-namespace read or an
 * in-flight mutation response of one principal could be served to (or written
 * into the cache of) the next principal in the same tab. Keying the boundary by
 * identity remounts every query and mutation observer too: observers keep the
 * client they were created with, so swapping only the provider's client would
 * leave the previous session's observers writing into, and reading from, the
 * wrong cache.
 */
export function SessionQueryProvider({
  children,
  create,
}: {
  children: ReactNode;
  create: () => QueryClient;
}) {
  const key = identityKey(usePrincipal());
  return (
    <SessionBoundary key={key} create={create}>
      {children}
    </SessionBoundary>
  );
}
