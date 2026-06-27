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
//   - the job-SCOPED key is an API-ONLY principal: the scope middleware
//     (api/middleware/auth_scope.go) denies a scoped key GET /auth/whoami with 403, so
//     it cannot complete the UI's api-key login (apiKeyLogin requires a 200 whoami).
//     We assert that 403 at the API level instead of attempting a UI login.
//
// The real RBAC affordance-gating assertions (a viewer must not see Replay; a scoped
// principal must not see cross-job lineage) land WITH their gated controls in B3
// (replay) and F3 (lineage). Note for F3: because a scoped key cannot UI-login, the
// scoped-lineage-denied behaviour must be exercised at the API level, not via the UI.

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

test("a job-scoped key is denied the global whoami (API-only principal)", async ({ request }) => {
  const response = await request.get("/auth/whoami", {
    headers: authHeaders(keys.scoped),
  });

  expect(response.status()).toBe(403);
});
