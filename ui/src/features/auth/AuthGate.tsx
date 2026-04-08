import { useState, useEffect, useCallback } from "react";
import { isAuthenticated, onAuthChange } from "@/lib/auth";
import { LoginPage } from "./LoginPage";

interface AuthGateProps {
  children: React.ReactNode;
}

/**
 * AuthGate checks whether authentication is required by probing a lightweight
 * viewer-level REST endpoint.
 * If the server returns 401, it shows the login page. If auth is not enabled
 * (server returns 2xx without credentials), it passes through directly.
 */
export function AuthGate({ children }: AuthGateProps) {
  const [authRequired, setAuthRequired] = useState<boolean | null>(null);
  const [authed, setAuthed] = useState(isAuthenticated);

  // Subscribe to auth state changes.
  useEffect(() => onAuthChange(() => setAuthed(isAuthenticated())), []);

  // Probe the server to determine if auth is enabled.
  useEffect(() => {
    let cancelled = false;

    async function probe() {
      try {
        const resp = await fetch("/v1/jobs?limit=1");
        if (!cancelled) {
          setAuthRequired(resp.status === 401);
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
    return <LoginPage onLogin={handleLogin} />;
  }

  return <>{children}</>;
}
