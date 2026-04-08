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
    <div
      style={{
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        minHeight: "100vh",
        background: "var(--background, #09090b)",
        color: "var(--foreground, #fafafa)",
      }}
    >
      <form
        onSubmit={handleSubmit}
        style={{
          display: "flex",
          flexDirection: "column",
          gap: "1rem",
          width: "100%",
          maxWidth: "400px",
          padding: "2rem",
          border: "1px solid var(--border, #27272a)",
          borderRadius: "0.5rem",
          background: "var(--card, #0a0a0a)",
        }}
      >
        <h1
          style={{
            fontSize: "1.25rem",
            fontWeight: 600,
            textAlign: "center",
            marginBottom: "0.5rem",
          }}
        >
          Caesium
        </h1>

        <p
          style={{
            fontSize: "0.875rem",
            textAlign: "center",
            opacity: 0.7,
          }}
        >
          Enter your API key to continue
        </p>

        <input
          type="password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder="csk_live_..."
          autoFocus
          autoComplete="off"
          style={{
            padding: "0.5rem 0.75rem",
            borderRadius: "0.375rem",
            border: "1px solid var(--border, #27272a)",
            background: "var(--input, #09090b)",
            color: "inherit",
            fontSize: "0.875rem",
            fontFamily: "monospace",
          }}
        />

        {error && (
          <p
            style={{
              fontSize: "0.8rem",
              color: "var(--destructive, #ef4444)",
              margin: 0,
            }}
          >
            {error}
          </p>
        )}

        <button
          type="submit"
          disabled={loading}
          style={{
            padding: "0.5rem 1rem",
            borderRadius: "0.375rem",
            border: "none",
            background: "var(--primary, #fafafa)",
            color: "var(--primary-foreground, #09090b)",
            fontWeight: 500,
            cursor: loading ? "not-allowed" : "pointer",
            opacity: loading ? 0.6 : 1,
          }}
        >
          {loading ? "Verifying..." : "Sign In"}
        </button>

        <p
          style={{
            fontSize: "0.75rem",
            textAlign: "center",
            opacity: 0.5,
            margin: 0,
          }}
        >
          Key is stored in memory only and cleared on tab close
        </p>
      </form>
    </div>
  );
}
