import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { AppShell } from "./components/layout/AppShell";
import { AtomsPage } from "./features/atoms/AtomsPage";
import { JobDefsPage } from "./features/jobdefs/JobDefsPage";
import { JobsPage } from "./features/jobs/JobsPage";
import { JobDetailPage } from "./features/jobs/JobDetailPage";
import { RunDetailPage } from "./features/jobs/RunDetailPage";
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

const runDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId/runs/$runId",
  component: RunDetailPage,
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
  runDetailRoute,
  statsRoute,
  triggersRoute,
  atomsRoute,
  systemRoute,
  jobDefsRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
