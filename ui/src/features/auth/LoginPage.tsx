import { useState, useCallback } from "react";
import { AtomLogo } from "@/components/brand/atom-logo";
import { UTCClock } from "@/components/ui/utc-clock";
import { Oscillator } from "@/components/ui/oscillator";
import {
  apiKeyLogin,
  type AuthMethod,
  authMethodKey,
  authMethodLabel,
  credentialLogin,
  isCredentialAuthMethod,
  isRedirectAuthMethod,
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
        const result = await apiKeyLogin(trimmed);
        if (result === "success") {
          onLogin();
          return;
        }
        if (result === "invalid") {
          setError("Invalid or expired API key");
          return;
        }
        if (result === "denied") {
          setError("API key does not have sufficient permissions");
          return;
        }
        setError("Unable to reach the server");
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
    <div className="relative flex min-h-screen items-center justify-center bg-void px-4 py-8 text-foreground">
      <Oscillator ambient className="pointer-events-none absolute left-0 top-1/2 w-full opacity-[.35]" />
      <div
        className="relative flex w-full max-w-[420px] flex-col gap-4"
      >
        <div className="flex flex-col items-center gap-3 pb-4">
          <AtomLogo size={180} />
          <h1 className="text-xl font-bold tracking-[.12em]">CAESIUM</h1>
          <p className="text-xs text-text-3">9 192 631 770 Hz</p>
        </div>
        <div className="space-y-4 rounded-lg border border-border bg-midnight p-6">
          <p className="text-sm text-text-2">caesium <span className="text-cyan">❯</span> login</p>

        {redirectMethods.length > 0 && (
          <div className="flex flex-col gap-2">
            {redirectMethods.map((method) => (
              <button
                key={authMethodKey(method)}
                type="button"
                onClick={() => handleSSOLogin(method.loginUrl)}
                className="inline-flex items-center justify-center gap-2 rounded-md border border-input bg-void px-4 py-2 font-normal text-foreground transition hover:bg-muted focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 focus:ring-offset-background"
              >

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
              aria-label="Username"
              placeholder="Username"
              autoFocus
              autoComplete="username"
              className="rounded-md border border-input bg-void px-3 py-2 text-sm text-foreground outline-none transition focus:border-primary"
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              aria-label="Password"
              placeholder="Password"
              autoComplete="current-password"
              className="rounded-md border border-input bg-void px-3 py-2 text-sm text-foreground outline-none transition focus:border-primary"
            />

            {credentialError && (
              <p className="m-0 text-[0.8rem] text-destructive">{credentialError}</p>
            )}

            <button
              type="submit"
              disabled={credentialLoading}
              className="rounded-md border-none bg-primary px-4 py-2 font-normal text-primary-foreground transition disabled:cursor-not-allowed disabled:opacity-60"
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
            aria-label="API key"
            placeholder="csk_live_..."
            autoFocus={!credentialMethod}
            autoComplete="off"
            className="rounded-md border border-input bg-void px-3 py-2 text-sm text-foreground outline-none transition focus:border-primary"
          />

          {error && (
            <p className="m-0 text-[0.8rem] text-destructive">{error}</p>
          )}

          <button
            type="submit"
            disabled={loading}
            className="rounded-md border-none bg-primary px-4 py-2 font-normal text-primary-foreground transition disabled:cursor-not-allowed disabled:opacity-60"
          >
            {loading ? "Verifying..." : "Sign In"}
          </button>
        </form>

        </div>
        <UTCClock className="justify-center" />
        <div className="text-center text-xs text-text-3">build unavailable</div>
        <p className="m-0 text-center text-xs text-text-3">
          Key is stored in memory only and cleared on tab close
        </p>
      </div>
    </div>
  );
}
