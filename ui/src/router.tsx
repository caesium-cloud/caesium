import { createRootRoute, createRoute, createRouter, redirect, lazyRouteComponent } from "@tanstack/react-router";
import { AppShell } from "./components/layout/AppShell";

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
  component: lazyRouteComponent(() => import("./features/jobs/JobsPage"), "JobsPage"),
});

const jobDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId",
  component: lazyRouteComponent(() => import("./features/jobs/JobDetailPage"), "JobDetailPage"),
});

const runDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "jobs/$jobId/runs/$runId",
  component: lazyRouteComponent(() => import("./features/jobs/RunDetailPage"), "RunDetailPage"),
});

const statsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "stats",
  component: lazyRouteComponent(() => import("./features/stats/StatsPage"), "StatsPage"),
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
