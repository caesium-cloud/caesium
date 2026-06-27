import { expect, test } from "@playwright/test";
import {
  loginAsRunner,
  loginAsScoped,
  loginAsViewer,
  obtainAuthKeys,
  type AuthLaneKeys,
} from "../helpers/auth";

// Smoke for the auth-ENABLED e2e lane. It proves the harness works: each seeded key
// (viewer, runner, and the job-scoped viewer key) logs in through the real UI and
// resolves its own principal via /auth/whoami, and the lane distinguishes roles.
//
// The real RBAC affordance-gating assertions — a viewer must NOT see the Replay
// action, a scoped key must NOT see cross-job lineage — land WITH their gated
// controls in B3 (replay) and F3 (lineage), which don't exist on master yet. This
// lane is the harness those specs will run on.

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

test("scoped key logs in and resolves its principal through the UI", async ({ page }) => {
  // The scoped key is a viewer-role key restricted to a single job; it must still
  // authenticate through the lane. F3 will assert it cannot see cross-job lineage.
  const principal = await loginAsScoped(page, keys);

  expect(principal.role).toBe("viewer");
});
