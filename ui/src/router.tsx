import { createRootRoute, createRoute, createRouter, redirect } from "@tanstack/react-router";
import { AppShell } from "./components/layout/AppShell";
import { ConsoleNotFound } from "./components/not-found-state";
import { AtomsPage } from "./features/atoms/AtomsPage";
import { DatabaseConsolePage } from "./features/database/DatabaseConsolePage";
import { DatasetsPage } from "./features/datasets/DatasetsPage";
import { IncidentDetailPage } from "./features/incidents/IncidentDetailPage";
import { IncidentsPage } from "./features/incidents/IncidentsPage";
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
import { api } from "./lib/api";
import { normalizeStatusFilter } from "./features/datasets/freshness-utils";

const rootRoute = createRootRoute({
  component: AppShell,
  notFoundComponent: ConsoleNotFound,
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

async function requireFreshnessEnabled() {
  const features = await api.getSystemFeatures();
  if (!features.freshness_enabled) {
    throw redirect({ to: "/jobs" });
  }
}

const datasetsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "datasets",
  validateSearch: (search: Record<string, unknown>) => {
    const status = normalizeStatusFilter(search.status);
    return {
      status: status === "all" ? undefined : status,
      namespace: typeof search.namespace === "string" ? search.namespace : undefined,
      name: typeof search.name === "string" ? search.name : undefined,
    };
  },
  loader: requireFreshnessEnabled,
  component: DatasetsPage,
});

async function requireAgentRemediationEnabled() {
  const features = await api.getSystemFeatures();
  if (!features.agent_remediation_enabled) {
    throw redirect({ to: "/jobs" });
  }
}

const incidentsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "incidents",
  validateSearch: (search: Record<string, unknown>) => {
    const out: {
      status?: string;
      class?: string;
      job_id?: string;
      needs_approval?: boolean;
    } = {};
    if (typeof search.status === "string") out.status = search.status;
    if (typeof search.class === "string") out.class = search.class;
    if (typeof search.job_id === "string") out.job_id = search.job_id;
    // The backend only supports a truthy needs_approval filter (awaiting-approval);
    // a falsy value is not a distinct server-side filter, so normalize it away
    // rather than persisting a URL that claims a filter the page never applies.
    if (search.needs_approval === true || search.needs_approval === "true") {
      out.needs_approval = true;
    }
    return out;
  },
  loader: requireAgentRemediationEnabled,
  component: IncidentsPage,
});

const incidentDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "incidents/$incidentId",
  loader: requireAgentRemediationEnabled,
  component: IncidentDetailPage,
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
  datasetsRoute,
  incidentsRoute,
  incidentDetailRoute,
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
