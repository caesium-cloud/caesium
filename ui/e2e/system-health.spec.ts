import { expect, test } from "@playwright/test";
import { failOnUnexpectedPageErrors } from "./helpers/fixtures";

/**
 * Cluster-health truthfulness on /system (issue #494).
 *
 * The first test runs LIVE against this e2e server: whatever the real `/health`
 * reports about quorum is what the page must render, and every node row must
 * carry an explicit, server-reported liveness.
 *
 * The remaining tests are explicitly SYNTHETIC, and cover ONLY the rendering
 * contract. This harness starts a single `caesium-server` container, so it
 * cannot stop one replica of a three-replica cluster; they intercept `/health`
 * and `/v1/system/nodes` to drive the multi-replica states the console must
 * never mis-render.
 *
 * They are NOT the regression for issue #494 and must not be read as one — a
 * `liveProbe` that always returned success would sail through every one of
 * them. The authoritative regression is
 * `TestHealthObservesARealStoppedNodeOverHTTP` in
 * `api/health_real_cluster_test.go`: three real dqlite nodes, one really
 * stopped, observed through the real background refresh and read back from the
 * real `/health` handler over HTTP, with no stubs anywhere. Between here
 * and there sit `internal/cluster` (the decision table), `api/health_test.go`
 * (the real handler and its status codes) and the
 * `TestSystemHealthReportsProbedQuorum` / `TestHealthProbeEndpointsAreSplit`
 * integration scenarios against a live server.
 */

failOnUnexpectedPageErrors();

const LEADER = "10.244.0.8:9001";
const FOLLOWER = "10.244.0.9:9001";
const CRASHED = "10.244.0.10:9001";

type Reachability = "reachable" | "unreachable" | "unknown";

function member(address: string, reachability: Reachability, leader = false) {
  return {
    address,
    role: "voter",
    leader,
    reachability,
    ...(reachability === "reachable" ? { latency_ms: 2 } : {}),
  };
}

function node(address: string, reachability: Reachability, leader = false) {
  return { ...member(address, reachability, leader), arch: "amd64", workers_busy: 0, workers_total: 4 };
}

function nodeSummary(members: ReturnType<typeof member>[]) {
  const count = (r: Reachability) => members.filter((m) => m.reachability === r).length;
  const unreachable = count("unreachable");
  const unknown = count("unknown");
  let status = "available";
  if (members.length === 0 || (unreachable === 0 && unknown === members.length)) status = "unknown";
  else if (unreachable > 0) status = "degraded";
  else if (unknown > 0) status = "unknown";
  return {
    status,
    total: members.length,
    reachable: count("reachable"),
    unreachable,
    unknown,
  };
}

function healthBody(
  status: string,
  quorum: Record<string, unknown>,
  members: ReturnType<typeof member>[],
  observed = true,
) {
  return {
    status,
    uptime: 600_000_000_000,
    checks: {
      database: { status: "healthy", latency_ms: 1 },
      active_runs: { status: "healthy", count: 0 },
      triggers: { status: "healthy", count: 1 },
      nodes: { status: status === "healthy" ? "healthy" : status, count: quorum.reachable_voters },
      cluster: {
        status: status === "healthy" ? "healthy" : status,
        clustered: true,
        observed,
        observed_at: new Date().toISOString(),
        quorum,
        nodes: nodeSummary(members),
        members,
      },
    },
  };
}

async function stubCluster(
  page: import("@playwright/test").Page,
  health: ReturnType<typeof healthBody>,
  nodes: ReturnType<typeof node>[],
) {
  await page.route("**/health", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(health),
    });
  });
  await page.route("**/v1/system/nodes", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(nodes),
    });
  });
}

test("the live system page renders quorum from the server's probed liveness", async ({
  page,
  request,
}) => {
  const response = await request.get("/health");
  expect(response.ok()).toBeTruthy();
  const body = await response.json();

  const cluster = body?.checks?.cluster;
  test.skip(!cluster?.clustered, "this server is not backed by a dqlite cluster");

  // Liveness is observed in the background; give the first probe a poll cycle.
  await expect
    .poll(
      async () => {
        const current = await (await request.get("/health")).json();
        return current?.checks?.cluster?.observed === true;
      },
      { timeout: 30_000 },
    )
    .toBe(true);

  const observed = await (await request.get("/health")).json();
  const quorum = observed.checks.cluster.quorum;

  await page.goto("/system");
  await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();

  // The numerator is the server's reachable voter count, not the row count.
  await expect(page.getByTestId("quorum-count")).toHaveText(
    `${quorum.reachable_voters}/${quorum.total_voters}`,
  );

  const rows = page.getByTestId("cluster-node-row");
  await expect(rows.first()).toBeVisible();
  for (const row of await rows.all()) {
    expect(["reachable", "unreachable", "unknown"]).toContain(
      await row.getAttribute("data-reachability"),
    );
  }
});

