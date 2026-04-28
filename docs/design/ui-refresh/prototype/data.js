/* === Caesium Refresh — Mock Data ===
   Realistic ETL/data jobs with run history, DAG topology, and lifecycle events.
*/

const NOW = Date.now();
const minutes = (m) => new Date(NOW - m * 60_000).toISOString();
const seconds = (s) => new Date(NOW - s * 1000).toISOString();

// 7-day sparkline generator (success rate + run count per day)
function spark(days, base, variance) {
  return Array.from({ length: days }, (_, i) => ({
    day: i,
    runs: Math.max(1, Math.round(base + (Math.sin(i * 1.3) * variance) + (Math.random() * 4 - 2))),
    success: Math.min(1, Math.max(0.4, 0.92 + Math.sin(i * 0.7) * 0.06 - Math.random() * 0.05)),
  }));
}

const JOBS = [
  {
    id: "job_01HX9FQ7AZK3M2N0JRT8WBCDX9",
    alias: "nightly-etl-warehouse",
    description: "Hydrates the analytics warehouse from production replicas",
    paused: false,
    schedule: "0 2 * * *",
    schedule_human: "Every day at 02:00 UTC",
    last_runs: [
      { id: "run_8B3F2A", status: "succeeded", started_at: minutes(180), duration: 743, cache_hits: 12, total_tasks: 18 },
      { id: "run_7C2A1D", status: "succeeded", started_at: minutes(1620), duration: 802, cache_hits: 9, total_tasks: 18 },
      { id: "run_6D1E0F", status: "succeeded", started_at: minutes(3060), duration: 711, cache_hits: 14, total_tasks: 18 },
      { id: "run_5E0F9A", status: "failed",    started_at: minutes(4500), duration: 234, cache_hits: 3,  total_tasks: 18 },
      { id: "run_4F8B2C", status: "succeeded", started_at: minutes(5940), duration: 689, cache_hits: 11, total_tasks: 18 },
      { id: "run_3A7B1D", status: "succeeded", started_at: minutes(7380), duration: 754, cache_hits: 8,  total_tasks: 18 },
      { id: "run_2B6C0E", status: "succeeded", started_at: minutes(8820), duration: 698, cache_hits: 13, total_tasks: 18 },
    ],
    sparkline: spark(7, 22, 4),
  },
  {
    id: "job_01HX9FQAVK3M2N0JRT8WBCDXA1",
    alias: "stripe-events-ingest",
    description: "Streams Stripe webhook backlog into the events lake",
    paused: false,
    schedule: "*/5 * * * *",
    schedule_human: "Every 5 minutes",
    last_runs: [
      { id: "run_9C4D3B", status: "running",   started_at: seconds(82),  duration: null, cache_hits: 2, total_tasks: 6 },
      { id: "run_8B3D2A", status: "succeeded", started_at: minutes(5),   duration: 41,  cache_hits: 1, total_tasks: 6 },
      { id: "run_7A2C1B", status: "succeeded", started_at: minutes(10),  duration: 38,  cache_hits: 2, total_tasks: 6 },
      { id: "run_6F1B0A", status: "succeeded", started_at: minutes(15),  duration: 44,  cache_hits: 1, total_tasks: 6 },
      { id: "run_5E0A9F", status: "succeeded", started_at: minutes(20),  duration: 39,  cache_hits: 2, total_tasks: 6 },
      { id: "run_4D9F8E", status: "succeeded", started_at: minutes(25),  duration: 42,  cache_hits: 0, total_tasks: 6 },
      { id: "run_3C8E7D", status: "succeeded", started_at: minutes(30),  duration: 37,  cache_hits: 1, total_tasks: 6 },
    ],
    sparkline: spark(7, 288, 8),
  },
  {
    id: "job_01HX9FQB1K3M2N0JRT8WBCDXB2",
    alias: "ml-feature-rollup",
    description: "Computes hourly aggregate features for the ranking model",
    paused: false,
    schedule: "0 * * * *",
    schedule_human: "Every hour",
    last_runs: [
      { id: "run_AA1B2C", status: "succeeded", started_at: minutes(12),  duration: 412, cache_hits: 4, total_tasks: 9 },
      { id: "run_BB2C3D", status: "succeeded", started_at: minutes(72),  duration: 398, cache_hits: 5, total_tasks: 9 },
      { id: "run_CC3D4E", status: "succeeded", started_at: minutes(132), duration: 421, cache_hits: 3, total_tasks: 9 },
      { id: "run_DD4E5F", status: "failed",    started_at: minutes(192), duration: 87,  cache_hits: 1, total_tasks: 9 },
      { id: "run_EE5F6A", status: "succeeded", started_at: minutes(252), duration: 405, cache_hits: 4, total_tasks: 9 },
      { id: "run_FF6A7B", status: "succeeded", started_at: minutes(312), duration: 416, cache_hits: 5, total_tasks: 9 },
      { id: "run_AB7B8C", status: "succeeded", started_at: minutes(372), duration: 393, cache_hits: 4, total_tasks: 9 },
    ],
    sparkline: spark(7, 24, 2),
  },
  {
    id: "job_01HX9FQC2K3M2N0JRT8WBCDXC3",
    alias: "session-rollups-hourly",
    description: "Sessionizes raw clickstream events into rolled-up bundles",
    paused: true,
    schedule: "15 * * * *",
    schedule_human: "Every hour at :15",
    last_runs: [
      { id: "run_BC8C9D", status: "succeeded", started_at: minutes(540), duration: 268, cache_hits: 2, total_tasks: 7 },
      { id: "run_CD9D0E", status: "succeeded", started_at: minutes(600), duration: 275, cache_hits: 3, total_tasks: 7 },
      { id: "run_DE0E1F", status: "succeeded", started_at: minutes(660), duration: 264, cache_hits: 2, total_tasks: 7 },
      { id: "run_EF1F2A", status: "succeeded", started_at: minutes(720), duration: 281, cache_hits: 4, total_tasks: 7 },
      { id: "run_FA2A3B", status: "succeeded", started_at: minutes(780), duration: 269, cache_hits: 3, total_tasks: 7 },
      { id: "run_AB3B4C", status: "succeeded", started_at: minutes(840), duration: 273, cache_hits: 2, total_tasks: 7 },
    ],
    sparkline: spark(7, 24, 1),
  },
  {
    id: "job_01HX9FQD3K3M2N0JRT8WBCDXD4",
    alias: "kafka-cdc-replay",
    description: "Replays the CDC topic into the lakehouse staging tables",
    paused: false,
    schedule: null,
    schedule_human: "On webhook /v1/triggers/cdc-replay",
    last_runs: [
      { id: "run_BC4C5D", status: "running",   started_at: seconds(420), duration: null, cache_hits: 0, total_tasks: 12 },
      { id: "run_CD5D6E", status: "failed",    started_at: minutes(45),  duration: 218, cache_hits: 1, total_tasks: 12 },
      { id: "run_DE6E7F", status: "succeeded", started_at: minutes(180), duration: 1102, cache_hits: 6, total_tasks: 12 },
      { id: "run_EF7F8A", status: "succeeded", started_at: minutes(360), duration: 1087, cache_hits: 5, total_tasks: 12 },
      { id: "run_FA8A9B", status: "succeeded", started_at: minutes(720), duration: 1124, cache_hits: 7, total_tasks: 12 },
    ],
    sparkline: spark(7, 4, 3),
  },
  {
    id: "job_01HX9FQE4K3M2N0JRT8WBCDXE5",
    alias: "dbt-models-build",
    description: "Compiles and runs the dbt project against Snowflake",
    paused: false,
    schedule: "30 3 * * *",
    schedule_human: "Every day at 03:30 UTC",
    last_runs: [
      { id: "run_AB9B0C", status: "succeeded", started_at: minutes(150), duration: 1843, cache_hits: 18, total_tasks: 42 },
      { id: "run_BC0C1D", status: "succeeded", started_at: minutes(1590), duration: 1922, cache_hits: 22, total_tasks: 42 },
      { id: "run_CD1D2E", status: "succeeded", started_at: minutes(3030), duration: 1798, cache_hits: 20, total_tasks: 42 },
      { id: "run_DE2E3F", status: "succeeded", started_at: minutes(4470), duration: 1856, cache_hits: 19, total_tasks: 42 },
      { id: "run_EF3F4A", status: "succeeded", started_at: minutes(5910), duration: 1881, cache_hits: 21, total_tasks: 42 },
    ],
    sparkline: spark(7, 1, 0.2),
  },
  {
    id: "job_01HX9FQF5K3M2N0JRT8WBCDXF6",
    alias: "prod-replica-snapshot",
    description: "Daily zfs snapshot of prod read-replicas to S3 Glacier",
    paused: false,
    schedule: "0 4 * * *",
    schedule_human: "Every day at 04:00 UTC",
    last_runs: [
      { id: "run_BC2C3D", status: "succeeded", started_at: minutes(220), duration: 2410, cache_hits: 0, total_tasks: 4 },
      { id: "run_CD3D4E", status: "succeeded", started_at: minutes(1660), duration: 2387, cache_hits: 0, total_tasks: 4 },
      { id: "run_DE4E5F", status: "succeeded", started_at: minutes(3100), duration: 2452, cache_hits: 0, total_tasks: 4 },
    ],
    sparkline: spark(7, 1, 0.1),
  },
  {
    id: "job_01HX9FQG6K3M2N0JRT8WBCDXG7",
    alias: "search-index-rebuild",
    description: "Rebuilds the OpenSearch product catalog index from Postgres",
    paused: false,
    schedule: "0 */6 * * *",
    schedule_human: "Every 6 hours",
    last_runs: [
      { id: "run_AB5B6C", status: "queued",    started_at: seconds(8),   duration: null, cache_hits: 0, total_tasks: 8 },
      { id: "run_BC6C7D", status: "succeeded", started_at: minutes(360), duration: 612, cache_hits: 2, total_tasks: 8 },
      { id: "run_CD7D8E", status: "succeeded", started_at: minutes(720), duration: 598, cache_hits: 3, total_tasks: 8 },
      { id: "run_DE8E9F", status: "succeeded", started_at: minutes(1080), duration: 605, cache_hits: 2, total_tasks: 8 },
    ],
    sparkline: spark(7, 4, 1),
  },
];

