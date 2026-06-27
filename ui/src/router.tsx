import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { AppShell } from "./components/layout/AppShell";
import { AtomsPage } from "./features/atoms/AtomsPage";
import { DatabaseConsolePage } from "./features/database/DatabaseConsolePage";
import { LogConsolePage } from "./features/logs/LogConsolePage";
import { JobDefsPage } from "./features/jobdefs/JobDefsPage";
import { BlameRoutePage } from "./features/jobs/BlameView";
import { JobsPage } from "./features/jobs/JobsPage";
import { JobDetailPage } from "./features/jobs/JobDetailPage";
import { LineageRoutePage } from "./features/jobs/LineageGraph";
import { RunDetailPage } from "./features/jobs/RunDetailPage";
import { RunDiffRoutePage } from "./features/jobs/RunDiffView";
import { StatsPage } from "./features/stats/StatsPage";
import { SystemPage } from "./features/system/SystemPage";
import { TriggersPage } from "./features/triggers/TriggersPage";

const rootRoute = createRootRoute({
  component: AppShell,
});

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  loader: () => { throw redirect({ to: '/jobs' }) },
});

const jobsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs",
  component: JobsPage,
});

const jobDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId",
  component: JobDetailPage,
});

const blameRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId/blame",
  validateSearch: (search: Record<string, unknown>) => ({
    from: typeof search.from === "string" ? search.from : undefined,
    to: typeof search.to === "string" ? search.to : undefined,
    task: typeof search.task === "string" ? search.task : undefined,
  }),
  component: BlameRoutePage,
});

const runDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId/runs/$runId",
  component: RunDetailPage,
});

const runDiffRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId/runs/$runId/diff",
  validateSearch: (search: Record<string, unknown>) => ({
    to: typeof search.to === "string" ? search.to : undefined,
  }),
  component: RunDiffRoutePage,
});

const lineageRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "lineage",
  validateSearch: (search: Record<string, unknown>) => ({
    namespace: typeof search.namespace === "string" ? search.namespace : undefined,
    name: typeof search.name === "string" ? search.name : undefined,
  }),
  component: LineageRoutePage,
});

const statsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "stats",
  component: StatsPage,
});

const triggersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "triggers",
  component: TriggersPage,
});

const atomsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "atoms",
  component: AtomsPage,
});

const databaseLegacyRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "database",
  loader: () => { throw redirect({ to: "/system/database" }) },
});

const databaseRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "system/database",
  component: DatabaseConsolePage,
});

const logsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "system/logs",
  component: LogConsolePage,
});

const systemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "system",
  component: SystemPage,
});

const jobDefsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobdefs",
  component: JobDefsPage,
});

const routeTree = rootRoute.addChildren([
  indexRoute,
  jobsRoute,
  jobDetailRoute,
  blameRoute,
  runDiffRoute,
  runDetailRoute,
  lineageRoute,
  statsRoute,
  triggersRoute,
  atomsRoute,
  systemRoute,
  logsRoute,
  databaseLegacyRoute,
  databaseRoute,
  jobDefsRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
