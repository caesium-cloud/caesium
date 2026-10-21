import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

// Keycloak lane smoke (identity-and-access G1, design §12.2). Runs only in
// the `keycloak` project, against `just integration-up-keycloak` (CI job
// ui-e2e-keycloak): Keycloak at http://127.0.0.1:8180 with the realm in
// test/testdata/keycloak/caesium-realm.json, and Caesium at the base URL with
// OIDC against it, the lane policy and one job per namespace.
//
// The admin signs in through Keycloak's REAL login page and lands in the
// Console with a cookie session. G2 adds bob, carol, the namespace switcher
// and the Access page; the no_binding message is F1's and is not asserted
// here.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const realmFile = path.join(repoRoot, "test/testdata/keycloak/caesium-realm.json");
const keycloakRealmURL = "http://127.0.0.1:8180/realms/caesium/";
const laneJobs = ["kc-lane-marketing", "kc-lane-finance"];

type RealmUser = { username: string; credentials?: { type: string; value: string }[] };

// The realm fixture is the one source of the lane users' passwords.
function realmPassword(username: string): string {
  const realm = JSON.parse(fs.readFileSync(realmFile, "utf8")) as { users: RealmUser[] };
  const password = realm.users
    .find((user) => user.username === username)
    ?.credentials?.find((credential) => credential.type === "password")?.value;
  if (!password) {
    throw new Error(`${username} has no password in ${realmFile}`);
  }
  return password;
}

test("admin signs in through Keycloak's login page and lands in the Console", async ({ page, baseURL }) => {
  const consoleOrigin = new URL(baseURL ?? "http://127.0.0.1:8080").origin;
  // Lane prerequisite: a server that does not offer OIDC is a broken lane,
  // never a skip.
  const status = await page.request.get("/auth/status");
  expect(status.ok()).toBe(true);
  const { methods } = (await status.json()) as { methods: { type: string }[] };
  expect(methods.map((method) => method.type)).toContain("oidc");

  await page.goto("/");
  await page.getByRole("button", { name: "Sign in with OIDC" }).click();

  await page.waitForURL((url) => url.href.startsWith(keycloakRealmURL));
  await page.locator("#username").fill("admin@example.com");
  await page.locator("#password").fill(realmPassword("admin@example.com"));
  await page.locator("#kc-login").click();

  // Keycloak redirects to Caesium's callback, which sets the session cookie
  // and returns the browser to the Console.
  await page.waitForURL((url) => url.origin === consoleOrigin);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  for (const alias of laneJobs) {
    await expect(page.getByText(alias, { exact: true }).first()).toBeVisible();
  }

  // The browser holds a real cookie session for the SSO admin.
  const whoami = await page.request.get("/auth/whoami");
  expect(whoami.status()).toBe(200);
  const principal = (await whoami.json()) as {
    kind: string;
    subject: string;
    cluster_role: string;
    namespaces: string[];
  };
  expect(principal).toMatchObject({
    kind: "user",
    subject: "admin@example.com",
    cluster_role: "admin",
    namespaces: ["*"],
  });
});