// DAG for nightly-etl-warehouse — used in detail view
const DAG = {
  nodes: [
    { id: "extract.users",       label: "extract.users",       lane: 0, x: 0, y: 0, kind: "task", image: "alpine:3.20", status: "succeeded", duration: 42 },
    { id: "extract.orders",      label: "extract.orders",      lane: 0, x: 0, y: 1, kind: "task", image: "alpine:3.20", status: "succeeded", duration: 51 },
    { id: "extract.events",      label: "extract.events",      lane: 0, x: 0, y: 2, kind: "task", image: "alpine:3.20", status: "succeeded", duration: 38, cached: true },
    { id: "transform.users",     label: "transform.users",     lane: 1, x: 1, y: 0, kind: "task", image: "ghcr.io/cs/dbt:1.7", status: "succeeded", duration: 87 },
    { id: "transform.orders",    label: "transform.orders",    lane: 1, x: 1, y: 1, kind: "task", image: "ghcr.io/cs/dbt:1.7", status: "succeeded", duration: 112 },
    { id: "transform.events",    label: "transform.events",    lane: 1, x: 1, y: 2, kind: "task", image: "ghcr.io/cs/dbt:1.7", status: "running",   duration: null },
    { id: "join.user_orders",    label: "join.user_orders",    lane: 2, x: 2, y: 0, kind: "task", image: "ghcr.io/cs/spark:3.5", status: "queued", duration: null },
    { id: "load.warehouse",      label: "load.warehouse",      lane: 3, x: 3, y: 1, kind: "task", image: "snowflake/snowsql", status: "queued", duration: null },
    { id: "notify.slack",        label: "notify.slack",        lane: 4, x: 4, y: 1, kind: "task", image: "alpine:3.20", status: "queued", duration: null },
  ],
  edges: [
    { from: "extract.users", to: "transform.users" },
    { from: "extract.orders", to: "transform.orders" },
    { from: "extract.events", to: "transform.events" },
    { from: "transform.users", to: "join.user_orders" },
    { from: "transform.orders", to: "join.user_orders" },
    { from: "transform.events", to: "load.warehouse" },
    { from: "join.user_orders", to: "load.warehouse" },
    { from: "load.warehouse", to: "notify.slack" },
  ],
};

