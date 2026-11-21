import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

const project = `caesium-health-${randomUUID()}`;
const file = fileURLToPath(new URL("../../test/cluster-health/compose.yaml", import.meta.url));
function compose(...args: string[]) {
  return execFileSync("docker", ["compose", "-p", project, "-f", file, ...args], {
    encoding: "utf8",
    timeout: 180_000,
  }).trim();
}

test.beforeAll(() => {
  compose("up", "-d", "--wait", "--wait-timeout", "120");
});

test.afterAll(() => {
  const info = test.info();
  try {
    if (info.status !== info.expectedStatus) console.error(compose("ps", "--all"));
  } finally {
    compose("down", "--volumes");
  }
});

test("system health tracks a voter stopping and restarting on the same open page", async ({ page }) => {
  const origin = compose("port", "node1", "8080");
  const target = compose("exec", "-T", "node3", "hostname", "-i") + ":9001";
  await page.goto(`http://${origin}/system`);

  const row = page.getByTestId("cluster-node-row").and(page.locator(`[data-address="${target}"]`));
  const quorum = page.getByTestId("quorum-count");
  const badge = page.getByTestId("system-health-badge");
  const banner = page.getByTestId("system-health-banner");
  const sidebar = page.getByTestId("sidebar-quorum");
  const pollWindow = { timeout: 75_000 };

  async function expectHealthy() {
    await expect(quorum).toHaveText("3/3", pollWindow);
    await expect(badge).toHaveText("operational", pollWindow);
    await expect(row).toHaveAttribute("data-reachability", "reachable", pollWindow);
    await expect(sidebar).toHaveText("3/3", pollWindow);
    await expect(page.getByText("All systems nominal", { exact: true })).toBeVisible();
  }

  await expectHealthy();
  compose("kill", "-s", "SIGKILL", "node3");
  await expect(quorum).toHaveText("2/3", pollWindow);
  await expect(banner).toHaveAttribute("data-tone", "warn", pollWindow);
  await expect(badge).toHaveText("degraded", pollWindow);
  await expect(row).toHaveAttribute("data-reachability", "unreachable", pollWindow);
  await expect(row.getByText("Unreachable", { exact: true })).toBeVisible();
  await expect(sidebar).toHaveText("2/3", pollWindow);
  await expect(page.getByRole("region", { name: "Cluster health" }).getByText(/Serving without full redundancy/)).toBeVisible();

  compose("start", "node3");
  await expectHealthy();
});
