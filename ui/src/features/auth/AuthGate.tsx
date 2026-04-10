import { useState, useEffect, useCallback } from "react";
import { isAuthenticated, onAuthChange } from "@/lib/auth";
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

        const body = (await resp.json()) as { enabled?: boolean };
        if (!cancelled) {
          setAuthRequired(Boolean(body.enabled));
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