const TRIGGERS = [
  { id: "trg_01ABCDEF", alias: "nightly-2am", type: "cron", config: "0 2 * * *", target: "nightly-etl-warehouse", paused: false },
  { id: "trg_02ABCDEG", alias: "stripe-webhook", type: "webhook", config: "POST /v1/triggers/stripe", target: "stripe-events-ingest", paused: false },
  { id: "trg_03ABCDEH", alias: "hourly-features", type: "cron", config: "0 * * * *", target: "ml-feature-rollup", paused: false },
  { id: "trg_04ABCDEI", alias: "session-quarter", type: "cron", config: "15 * * * *", target: "session-rollups-hourly", paused: true },
  { id: "trg_05ABCDEJ", alias: "cdc-replay", type: "webhook", config: "POST /v1/triggers/cdc-replay", target: "kafka-cdc-replay", paused: false },
  { id: "trg_06ABCDEK", alias: "dbt-3-30", type: "cron", config: "30 3 * * *", target: "dbt-models-build", paused: false },
  { id: "trg_07ABCDEL", alias: "snapshot-4am", type: "cron", config: "0 4 * * *", target: "prod-replica-snapshot", paused: false },
  { id: "trg_08ABCDEM", alias: "search-6hr", type: "cron", config: "0 */6 * * *", target: "search-index-rebuild", paused: false },
];

