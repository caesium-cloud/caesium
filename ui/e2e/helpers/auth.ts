import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { expect, type APIRequestContext, type Browser, type BrowserContext, type Page, type Response } from "@playwright/test";

export type AuthLaneRole = "viewer" | "runner" | "scoped";

export type WhoamiPrincipal = {
  kind?: string;
  subject?: string;
  role: "viewer" | "runner" | "operator" | "admin";
};

export type AuthLaneKeys = {
  viewer: string;
  runner: string;
  scoped: string;
  scopedJobAlias: string;
};

let cachedKeys: Promise<AuthLaneKeys> | null = null;

export function authHeaders(key: string): Record<string, string> {
  return { Authorization: `Bearer ${key}` };
}

export async function obtainAuthKeys(request: APIRequestContext): Promise<AuthLaneKeys> {
  cachedKeys ??= seedAuthKeys(request);
  return cachedKeys;
}

export async function loginAsViewer(page: Page, keys: AuthLaneKeys): Promise<WhoamiPrincipal> {
  return loginWithApiKey(page, keys.viewer);
}

export async function loginAsRunner(page: Page, keys: AuthLaneKeys): Promise<WhoamiPrincipal> {
  return loginWithApiKey(page, keys.runner);
}

export async function loginAsScoped(page: Page, keys: AuthLaneKeys): Promise<WhoamiPrincipal> {
  return loginWithApiKey(page, keys.scoped);
}

export async function loginWithApiKey(page: Page, key: string): Promise<WhoamiPrincipal> {
  const principal = await loginAtUrl(page, "/", key);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  return principal;
}

// Log in WITHOUT a separate page load afterwards. The api-key session lives in a
// module-scoped variable (ui/src/lib/auth.ts) — NOT localStorage — so a full
// page reload (page.goto) AFTER login drops the key and bounces back to the
// login screen. AuthGate (ui/src/features/auth/AuthGate.tsx) instead renders the
// LoginPage IN PLACE at the current URL when unauthenticated and swaps in the
// app's children (the routed page) on login, with no navigation. So to land
// authenticated on a deep route, navigate THERE first, then log in: the routed
// page renders at the preserved URL. Do not page.goto again after this.
export async function loginAtUrl(page: Page, url: string, key: string): Promise<WhoamiPrincipal> {
  await page.goto(url);
  await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
  await page.getByPlaceholder("csk_live_...").fill(key);

  // Register the matcher only for the whoami the Sign In click triggers — NOT
  // before goto, where the page's own session-check whoami could be matched
  // (or its absence could exhaust the wait). The login verifies the key with a
  // GET /auth/whoami once the key is submitted.
  const whoami = page.waitForResponse(isSuccessfulWhoamiResponse);
  await page.getByRole("button", { name: "Sign In" }).click();

  return readWhoami(await whoami);
}

/**
 * Like loginAtUrl, but returns the raw whoami body and tolerates a principal
 * with no cluster role (a key confined to namespaces answers role: "").
 */
export async function loginRawAtUrl(page: Page, url: string, key: string): Promise<Record<string, unknown>> {
  await page.goto(url);
  await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
  await page.getByPlaceholder("csk_live_...").fill(key);
  const whoami = page.waitForResponse(isSuccessfulWhoamiResponse);
  await page.getByRole("button", { name: "Sign In" }).click();
  return (await (await whoami).json()) as Record<string, unknown>;
}

/** Mint a key with the given role and (optionally) namespace confinement. */
export async function createNamespacedKey(
  request: APIRequestContext,
  adminKey: string,
  role: "viewer" | "runner" | "operator" | "admin",
  description: string,
  namespaces?: string[],
): Promise<string> {
  const response = await request.post("/v1/auth/keys", {
    headers: { ...authHeaders(adminKey), "Content-Type": "application/json" },
    data: { role, description, ...(namespaces ? { namespaces } : {}) },
  });
  if (!response.ok()) {
    throw new Error(`failed to mint ${description}: ${response.status()} ${await response.text()}`);
  }
  const key = ((await response.json()) as { key?: unknown }).key;
  if (typeof key !== "string" || !key.startsWith("csk_")) {
    throw new Error(`failed to mint ${description}: no plaintext key in the response`);
  }
  return key;
}

