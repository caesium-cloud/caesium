import { expect, test } from "@playwright/test";
import {
  authHeaders,
  loginAsRunner,
  loginAsViewer,
  obtainAuthKeys,
  type AuthLaneKeys,
} from "../helpers/auth";

// Smoke for the auth-ENABLED e2e lane. It proves the harness works and distinguishes
// roles:
//   - viewer and runner keys log in through the real UI and resolve their principal
//     via GET /auth/whoami (200).
//   - the job-SCOPED key also resolves its principal: whoami is IDENTITY, not
//     resource access, so the scope middleware (api/middleware/auth_scope.go
//     `authorizeScope`, case "/auth/whoami") allows every authenticated API-key
//     principal through, and the response names the jobs the key is scoped to.
//     This job is the ONLY place that assertion runs, which is why ui-e2e-auth is
//     a required check.
//
// Passing the whoami gate grants identity ONLY — the scoped key remains 403'd on
// every cross-job route (see test/auth_scoped_test.go TestScopedKeyAllowDenyMatrix,
// which pins the allow/deny matrix against the live server). Agent-session tokens
// are a different principal kind and stay confined to /v1/agent/*: they are still
// denied whoami, so an agent can never complete a UI login.
//
// The real RBAC affordance-gating assertions (a viewer must not see Replay; a scoped
// principal must not see cross-job lineage) land WITH their gated controls in B3
// (replay) and F3 (lineage).

let keys: AuthLaneKeys;

test.beforeAll(async ({ request }) => {
  keys = await obtainAuthKeys(request);
});

test("viewer login resolves a viewer principal through the UI", async ({ page }) => {
  const principal = await loginAsViewer(page, keys);

  expect(principal.role).toBe("viewer");
});

test("runner login resolves a runner principal through the UI", async ({ page }) => {
  const principal = await loginAsRunner(page, keys);

  expect(principal.role).toBe("runner");
});

test("a job-scoped key resolves its own principal and whoami names its scope", async ({
  request,
}) => {
  const response = await request.get("/auth/whoami", {
    headers: authHeaders(keys.scoped),
  });

  expect(response.status()).toBe(200);

  const body = (await response.json()) as {
    kind?: string;
    role?: string;
    scope?: { jobs?: string[] };
  };
  expect(body.kind).toBe("api_key");
  expect(body.role).toBe("viewer");
  expect(body.scope?.jobs).toContain(keys.scopedJobAlias);
});