// Activity feed (live events)
const ACTIVITY = [
  { t: seconds(8),   kind: "queued",   msg: "search-index-rebuild queued",                     job: "search-index-rebuild" },
  { t: seconds(82),  kind: "started",  msg: "stripe-events-ingest run started",                job: "stripe-events-ingest" },
  { t: seconds(150), kind: "cached",   msg: "transform.events served from cache (saved 38s)",  job: "ml-feature-rollup" },
  { t: seconds(420), kind: "started",  msg: "kafka-cdc-replay run started (manual)",           job: "kafka-cdc-replay" },
  { t: minutes(5),   kind: "success",  msg: "stripe-events-ingest run_8B3D2A succeeded",       job: "stripe-events-ingest" },
  { t: minutes(12),  kind: "success",  msg: "ml-feature-rollup run_AA1B2C succeeded",          job: "ml-feature-rollup" },
  { t: minutes(45),  kind: "failed",   msg: "kafka-cdc-replay run_CD5D6E failed: timeout",     job: "kafka-cdc-replay" },
  { t: minutes(180), kind: "success",  msg: "nightly-etl-warehouse run_8B3F2A succeeded",      job: "nightly-etl-warehouse" },
];

// Stats
const STATS = {
  totals: { jobs: 8, recent_runs_24h: 1842, success_rate: 0.974, avg_duration_s: 187.4 },
  trend: Array.from({ length: 30 }, (_, i) => ({
    day: i,
    runs: 240 + Math.round(Math.sin(i / 3) * 30 + Math.random() * 20),
    success: Math.max(0.86, Math.min(0.999, 0.97 + Math.sin(i / 2) * 0.02 - Math.random() * 0.015)),
  })),
  top_failing: [
    { alias: "kafka-cdc-replay", count: 14 },
    { alias: "ml-feature-rollup", count: 6 },
    { alias: "nightly-etl-warehouse", count: 3 },
    { alias: "search-index-rebuild", count: 2 },
  ],
  slowest: [
    { alias: "prod-replica-snapshot", avg: 2410 },
    { alias: "dbt-models-build", avg: 1860 },
    { alias: "kafka-cdc-replay", avg: 1102 },
    { alias: "nightly-etl-warehouse", avg: 743 },
  ],
};