export function adminKeyFromEnv(): string {
  const key = envString("CAESIUM_E2E_AUTH_ADMIN_KEY");
  if (!key) {
    throw new Error("auth e2e lane requires CAESIUM_E2E_AUTH_ADMIN_KEY");
  }
  return key;
}

async function seedAuthKeys(request: APIRequestContext): Promise<AuthLaneKeys> {
  const scopedJobAlias = envString("CAESIUM_E2E_AUTH_SCOPED_JOB") ?? "ui-e2e-scoped-job";
  const viewer = envString("CAESIUM_E2E_AUTH_VIEWER_KEY");
  const runner = envString("CAESIUM_E2E_AUTH_RUNNER_KEY");
  const scoped = envString("CAESIUM_E2E_AUTH_SCOPED_KEY");

  if (viewer && runner && scoped) {
    return { viewer, runner, scoped, scopedJobAlias };
  }

  const admin = envString("CAESIUM_E2E_AUTH_ADMIN_KEY");
  if (!admin) {
    throw new Error(
      "auth e2e lane requires CAESIUM_E2E_AUTH_ADMIN_KEY or pre-seeded CAESIUM_E2E_AUTH_* role keys",
    );
  }

  return {
    viewer: viewer ?? (await createAuthKey(request, admin, "viewer", "UI e2e viewer key")),
    runner: runner ?? (await createAuthKey(request, admin, "runner", "UI e2e runner key")),
    scoped:
      scoped ??
      (await createAuthKey(request, admin, "viewer", "UI e2e scoped viewer key", {
        jobs: [scopedJobAlias],
      })),
    scopedJobAlias,
  };
}

async function createAuthKey(
  request: APIRequestContext,
  adminKey: string,
  role: "viewer" | "runner",
  description: string,
  scope?: { jobs: string[] },
): Promise<string> {
  const response = await request.post("/v1/auth/keys", {
    headers: {
      ...authHeaders(adminKey),
      "Content-Type": "application/json",
    },
    data: {
      role,
      description,
      ...(scope ? { scope } : {}),
    },
  });

  if (!response.ok()) {
    throw new Error(`failed to seed ${description}: ${response.status()} ${await response.text()}`);
  }

  const body: unknown = await response.json();
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    throw new Error(`failed to seed ${description}: malformed response body`);
  }

  const key = (body as { key?: unknown }).key;
  if (typeof key !== "string" || !key.startsWith("csk_")) {
    throw new Error(`failed to seed ${description}: response did not include a plaintext key`);
  }

  return key;
}

async function readWhoami(response: Response): Promise<WhoamiPrincipal> {
  const body: unknown = await response.json();
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    throw new Error("auth login did not return a whoami object");
  }

  const role = (body as { role?: unknown }).role;
  if (role !== "viewer" && role !== "runner" && role !== "operator" && role !== "admin") {
    throw new Error(`auth login returned unexpected role: ${String(role)}`);
  }

  return {
    kind: stringField(body, "kind"),
    subject: stringField(body, "subject"),
    role,
  };
}

function isSuccessfulWhoamiResponse(response: Response): boolean {
  return (
    new URL(response.url()).pathname === "/auth/whoami" &&
    response.request().method() === "GET" &&
    response.status() === 200
  );
}

function envString(name: string): string | null {
  const value = process.env[name]?.trim();
  return value ? value : null;
}

