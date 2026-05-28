import { useState, useEffect, useCallback } from "react";
import {
  type AuthMethod,
  type AuthStatus,
  checkSession,
  isAuthenticated,
  onAuthChange,
} from "@/lib/auth";
import { LoginPage } from "./LoginPage";

interface AuthGateProps {
  children: React.ReactNode;
}

/**
 * AuthGate checks whether authentication is required using the explicit
 * `/auth/status` endpoint rather than inferring auth state from a protected
 * resource side-channel.
 */
export function AuthGate({ children }: AuthGateProps) {
  const [authRequired, setAuthRequired] = useState<boolean | null>(null);
  const [authMethods, setAuthMethods] = useState<AuthMethod[]>([]);
  const [authed, setAuthed] = useState(isAuthenticated);

  // Subscribe to auth state changes.
  useEffect(() => onAuthChange(() => setAuthed(isAuthenticated())), []);

  // Probe the server to determine if auth is enabled.
  useEffect(() => {
    let cancelled = false;

    async function probe() {
      try {
        const resp = await fetch("/auth/status");
        if (!resp.ok) {
          if (!cancelled) setAuthRequired(false);
          return;
        }

        const body = (await resp.json()) as AuthStatus;
        const enabled = Boolean(body.enabled);
        const methods = Array.isArray(body.methods) ? body.methods : [];
        const hasSession = enabled && !isAuthenticated() ? await checkSession() : false;
        if (!cancelled) {
          setAuthRequired(enabled);
          setAuthMethods(methods);
          if (hasSession) {
            setAuthed(true);
          }
        }
      } catch {
        // Network error — assume auth not required, let real errors surface later.
        if (!cancelled) setAuthRequired(false);
      }
    }

    probe();
    return () => {
      cancelled = true;
    };
  }, []);

  const handleLogin = useCallback(() => {
    setAuthed(true);
  }, []);

  // Still probing.
  if (authRequired === null) return null;

  // Auth is required but user hasn't authenticated yet.
  if (authRequired && !authed) {
    return <LoginPage methods={authMethods} onLogin={handleLogin} />;
  }

  return <>{children}</>;
}
