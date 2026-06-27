import { expect, type APIRequestContext, type Page, type Response } from "@playwright/test";

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
  await page.goto("/");
  await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
  await page.getByPlaceholder("csk_live_...").fill(key);

  // Register the matcher only for the whoami the Sign In click triggers — NOT
  // before goto, where the page's own session-check whoami could be matched
  // (or its absence could exhaust the wait). The login verifies the key with a
  // GET /auth/whoami once the key is submitted.
  const whoami = page.waitForResponse(isSuccessfulWhoamiResponse);
  await page.getByRole("button", { name: "Sign In" }).click();

  const principal = await readWhoami(await whoami);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  return principal;
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