function stringField(body: object, key: string): string | undefined {
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

// ---------------------------------------------------------------------------
// Keycloak lane helpers (identity-and-access G2). Used only by the `keycloak`
// Playwright project, against `just integration-up-keycloak`.
// ---------------------------------------------------------------------------

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const realmFile = path.join(repoRoot, "test/testdata/keycloak/caesium-realm.json");
export const keycloakRealmURL = "http://127.0.0.1:8180/realms/caesium/";

export type KeycloakUser = "admin@example.com" | "bob@example.com" | "carol@example.com";

type RealmUser = { username: string; credentials?: { type: string; value: string }[] };

/** The realm fixture is the one source of the lane users' passwords. */
export function realmPassword(username: string): string {
  const realm = JSON.parse(fs.readFileSync(realmFile, "utf8")) as { users: RealmUser[] };
  const password = realm.users
    .find((user) => user.username === username)
    ?.credentials?.find((credential) => credential.type === "password")?.value;
  if (!password) {
    throw new Error(`${username} has no password in ${realmFile}`);
  }
  return password;
}

/**
 * Drive Keycloak's real login page for `username`: from the Console login
 * screen, "Sign in with OIDC", the realm's form, then back to the Console
 * origin (the callback's redirect). It does not assert what the Console shows
 * afterwards; the caller does (carol is refused there).
 */
export async function ssoLogin(page: Page, username: KeycloakUser, baseURL?: string): Promise<void> {
  const consoleOrigin = new URL(baseURL ?? "http://127.0.0.1:8080").origin;
  await page.goto("/");
  await page.getByRole("button", { name: "Sign in with OIDC" }).click();
  await page.waitForURL((url) => url.href.startsWith(keycloakRealmURL));
  await page.locator("#username").fill(username);
  await page.locator("#password").fill(realmPassword(username));
  await page.locator("#kc-login").click();
  await page.waitForURL((url) => url.origin === consoleOrigin);
}

/** The cookie session's CSRF header, read from GET /auth/whoami (the Console does the same). */
export async function sessionCsrfHeaders(context: BrowserContext): Promise<Record<string, string>> {
  const whoami = await context.request.get("/auth/whoami");
  if (!whoami.ok()) {
    throw new Error(`whoami for the cookie session failed: ${whoami.status()}`);
  }
  const token = ((await whoami.json()) as { csrf_token?: unknown }).csrf_token;
  if (typeof token !== "string" || token === "") {
    throw new Error("whoami did not return a csrf_token for the cookie session");
  }
  return { "X-CSRF-Token": token };
}

export type KeycloakJobDefinition = {
  alias: string;
  namespace: string;
  command: string;
};

/**
 * Apply jobs as the lane's SSO admin, in a THROWAWAY browser context that no
 * asserted identity shares (a second login on one context keeps the first
 * cookie). Returns alias -> job id for every applied alias and every alias in
 * `lookupAliases`. A mutating request with a wrong CSRF token is a 403, which
 * counts against the auth-failure limiter, so the token is read fresh from
 * whoami (an unauthenticated whoami 401 does not count).
 */
export async function seedAsKeycloakAdmin(
  browser: Browser,
  baseURL: string | undefined,
  definitions: KeycloakJobDefinition[],
  lookupAliases: string[] = [],
): Promise<Record<string, string>> {
  const context = await browser.newContext({ baseURL });
  try {
    const page = await context.newPage();
    await ssoLogin(page, "admin@example.com", baseURL);
    if (definitions.length > 0) {
      const apply = await context.request.post("/v1/jobdefs/apply", {
        headers: { ...(await sessionCsrfHeaders(context)), "Content-Type": "application/json" },
        data: {
          definitions: definitions.map((def) => ({
            apiVersion: "v1",
            kind: "Job",
            metadata: { alias: def.alias, namespace: def.namespace },
            trigger: { type: "cron", configuration: { cron: "0 0 1 1 *", timezone: "UTC" } },
            steps: [{ name: "only", engine: "docker", image: "alpine:3.23", command: ["sh", "-c", def.command] }],
          })),
        },
      });
      if (!apply.ok()) {
        throw new Error(`failed to apply seeded jobs: ${apply.status()} ${await apply.text()}`);
      }
    }
    const list = await context.request.get("/v1/jobs");
    if (!list.ok()) {
      throw new Error(`failed to list jobs as the SSO admin: ${list.status()}`);
    }
    const jobs = (await list.json()) as Array<{ id: string; alias: string }>;
    const ids: Record<string, string> = {};
    for (const alias of [...definitions.map((def) => def.alias), ...lookupAliases]) {
      const found = jobs.find((job) => job.alias === alias);
      if (!found) {
        throw new Error(`job ${alias} not found after seeding`);
      }
      ids[alias] = found.id;
    }
    return ids;
  } finally {
    await context.close();
  }
}
