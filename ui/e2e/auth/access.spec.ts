import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import {
  adminKeyFromEnv,
  authHeaders,
  createNamespacedKey,
  loginRawAtUrl,
  obtainAuthKeys,
  type AuthLaneKeys,
} from "../helpers/auth";

// The Access page and namespace-aware gating against the api-key auth lane.
// That server mounts no policy file, so any syntactically valid namespace can
// be minted into a key and applied to a job; marketing and finance stand in for
// two tenants.

let keys: AuthLaneKeys;
let admin: string;
const suffix = Date.now().toString(36);
const marketingAlias = `access-marketing-${suffix}`;
const financeAlias = `access-finance-${suffix}`;

function definition(alias: string, namespace: string) {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias, namespace },
    trigger: { type: "cron", configuration: { cron: "0 3 * * *", timezone: "UTC" } },
    steps: [{ name: "only", engine: "docker", image: "alpine:3.23", command: ["sh", "-c", "echo ok"] }],
  };
}

async function applyJobs(request: APIRequestContext) {
  const response = await request.post("/v1/jobdefs/apply", {
    headers: { ...authHeaders(admin), "Content-Type": "application/json" },
    data: { definitions: [definition(marketingAlias, "marketing"), definition(financeAlias, "finance")] },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply namespaced jobs: ${response.status()} ${await response.text()}`);
  }
}

test.beforeAll(async ({ request }) => {
  keys = await obtainAuthKeys(request);
  admin = adminKeyFromEnv();
  await applyJobs(request);
});

async function openAccessViaSidebar(page: Page) {
  await page.getByRole("link", { name: /^access$/i }).click();
  await expect(page.getByRole("heading", { name: "access", exact: true })).toBeVisible();
}

test("an admin sees its grants, the namespaces, the policy and the users section", async ({ page }) => {
  const whoami = await loginRawAtUrl(page, "/access", admin);
  expect(whoami.role).toBe("admin");

  await expect(page.getByTestId("access-cluster-role")).toHaveText("admin");
  await expect(page.getByTestId("access-grant-row")).toContainText("all namespaces");
  await expect(page.getByTestId("access-namespaces")).toBeVisible();
  await expect(page.getByTestId("access-namespace-row").first()).toContainText("default");
  await expect(page.getByTestId("access-policy")).toBeVisible();
  await expect(page.getByTestId("access-policy-last-reload")).toBeVisible();
  // The api-key lane has no SSO users: the section renders its empty state or rows.
  await expect(page.getByTestId("access-users")).toBeVisible();
});

test("the Access entry is in the sidebar and opens the page", async ({ page }) => {
  await loginRawAtUrl(page, "/", keys.viewer);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await openAccessViaSidebar(page);
  await expect(page.getByTestId("access-mine")).toBeVisible();
});

test("a cluster viewer sees the namespaces but no policy or users", async ({ page }) => {
  await loginRawAtUrl(page, "/access", keys.viewer);
  await expect(page.getByTestId("access-cluster-role")).toHaveText("viewer");
  await expect(page.getByTestId("access-namespaces")).toBeVisible();
  await expect(page.getByTestId("access-policy")).toHaveCount(0);
  await expect(page.getByTestId("access-users")).toHaveCount(0);
  // The namespaces payload never carries bindings.
  await expect(page.getByTestId("access-namespaces")).not.toContainText("groups");
});

test("a key confined to a namespace sees only its own access", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "runner", `access confined ${suffix}`, ["marketing"]);
  const whoami = await loginRawAtUrl(page, "/access", key);
  expect(whoami.role).toBe("");

  await expect(page.getByTestId("access-cluster-role")).toHaveText("none");
  const rows = page.getByTestId("access-grant-row");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("marketing");
  await expect(rows.first()).toContainText("runner");
  await expect(page.getByTestId("access-namespaces")).toHaveCount(0);
  await expect(page.getByTestId("access-policy")).toHaveCount(0);
  await expect(page.getByTestId("access-users")).toHaveCount(0);
});

function row(page: Page, alias: string) {
  return page.getByTestId("job-row").filter({ hasText: alias });
}

test("a runner confined to marketing triggers its job but cannot pause it", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "runner", `access runner ${suffix}`, ["marketing"]);
  await loginRawAtUrl(page, "/jobs", key);

  const mine = row(page, marketingAlias);
  await expect(mine).toBeVisible();
  await expect(mine.getByRole("button", { name: "Trigger run" })).toBeEnabled();
  const pause = mine.getByRole("button", { name: "Pause future runs" });
  await expect(pause).toBeDisabled();
  await expect(pause).toHaveAttribute("title", "Requires operator role in marketing");
  // Another tenant's job is not listed at all.
  await expect(row(page, financeAlias)).toHaveCount(0);
});

test("an operator confined to finance pauses finance but never sees marketing", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "operator", `access operator ${suffix}`, ["finance"]);
  await loginRawAtUrl(page, "/jobs", key);

  const mine = row(page, financeAlias);
  await expect(mine).toBeVisible();
  await expect(mine.getByRole("button", { name: "Pause future runs" })).toBeEnabled();
  await expect(mine.getByRole("button", { name: "Trigger run" })).toBeEnabled();
  await expect(row(page, marketingAlias)).toHaveCount(0);
});

test("a cluster viewer sees every namespace's jobs with all actions disabled", async ({ page }) => {
  await loginRawAtUrl(page, "/jobs", keys.viewer);
  for (const alias of [marketingAlias, financeAlias]) {
    const r = row(page, alias);
    await expect(r).toBeVisible();
    await expect(r.getByRole("button", { name: "Trigger run" })).toBeDisabled();
    await expect(r.getByRole("button", { name: "Pause future runs" })).toBeDisabled();
  }
});
