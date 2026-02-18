import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { AppShell } from "./components/layout/AppShell";
import { JobsPage } from "./features/jobs/JobsPage";
import { JobDetailPage } from "./features/jobs/JobDetailPage";
import { RunDetailPage } from "./features/jobs/RunDetailPage";
import { StatsPage } from "./features/stats/StatsPage";

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

const routeTree = rootRoute.addChildren([
  indexRoute,
  jobsRoute,
  jobDetailRoute,
  runDetailRoute,
  statsRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