test("SYNTHETIC: a crashed replica renders as degraded 2/3, not operational 3/3", async ({
  page,
}) => {
  await stubCluster(
    page,
    healthBody(
      "degraded",
      {
        status: "degraded",
        total_voters: 3,
        reachable_voters: 2,
        unreachable_voters: 1,
        unknown_voters: 0,
        required_voters: 2,
        available: true,
        degraded: true,
        leader_address: LEADER,
      },
      [
        member(LEADER, "reachable", true),
        member(FOLLOWER, "reachable"),
        member(CRASHED, "unreachable"),
      ],
    ),
    [node(LEADER, "reachable", true), node(FOLLOWER, "reachable"), node(CRASHED, "unreachable")],
  );

  await page.goto("/system");

  await expect(page.getByTestId("quorum-count")).toHaveText("2/3");
  await expect(page.getByTestId("system-health-badge")).toHaveText("degraded");
  await expect(page.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "warn");
  await expect(page.getByText("All systems operational")).toHaveCount(0);
  await expect(page.getByTestId("system-nodes-kpi")).toHaveText("2/3");

  // The dead replica is still listed — as unreachable.
  const dead = page.locator(`[data-testid="cluster-node-row"][data-address="${CRASHED}"]`);
  await expect(dead).toHaveAttribute("data-reachability", "unreachable");
  await expect(dead.getByText("Unreachable")).toBeVisible();

  // The Nodes health check is no longer unconditionally green.
  await expect(page.locator('[data-testid="health-check-row"][data-check="Nodes"]')).toHaveAttribute(
    "data-tone",
    "warn",
  );

  // ...and neither is the sidebar.
  await expect(page.getByText("All systems nominal")).toHaveCount(0);
  await expect(page.getByTestId("sidebar-quorum")).toHaveText("2/3");
});

test("SYNTHETIC: an unreachable standby degrades the page even with a full voter quorum", async ({
  page,
}) => {
  const STANDBY = "10.244.0.11:9001";
  const members = [
    member(LEADER, "reachable", true),
    member(FOLLOWER, "reachable"),
    member(CRASHED, "reachable"),
    { ...member(STANDBY, "unreachable"), role: "standby" },
  ];

  await stubCluster(
    page,
    healthBody(
      "degraded",
      {
        status: "available",
        total_voters: 3,
        reachable_voters: 3,
        unreachable_voters: 0,
        unknown_voters: 0,
        required_voters: 2,
        available: true,
        degraded: false,
        leader_address: LEADER,
      },
      members,
    ),
    [
      node(LEADER, "reachable", true),
      node(FOLLOWER, "reachable"),
      node(CRASHED, "reachable"),
      { ...node(STANDBY, "unreachable"), role: "standby" },
    ],
  );

  await page.goto("/system");

  // Quorum arithmetic is voter-only and still correct.
  await expect(page.getByTestId("quorum-count")).toHaveText("3/3");

  // The page must not call that operational.
  await expect(page.getByText("All systems operational")).toHaveCount(0);
  await expect(page.getByText("All systems nominal")).toHaveCount(0);
  await expect(page.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "warn");
  await expect(page.getByTestId("system-health-badge")).toHaveText("degraded");
  await expect(page.locator('[data-testid="health-check-row"][data-check="Nodes"]')).toHaveAttribute(
    "data-tone",
    "warn",
  );

  const standby = page.locator(`[data-testid="cluster-node-row"][data-address="${STANDBY}"]`);
  await expect(standby).toHaveAttribute("data-reachability", "unreachable");
});

test("SYNTHETIC: a lost quorum renders as an outage", async ({ page }) => {
  await stubCluster(
    page,
    healthBody(
      "unavailable",
      {
        status: "unavailable",
        total_voters: 3,
        reachable_voters: 1,
        unreachable_voters: 2,
        unknown_voters: 0,
        required_voters: 2,
        available: false,
        degraded: true,
        leader_address: "",
      },
      [
        member(LEADER, "reachable", true),
        member(FOLLOWER, "unreachable"),
        member(CRASHED, "unreachable"),
      ],
    ),
    [node(LEADER, "reachable", true), node(FOLLOWER, "unreachable"), node(CRASHED, "unreachable")],
  );

  await page.goto("/system");

  await expect(page.getByTestId("quorum-count")).toHaveText("1/3");
  await expect(page.getByTestId("system-health-badge")).toHaveText("unavailable");
  await expect(page.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "danger");
  await expect(page.getByTestId("quorum-detail")).toContainText("Quorum lost");
});

test("SYNTHETIC: unverified liveness never renders as green", async ({ page }) => {
  await stubCluster(
    page,
    healthBody(
      "unknown",
      {
        status: "unknown",
        total_voters: 3,
        reachable_voters: 0,
        unreachable_voters: 0,
        unknown_voters: 3,
        required_voters: 2,
        available: false,
        degraded: false,
      },
      [],
      false,
    ),
    [node(LEADER, "unknown"), node(FOLLOWER, "unknown"), node(CRASHED, "unknown")],
  );

  await page.goto("/system");

  await expect(page.getByTestId("quorum-count")).toHaveText("?/3");
  await expect(page.getByTestId("system-nodes-kpi")).toHaveText("?/3");
  await expect(page.getByTestId("system-health-banner")).not.toHaveAttribute("data-tone", "ok");
  await expect(page.getByText("All systems operational")).toHaveCount(0);
  await expect(page.getByText("All systems nominal")).toHaveCount(0);
});
