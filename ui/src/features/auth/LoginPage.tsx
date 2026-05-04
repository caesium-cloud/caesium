import { useState, useCallback } from "react";
import { setApiKey } from "@/lib/auth";

interface LoginPageProps {
  onLogin: () => void;
}

export function LoginPage({ onLogin }: LoginPageProps) {
  const [key, setKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

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

  return (
    <div className="flex min-h-screen items-center justify-center bg-background text-foreground">
      <form
        onSubmit={handleSubmit}
        className="flex w-full max-w-[400px] flex-col gap-4 rounded-lg border border-border bg-card p-8"
      >
        <h1 className="mb-2 text-center text-xl font-semibold">Caesium</h1>

        <p className="text-center text-sm text-muted-foreground">
          Enter your API key to continue
        </p>

        <input
          type="password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder="csk_live_..."
          autoFocus
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

        <p className="m-0 text-center text-xs text-muted-foreground">
          Key is stored in memory only and cleared on tab close
        </p>
      </form>
    </div>
  );
}
