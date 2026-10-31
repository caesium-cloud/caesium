import { expect, test, type Page } from "@playwright/test";
import { seedAsKeycloakAdmin, ssoLogin } from "./helpers/auth";

// Keycloak lane journeys (identity-and-access G1 smoke + G2, design §12.2).
// Runs only in the `keycloak` project, against `just integration-up-keycloak`
// (CI job ui-e2e-keycloak): Keycloak at http://127.0.0.1:8180 with the realm
// in test/testdata/keycloak/caesium-realm.json, and Caesium at the base URL
// with OIDC against it, the lane policy (test/testdata/keycloak/policy.yaml)
// and one job per namespace.
//
// Every identity signs in through Keycloak's REAL login page in its OWN
// browser context (Playwright gives each test a fresh one); a second login on
// one context would keep the first cookie. Nothing here asserts the full set
// of declared namespaces: other streams add fixture namespaces, so a test
// checks only the ones it uses.
//
// Auth-failure limiter: carol's refusal is a 302 from the callback to
// /?login_error=no_binding, and an unauthenticated /auth/whoami 401 does not
// count against the 10/min limiter; an invalid CSRF token (403) does, which is
// why the seeding helper always reads a fresh token.

const laneMarketing = "kc-lane-marketing";
const laneFinance = "kc-lane-finance";

test.describe.configure({ mode: "serial" });

function row(page: Page, alias: string) {
  return page.getByTestId("job-row").filter({ hasText: alias });
}

test("admin signs in through Keycloak's login page and lands in the Console", async ({ page, baseURL }) => {
  // Lane prerequisite: a server that does not offer OIDC is a broken lane,
  // never a skip.
  const status = await page.request.get("/auth/status");
  expect(status.ok()).toBe(true);
  const { methods } = (await status.json()) as { methods: { type: string }[] };
  expect(methods.map((method) => method.type)).toContain("oidc");

  await ssoLogin(page, "admin@example.com", baseURL);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  for (const alias of [laneMarketing, laneFinance]) {
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

test("admin sees both namespaces, their chips and the switcher", async ({ page, baseURL }) => {
  await ssoLogin(page, "admin@example.com", baseURL);
  await expect(row(page, laneMarketing)).toBeVisible();
  await expect(row(page, laneFinance)).toBeVisible();
  await expect(row(page, laneMarketing).getByTestId("namespace-chip")).toHaveText("marketing");
  await expect(row(page, laneFinance).getByTestId("namespace-chip")).toHaveText("finance");

  const switcher = page.getByTestId("namespace-switcher");
  await expect(switcher).toBeVisible();
  // The declared set grows as streams add fixtures: require only the two this
  // journey uses.
  const options = await switcher.locator("option").evaluateAll((nodes) =>
    nodes.map((node) => (node as HTMLOptionElement).value),
  );
  expect(options).toEqual(expect.arrayContaining(["marketing", "finance"]));

  const narrowed = page.waitForRequest((req) => req.url().includes("/v1/jobs?namespace=marketing"));
  await switcher.selectOption("marketing");
  await narrowed;
  await expect(row(page, laneMarketing)).toBeVisible();
  await expect(row(page, laneFinance)).toHaveCount(0);

  await switcher.selectOption("");
  await expect(row(page, laneFinance)).toBeVisible();
});

test("bob sees only marketing and a finance deep link renders InsufficientAccess", async ({ page, browser, baseURL }) => {
  // The finance job id comes from a throwaway admin context: bob cannot list it.
  const ids = await seedAsKeycloakAdmin(browser, baseURL, [], [laneFinance]);

  await ssoLogin(page, "bob@example.com", baseURL);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expect(row(page, laneMarketing)).toBeVisible();
  await expect(row(page, laneMarketing).getByTestId("namespace-chip")).toHaveText("marketing");
  await expect(row(page, laneFinance)).toHaveCount(0);
  // One visible namespace: the chip shows, the switcher does not.
  await expect(page.getByTestId("namespace-switcher")).toHaveCount(0);

  // The cookie session survives a full navigation, so the deep link needs no
  // second login.
  await page.goto(`/jobs/${ids[laneFinance]}`);
  const denied = page.getByTestId("job-insufficient-access");
  await expect(denied).toBeVisible();
  await expect(denied).toContainText("You do not have access to this job's namespace.");
  await expect(page.getByText(laneFinance)).toHaveCount(0);
  await expect(page.getByText(/job not found/i)).toHaveCount(0);
  await expect(page.getByText(/\b403\b/)).toHaveCount(0);
});

test("carol authenticates at Keycloak and is refused with no_binding", async ({ page, baseURL }) => {
  await ssoLogin(page, "carol@example.com", baseURL);

  const alert = page.getByRole("alert").filter({ hasText: "has no access to Caesium" });
  await expect(alert).toBeVisible();
  await expect(alert).toHaveText(
    "You signed in, but your account has no access to Caesium. Ask an administrator to grant your group a role.",
  );
  // The reason is consumed from the address bar, and no session was created.
  expect(new URL(page.url()).searchParams.has("login_error")).toBe(false);
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toHaveCount(0);
  const whoami = await page.request.get("/auth/whoami");
  expect(whoami.status()).toBe(401);
});

test("admin's Access page shows grants, namespaces, policy and the users list", async ({ page, baseURL }) => {
  await ssoLogin(page, "admin@example.com", baseURL);
  await page.goto("/access");

  await expect(page.getByTestId("access-subject")).toHaveText("admin@example.com");
  await expect(page.getByTestId("access-kind")).toHaveText("user");
  await expect(page.getByTestId("access-cluster-role")).toHaveText("admin");
  const grants = page.getByTestId("access-grant-row");
  await expect(grants).toHaveCount(1);
  await expect(grants.first()).toContainText("all namespaces");
  await expect(grants.first()).toContainText("admin");

  const namespaces = page.getByTestId("access-namespaces");
  await expect(namespaces).toBeVisible();
  await expect(namespaces.getByTestId("access-namespace-row").filter({ hasText: "marketing" })).toHaveCount(1);
  await expect(namespaces.getByTestId("access-namespace-row").filter({ hasText: "finance" })).toHaveCount(1);
  await expect(page.getByTestId("access-policy")).toBeVisible();

  // The users list holds whoever has signed in through SSO: at least the admin.
  const users = page.getByTestId("access-users");
  await expect(users).toBeVisible();
  await expect(users.getByTestId("access-user-row").filter({ hasText: "admin@example.com" })).toHaveCount(1);
});

test("bob's Access page shows his one grant and no cluster sections", async ({ page, baseURL }) => {
  await ssoLogin(page, "bob@example.com", baseURL);
  await page.goto("/access");

  await expect(page.getByTestId("access-subject")).toHaveText("bob@example.com");
  const grants = page.getByTestId("access-grant-row");
  await expect(grants).toHaveCount(1);
  await expect(grants.first()).toContainText("marketing");
  await expect(grants.first()).toContainText("operator");
  await expect(page.getByTestId("access-namespaces")).toHaveCount(0);
  await expect(page.getByTestId("access-policy")).toHaveCount(0);
  await expect(page.getByTestId("access-users")).toHaveCount(0);
});