const LOG_LINES = [
  { t: seconds(82), level: "INFO",  msg: "task transform.events: scheduling on docker engine" },
  { t: seconds(81), level: "INFO",  msg: "task transform.events: pulling image ghcr.io/cs/dbt:1.7" },
  { t: seconds(78), level: "INFO",  msg: "task transform.events: container c8a7f3 started" },
  { t: seconds(75), level: "INFO",  msg: "stdout: dbt: 1.7.4 — initializing project..." },
  { t: seconds(72), level: "INFO",  msg: "stdout: Found 14 models, 0 tests, 0 snapshots, 0 analyses" },
  { t: seconds(68), level: "INFO",  msg: "stdout: Concurrency: 4 threads (target='warehouse')" },
  { t: seconds(60), level: "DEBUG", msg: "task transform.events: heartbeat ok (cpu=42% mem=712Mi)" },
  { t: seconds(45), level: "INFO",  msg: "stdout: 1 of 14 OK created sql incremental model events.sessions ......... [OK in 4.21s]" },
  { t: seconds(38), level: "INFO",  msg: "stdout: 2 of 14 OK created sql incremental model events.pageviews ........ [OK in 3.87s]" },
  { t: seconds(28), level: "INFO",  msg: "stdout: 3 of 14 OK created sql view model events.daily_active ............ [OK in 0.94s]" },
  { t: seconds(20), level: "WARN",  msg: "stdout: events.fct_funnel: depends_on resolved against stale partition" },
  { t: seconds(12), level: "DEBUG", msg: "task transform.events: heartbeat ok (cpu=51% mem=801Mi)" },
  { t: seconds(4),  level: "INFO",  msg: "stdout: 4 of 14 RUNNING incremental model events.fct_funnel" },
];

const SYSTEM = {
  uptime: "12d 04h 37m",
  active_runs: 7,
  queued_runs: 3,
  triggers_count: 24,
  cron_count: 18,
  webhook_count: 6,
  db: { latency_ms: 1.4, size_mb: 384 },
  nodes: [
    { address: "10.0.4.21:9001", role: "leader", arch: "amd64", cpu: 42, mem: 61, workers_busy: 5, workers_total: 8, uptime: "12d" },
    { address: "10.0.4.22:9001", role: "voter",  arch: "amd64", cpu: 28, mem: 47, workers_busy: 3, workers_total: 8, uptime: "12d" },
    { address: "10.0.4.23:9001", role: "voter",  arch: "arm64", cpu: 71, mem: 52, workers_busy: 6, workers_total: 8, uptime: "8d" },
    { address: "10.0.4.24:9001", role: "voter",  arch: "amd64", cpu: 19, mem: 38, workers_busy: 2, workers_total: 8, uptime: "12d" },
    { address: "10.0.4.25:9001", role: "voter",  arch: "arm64", cpu: 56, mem: 64, workers_busy: 4, workers_total: 8, uptime: "5d" },
  ],
  checks: [
    { key: "api.responsive",         ok: true, detail: "p99 14ms" },
    { key: "scheduler.healthy",      ok: true, detail: "tick 250ms" },
    { key: "dispatcher.queue_depth", ok: true, detail: "3 / 1024" },
    { key: "raft.quorum",            ok: true, detail: "5/5 voters" },
    { key: "object_store.reachable", ok: true, detail: "s3 us-east-1" },
    { key: "registry.reachable",     ok: true, detail: "ghcr.io" },
  ],
  metrics: [
    "caesium_jobs_total", "caesium_runs_total{status}", "caesium_runs_active",
    "caesium_run_duration_seconds", "caesium_atom_runs_total{status}",
    "caesium_dispatcher_queue_depth", "caesium_scheduler_tick_seconds",
    "caesium_db_size_bytes", "caesium_db_query_seconds", "caesium_raft_leader",
    "caesium_workers_busy", "caesium_workers_total",
  ],
};

window.MOCK = { JOBS, DAG, TRIGGERS, ACTIVITY, STATS, LOG_LINES, SYSTEM };
