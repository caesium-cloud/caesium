import { useState, useCallback } from "react";
import { LogIn } from "lucide-react";
import {
  type AuthMethod,
  authMethodKey,
  authMethodLabel,
  credentialLogin,
  isCredentialAuthMethod,
  isRedirectAuthMethod,
  setApiKey,
  ssoLoginUrl,
} from "@/lib/auth";

interface LoginPageProps {
  methods?: AuthMethod[];
  navigate?: (url: string) => void;
  onLogin: () => void;
  returnTo?: () => string;
}

const defaultNavigate = (url: string) => {
  window.location.assign(url);
};

function credentialProviderName(method: AuthMethod): string {
  const label = authMethodLabel(method).trim();
  const provider = label.replace(/^sign in with\s+/i, "").trim();
  return provider || label || "credential provider";
}

export function LoginPage({
  methods = [],
  navigate = defaultNavigate,
  onLogin,
  returnTo,
}: LoginPageProps) {
  const [key, setKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [credentialError, setCredentialError] = useState<string | null>(null);
  const [credentialLoading, setCredentialLoading] = useState(false);
  const redirectMethods = methods.filter(isRedirectAuthMethod);
  const credentialMethod = methods.find(isCredentialAuthMethod);

  const handleSSOLogin = useCallback(
    (loginUrl: string) => {
      navigate(ssoLoginUrl(loginUrl, returnTo?.()));
    },
    [navigate, returnTo],
  );

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setError(null);

      const trimmed = key.trim();
      if (!trimmed) {
        setError("API key is required");
        return;
      }

      if (!trimmed.startsWith("csk_")) {
        setError("Invalid key format (expected csk_... prefix)");
        return;
      }

      setLoading(true);

      try {
        // Verify the key against a viewer-level REST endpoint so scoped keys can log in.
        const response = await fetch("/v1/jobs?limit=1", {
          headers: { Authorization: `Bearer ${trimmed}` },
        });

        if (response.status === 401) {
          setError("Invalid or expired API key");
          return;
        }

        if (response.status === 403) {
          setError("API key does not have sufficient permissions");
          return;
        }

        // Key is valid — store in memory and proceed.
        setApiKey(trimmed);
        onLogin();
      } catch {
        setError("Unable to reach the server");
      } finally {
        setLoading(false);
      }
    },
    [key, onLogin],
  );

  const handleCredentialSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (!credentialMethod) {
        return;
      }

      setCredentialError(null);

      const trimmedUsername = username.trim();
      if (!trimmedUsername || !password) {
        setCredentialError("Username and password are required");
        return;
      }

      setCredentialLoading(true);

      const result = await credentialLogin(credentialMethod.loginUrl, trimmedUsername, password);
      const providerName = credentialProviderName(credentialMethod);
      switch (result) {
        case "success":
          onLogin();
          break;
        case "invalid":
          setCredentialError("Invalid username or password");
          break;
        case "denied":
          setCredentialError(`${providerName} login is not allowed for this account`);
          break;
        case "network":
          setCredentialError("Unable to reach the server");
          break;
        default:
          setCredentialError(`Unable to sign in with ${providerName}`);
      }

      setCredentialLoading(false);
    },
    [credentialMethod, onLogin, password, username],
  );

  return (
    <div className="flex min-h-screen items-center justify-center bg-background text-foreground">
      <div
        className="flex w-full max-w-[400px] flex-col gap-4 rounded-lg border border-border bg-card p-8"
      >
        <h1 className="mb-2 text-center text-xl font-semibold">Caesium</h1>

        {redirectMethods.length > 0 && (
          <div className="flex flex-col gap-2">
            {redirectMethods.map((method) => (
              <button
                key={authMethodKey(method)}
                type="button"
                onClick={() => handleSSOLogin(method.loginUrl)}
                className="inline-flex items-center justify-center gap-2 rounded-md border border-input bg-background px-4 py-2 font-medium text-foreground transition hover:bg-muted focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 focus:ring-offset-background"
              >
                <LogIn className="h-4 w-4" aria-hidden="true" />
                <span>{authMethodLabel(method)}</span>
              </button>
            ))}
          </div>
        )}

        {credentialMethod && (
          <form onSubmit={handleCredentialSubmit} className="flex flex-col gap-3">
            <p className="text-center text-sm text-muted-foreground">
              Sign in with LDAP
            </p>
            <input
              type="text"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username"
              autoFocus
              autoComplete="username"
              className="rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground outline-none transition focus:border-primary"
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
              autoComplete="current-password"
              className="rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground outline-none transition focus:border-primary"
            />

            {credentialError && (
              <p className="m-0 text-[0.8rem] text-destructive">{credentialError}</p>
            )}

            <button
              type="submit"
              disabled={credentialLoading}
              className="rounded-md border-none bg-primary px-4 py-2 font-medium text-primary-foreground transition disabled:cursor-not-allowed disabled:opacity-60"
            >
              {credentialLoading ? "Signing in..." : authMethodLabel(credentialMethod)}
            </button>
          </form>
        )}

        <form onSubmit={handleSubmit} className="flex flex-col gap-3">
          <p className="text-center text-sm text-muted-foreground">
            Enter your API key to continue
          </p>

          <input
            type="password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            placeholder="csk_live_..."
            autoFocus={!credentialMethod}
            autoComplete="off"
            className="rounded-md border border-input bg-background px-3 py-2 font-mono text-sm text-foreground outline-none transition focus:border-primary"
          />

          {error && (
            <p className="m-0 text-[0.8rem] text-destructive">{error}</p>
          )}

          <button
            type="submit"
            disabled={loading}
            className="rounded-md border-none bg-primary px-4 py-2 font-medium text-primary-foreground transition disabled:cursor-not-allowed disabled:opacity-60"
          >
            {loading ? "Verifying..." : "Sign In"}
          </button>
        </form>

        <p className="m-0 text-center text-xs text-muted-foreground">
          Key is stored in memory only and cleared on tab close
        </p>
      </div>
    </div>
  );
}
