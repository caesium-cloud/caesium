/* === Caesium Refresh — Pages: System, JobDefs === */

const { useState: usSj, useEffect: usEj, useMemo: usMj, useMemo } = React;
const { Btn: B3, StatusBadge: SB3, I: I3, AtomLogo: AL3, EmptyState: ES3, timeAgo: ta3, fmtDuration: fd3 } = window.UI;

/* ================================================================== */
/* SYSTEM PAGE — fleet health, nodes, operator tools, metrics         */
/* ================================================================== */
function SystemPage() {
  const [refreshTick, setRefreshTick] = usSj(0);
  const [pruneOpen, setPruneOpen] = usSj(false);
  const lastUpdated = usMj(() => new Date(), [refreshTick]);
  const sys = window.MOCK.SYSTEM;

  return (
    <div className="fade-up" style={{ padding: "20px 28px", display: "flex", flexDirection: "column", gap: 18, minHeight: "100%", overflow: "auto" }}>
      {/* Title */}
      <div style={{ display: "flex", alignItems: "flex-end", justifyContent: "space-between", gap: 16 }}>
        <div>
          <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>Cluster · Health</div>
          <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0", letterSpacing: "-0.01em" }}>System</h1>
          <p style={{ margin: "4px 0 0", color: "hsl(var(--text-3))", fontSize: 13 }}>Fleet observability, operator tools, and Prometheus reference</p>
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
          <span style={{ fontSize: 11, color: "hsl(var(--text-3))" }}>Updated {ta3(lastUpdated.getTime())}</span>
          <B3 variant="outline" size="md" icon={I3.refresh} onClick={() => setRefreshTick(t => t + 1)}>Refresh</B3>
        </div>
      </div>

      {/* Health banner */}
      <div style={{
        display: "flex", alignItems: "center", gap: 14, padding: "14px 18px",
        background: "linear-gradient(90deg, hsl(var(--success) / 0.12), hsl(var(--success) / 0.02))",
        border: "1px solid hsl(var(--success) / 0.35)", borderRadius: 10,
      }}>
        <div style={{ position: "relative", width: 34, height: 34, display: "flex", alignItems: "center", justifyContent: "center" }}>
          <span style={{ position: "absolute", inset: 0, borderRadius: "50%", background: "hsl(var(--success) / 0.18)", animation: "pulse 2.4s ease-out infinite" }} />
          <I3.check width={18} height={18} style={{ color: "hsl(var(--success))", position: "relative" }} />
        </div>
        <div style={{ flex: 1 }}>
          <div style={{ fontSize: 14, fontWeight: 500, color: "hsl(var(--text-1))" }}>All systems operational</div>
          <div className="mono" style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>
            Uptime <span className="tnum" style={{ color: "hsl(var(--text-2))" }}>{sys.uptime}</span>
            <span style={{ margin: "0 8px", color: "hsl(var(--text-4))" }}>·</span>
            Last incident <span style={{ color: "hsl(var(--text-2))" }}>9 days ago</span>
            <span style={{ margin: "0 8px", color: "hsl(var(--text-4))" }}>·</span>
            Auth mode <span style={{ color: "hsl(var(--cyan-glow))" }}>api-key</span>
          </div>
        </div>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6, padding: "4px 10px", borderRadius: 4, background: "hsl(var(--success) / 0.15)", color: "hsl(var(--success))", fontSize: 11, fontWeight: 600, letterSpacing: "0.08em", textTransform: "uppercase" }}>
          Healthy
        </span>
      </div>

      {/* KPI row */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 12 }}>
        <SysKpi icon={I3.database} label="Database" value={<span className="capitalize" style={{ color: "hsl(var(--success))" }}>healthy</span>} sub={`${sys.db.latency_ms}ms · ${sys.db.size_mb}MB`} dot="ok" />
        <SysKpi icon={I3.activity} label="Active runs" value={<span className="tnum">{sys.active_runs}</span>} sub={`${sys.queued_runs} queued`} accent="cyan" />
        <SysKpi icon={I3.zap} label="Triggers" value={<span className="tnum">{sys.triggers_count}</span>} sub={`${sys.cron_count} cron · ${sys.webhook_count} webhook`} />
        <SysKpi icon={I3.server} label="Nodes" value={<span className="tnum">{sys.nodes.length}</span>} sub={`${sys.nodes.filter(n => n.role === "leader").length} leader · ${sys.nodes.filter(n => n.role === "voter").length} voter`} dot="ok" />
      </div>

      {/* Two-up: Nodes table + Cluster topology */}
      <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: 14 }}>
        <div className="surface-elev" style={{ overflow: "hidden" }}>
          <div style={{ padding: "12px 16px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <div>
              <div style={{ fontSize: 13, fontWeight: 500 }}>Cluster nodes</div>
              <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>dqlite quorum · mixed amd64/arm64</div>
            </div>
            <span className="mono tnum" style={{ fontSize: 11, color: "hsl(var(--text-3))" }}>{sys.nodes.length} total</span>
          </div>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 90px 80px 80px 90px 110px", padding: "8px 16px", background: "hsl(var(--obsidian) / 0.5)", borderBottom: "1px solid hsl(var(--graphite))", fontSize: 10, fontWeight: 500, letterSpacing: "0.16em", textTransform: "uppercase", color: "hsl(var(--text-3))" }}>
            <span>Address</span><span>Role</span><span>Arch</span><span>CPU</span><span>Mem</span><span>Workers</span>
          </div>
          {sys.nodes.map((n, i) => (
            <div key={n.address} style={{
              display: "grid", gridTemplateColumns: "1fr 90px 80px 80px 90px 110px",
              padding: "12px 16px", alignItems: "center",
              borderBottom: i === sys.nodes.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.5)",
            }}>
              <div style={{ display: "flex", alignItems: "center", gap: 10, minWidth: 0 }}>
                <span style={{ width: 8, height: 8, borderRadius: "50%", background: "hsl(var(--success))", boxShadow: "0 0 6px hsl(var(--success) / 0.6)", flexShrink: 0 }} />
                <div style={{ minWidth: 0 }}>
                  <div className="mono" style={{ fontSize: 12, color: "hsl(var(--text-1))", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{n.address}</div>
                  <div className="mono" style={{ fontSize: 10, color: "hsl(var(--text-4))" }}>uptime {n.uptime}</div>
                </div>
              </div>
              <span style={{
                display: "inline-flex", padding: "2px 8px", borderRadius: 4, fontSize: 10, fontWeight: 600,
                letterSpacing: "0.1em", textTransform: "uppercase",
                background: n.role === "leader" ? "hsl(var(--gold) / 0.15)" : "hsl(var(--cyan) / 0.1)",
                color: n.role === "leader" ? "hsl(var(--gold))" : "hsl(var(--cyan-glow))",
                border: `1px solid ${n.role === "leader" ? "hsl(var(--gold) / 0.3)" : "hsl(var(--cyan) / 0.25)"}`,
                width: "fit-content",
              }}>{n.role}</span>
              <span className="mono" style={{ fontSize: 11, color: "hsl(var(--text-2))" }}>{n.arch}</span>
              <UsageBar value={n.cpu} />
              <UsageBar value={n.mem} />
              <span className="mono tnum" style={{ fontSize: 12, color: "hsl(var(--text-2))" }}>{n.workers_busy}/{n.workers_total}</span>
            </div>
          ))}
        </div>

        {/* Cluster topology mini-map */}
        <div className="surface-elev" style={{ padding: 16 }}>
          <div style={{ fontSize: 13, fontWeight: 500, marginBottom: 4 }}>Topology</div>
          <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginBottom: 14 }}>Quorum view</div>
          <ClusterTopology nodes={sys.nodes} />
        </div>
      </div>

      {/* Operator tools */}
      <div className="surface-elev" style={{
        padding: 18, position: "relative", overflow: "hidden",
        border: "1px solid hsl(var(--cyan) / 0.25)",
        background: "linear-gradient(135deg, hsl(var(--midnight)) 0%, hsl(var(--cyan) / 0.04) 100%)",
      }}>
        <div style={{ position: "absolute", top: -40, right: -40, width: 200, height: 200, opacity: 0.05 }}>
          <AL3 size={200} animated={false} />
        </div>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16, position: "relative" }}>
          <div>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <I3.terminal width={16} height={16} style={{ color: "hsl(var(--cyan-glow))" }} />
              <span style={{ fontSize: 14, fontWeight: 500 }}>Operator tools</span>
            </div>
            <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 4 }}>Read-only debugging surfaces — gated by environment variables</div>
          </div>
          <span style={{ fontSize: 10, fontWeight: 600, letterSpacing: "0.1em", textTransform: "uppercase", padding: "3px 8px", borderRadius: 3, border: "1px solid hsl(var(--gold) / 0.4)", color: "hsl(var(--gold))", background: "hsl(var(--gold) / 0.05)" }}>Power feature</span>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 12, position: "relative" }}>
          <ToolCard icon={I3.database} title="Database console" desc="Schema-aware SQL with read-only safeguards." env="CAESIUM_DATABASE_CONSOLE_ENABLED" enabled />
          <ToolCard icon={I3.scroll} title="Log console" desc="Stream server logs in real time with level filtering." env="CAESIUM_LOG_CONSOLE_ENABLED" enabled />
          <ToolCard icon={I3.broom} title="Cache maintenance" desc="Prune expired task cache entries across all jobs." onClick={() => setPruneOpen(true)} />
        </div>
      </div>

      {/* Health checks + Prometheus metrics */}
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 14 }}>
        <div className="surface-elev">
          <div style={{ padding: "14px 16px 10px", borderBottom: "1px solid hsl(var(--graphite))" }}>
            <div style={{ fontSize: 13, fontWeight: 500 }}>Health checks</div>
            <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>Polled every 15s · all green</div>
          </div>
          <div>
            {sys.checks.map((c, i) => (
              <div key={c.key} style={{
                display: "flex", justifyContent: "space-between", alignItems: "center",
                padding: "12px 16px",
                borderBottom: i === sys.checks.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.4)",
              }}>
                <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                  <span style={{ width: 7, height: 7, borderRadius: "50%", background: c.ok ? "hsl(var(--success))" : "hsl(var(--danger))", boxShadow: c.ok ? "0 0 6px hsl(var(--success) / 0.6)" : "0 0 6px hsl(var(--danger) / 0.6)" }} />
                  <span style={{ fontSize: 13 }}>{c.key}</span>
                </div>
                <span className="mono tnum" style={{ fontSize: 11, color: "hsl(var(--text-3))" }}>{c.detail}</span>
              </div>
            ))}
          </div>
        </div>

        <div className="surface-elev">
          <div style={{ padding: "14px 16px 10px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <div>
              <div style={{ fontSize: 13, fontWeight: 500, display: "flex", alignItems: "center", gap: 8 }}>
                <I3.clock width={13} height={13} style={{ color: "hsl(var(--text-3))" }} />
                Prometheus metrics
              </div>
              <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>
                Exposed at <code className="mono" style={{ background: "hsl(var(--obsidian))", padding: "1px 5px", borderRadius: 3, color: "hsl(var(--cyan-glow))" }}>/metrics</code>
              </div>
            </div>
            <B3 variant="ghost" size="sm">Open</B3>
          </div>
          <div style={{ padding: 14, display: "flex", flexWrap: "wrap", gap: 6 }}>
            {sys.metrics.map((m) => (
              <code key={m} className="mono" style={{
                fontSize: 10.5, padding: "4px 8px", borderRadius: 4,
                background: "hsl(var(--obsidian))",
                border: "1px solid hsl(var(--graphite))",
                color: "hsl(var(--text-2))",
              }}>{m}</code>
            ))}
          </div>
        </div>
      </div>

      {/* Prune dialog */}
      {pruneOpen ? <PruneDialog onClose={() => setPruneOpen(false)} /> : null}
    </div>
  );
}

function SysKpi({ icon: Icon, label, value, sub, dot, accent }) {
  return (
    <div className="surface-elev" style={{ padding: 14 }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 8 }}>
        <span className="eyebrow">{label}</span>
        <Icon width={13} height={13} style={{ color: "hsl(var(--text-3))" }} />
      </div>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        {dot ? <span style={{ width: 7, height: 7, borderRadius: "50%", background: "hsl(var(--success))", boxShadow: "0 0 6px hsl(var(--success) / 0.6)" }} /> : null}
        <div style={{ fontSize: 22, fontWeight: 500, color: accent === "cyan" ? "hsl(var(--cyan-glow))" : "hsl(var(--text-1))", letterSpacing: "-0.01em" }}>{value}</div>
      </div>
      <div className="mono tnum" style={{ marginTop: 4, fontSize: 11, color: "hsl(var(--text-3))" }}>{sub}</div>
    </div>
  );
}

function UsageBar({ value }) {
  const color = value > 85 ? "hsl(var(--danger))" : value > 65 ? "hsl(var(--gold))" : "hsl(var(--cyan))";
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
      <div style={{ flex: 1, height: 4, background: "hsl(var(--graphite) / 0.5)", borderRadius: 2, overflow: "hidden" }}>
        <div style={{ width: `${value}%`, height: "100%", background: color, borderRadius: 2 }} />
      </div>
      <span className="mono tnum" style={{ fontSize: 10, color: "hsl(var(--text-3))", minWidth: 26, textAlign: "right" }}>{value}%</span>
    </div>
  );
}

function ClusterTopology({ nodes }) {
  const cx = 110, cy = 110, R = 70;
  return (
    <svg viewBox="0 0 220 220" style={{ width: "100%", height: 220 }}>
      <defs>
        <radialGradient id="topo-glow" cx="0.5" cy="0.5" r="0.5">
          <stop offset="0%" stopColor="hsl(var(--cyan))" stopOpacity="0.3" />
          <stop offset="100%" stopColor="hsl(var(--cyan))" stopOpacity="0" />
        </radialGradient>
      </defs>
      {/* connections */}
      {nodes.map((n1, i) => nodes.slice(i + 1).map((n2, j) => {
        const a1 = (i / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const a2 = ((i + j + 1) / nodes.length) * Math.PI * 2 - Math.PI / 2;
        return (
          <line key={`${i}-${j}`}
            x1={cx + Math.cos(a1) * R} y1={cy + Math.sin(a1) * R}
            x2={cx + Math.cos(a2) * R} y2={cy + Math.sin(a2) * R}
            stroke="hsl(var(--cyan) / 0.3)" strokeWidth="1" strokeDasharray="2 3"
          />
        );
      }))}
      {/* center label */}
      <circle cx={cx} cy={cy} r="40" fill="url(#topo-glow)" />
      <text x={cx} y={cy - 2} textAnchor="middle" fontSize="9" fill="hsl(var(--text-3))" style={{ letterSpacing: "0.18em", textTransform: "uppercase", fontWeight: 600 }}>quorum</text>
      <text x={cx} y={cy + 12} textAnchor="middle" fontSize="14" fill="hsl(var(--text-1))" fontWeight="500" className="tnum">{nodes.length}/{nodes.length}</text>
      {/* nodes */}
      {nodes.map((n, i) => {
        const angle = (i / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const x = cx + Math.cos(angle) * R;
        const y = cy + Math.sin(angle) * R;
        const isLeader = n.role === "leader";
        return (
          <g key={n.address}>
            <circle cx={x} cy={y} r="14" fill={isLeader ? "hsl(var(--gold))" : "hsl(var(--cyan))"} opacity="0.18" />
            <circle cx={x} cy={y} r="8" fill={isLeader ? "hsl(var(--gold))" : "hsl(var(--cyan))"} stroke="hsl(var(--midnight))" strokeWidth="2" />
            <text x={x} y={y + Math.sin(angle) * 24 + (Math.sin(angle) > 0 ? 8 : -2)} textAnchor="middle" fontSize="9" fill="hsl(var(--text-2))" className="mono">{n.address.split(":")[0]}</text>
          </g>
        );
      })}
    </svg>
  );
}

function ToolCard({ icon: Icon, title, desc, env, enabled, onClick }) {
  return (
    <div onClick={onClick} style={{
      padding: 14, borderRadius: 8, cursor: onClick ? "pointer" : "default",
      background: "hsl(var(--obsidian) / 0.6)",
      border: "1px solid hsl(var(--graphite))",
      transition: "all 160ms",
      display: "flex", flexDirection: "column", gap: 8,
    }}
      onMouseEnter={(e) => { if (onClick) { e.currentTarget.style.borderColor = "hsl(var(--cyan) / 0.4)"; e.currentTarget.style.background = "hsl(var(--obsidian))"; } }}
      onMouseLeave={(e) => { if (onClick) { e.currentTarget.style.borderColor = "hsl(var(--graphite))"; e.currentTarget.style.background = "hsl(var(--obsidian) / 0.6)"; } }}
    >
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <Icon width={16} height={16} style={{ color: "hsl(var(--cyan-glow))" }} />
        {enabled != null ? (
          <span style={{
            fontSize: 9, fontWeight: 600, letterSpacing: "0.12em", textTransform: "uppercase",
            padding: "2px 6px", borderRadius: 3,
            background: enabled ? "hsl(var(--success) / 0.15)" : "hsl(var(--graphite))",
            color: enabled ? "hsl(var(--success))" : "hsl(var(--text-3))",
          }}>{enabled ? "Enabled" : "Disabled"}</span>
        ) : null}
      </div>
      <div>
        <div style={{ fontSize: 13, fontWeight: 500, color: "hsl(var(--text-1))" }}>{title}</div>
        <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 4, lineHeight: 1.5 }}>{desc}</div>
      </div>
      {env ? (
        <code className="mono" style={{ fontSize: 10, color: "hsl(var(--text-4))", background: "hsl(var(--void))", padding: "3px 6px", borderRadius: 3, marginTop: "auto", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{env}</code>
      ) : null}
    </div>
  );
}

function PruneDialog({ onClose }) {
  return (
    <div onClick={onClose} style={{ position: "fixed", inset: 0, background: "hsl(0 0% 0% / 0.6)", zIndex: 200, display: "flex", alignItems: "center", justifyContent: "center", backdropFilter: "blur(4px)" }}>
      <div onClick={(e) => e.stopPropagation()} style={{
        width: 440, background: "hsl(var(--midnight))",
        border: "1px solid hsl(var(--graphite))", borderRadius: 12,
        boxShadow: "0 20px 60px hsl(0 0% 0% / 0.5)", overflow: "hidden",
      }}>
        <div style={{ padding: "16px 20px", borderBottom: "1px solid hsl(var(--graphite))" }}>
          <div style={{ fontSize: 15, fontWeight: 500 }}>Prune expired cache entries</div>
          <div style={{ fontSize: 12, color: "hsl(var(--text-3))", marginTop: 4 }}>This removes only expired records across all jobs. Active cache entries are not affected.</div>
        </div>
        <div style={{ padding: "20px", display: "flex", justifyContent: "flex-end", gap: 8 }}>
          <B3 variant="ghost" onClick={onClose}>Cancel</B3>
          <B3 variant="primary" onClick={onClose} icon={I3.broom}>Confirm prune</B3>
        </div>
      </div>
    </div>
  );
}

/* ================================================================== */
/* JOBDEFS PAGE — manifest editor + apply                             */
/* ================================================================== */
const EXAMPLE_YAML = `apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl-warehouse
  labels:
    team: data-platform
    tier: critical
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: "UTC"
steps:
  - name: extract.users
    image: ghcr.io/cs/postgres-extractor:1.4
    command: ["python", "/app/extract.py", "--table=users"]
  - name: extract.orders
    image: ghcr.io/cs/postgres-extractor:1.4
    command: ["python", "/app/extract.py", "--table=orders"]
  - name: transform.users
    image: ghcr.io/cs/dbt:1.7
    dependsOn: [extract.users]
    command: ["dbt", "run", "--select", "users"]
  - name: transform.orders
    image: ghcr.io/cs/dbt:1.7
    dependsOn: [extract.orders]
    command: ["dbt", "run", "--select", "orders"]
  - name: load.warehouse
    image: snowflake/snowsql
    dependsOn: [transform.users, transform.orders]
    command: ["snowsql", "-f", "/sql/load.sql"]
`;

function JobDefsPage() {
  const [yaml, setYaml] = usSj(EXAMPLE_YAML);
  const [tab, setTab] = usSj("editor"); // editor | diff | history
  const [result, setResult] = usSj(null);
  const [applying, setApplying] = usSj(false);

  const lineCount = yaml.split("\n").length;
  const lint = useMemo(() => lintYaml(yaml), [yaml]);
  const diff = useMemo(() => synthDiff(yaml), [yaml]);

  const apply = () => {
    if (lint.errors.length > 0) return;
    setApplying(true);
    setTimeout(() => {
      setApplying(false);
      setResult({ ok: true, summary: { jobs: 1, atoms: 5, triggers: 1 }, time: 142 });
    }, 700);
  };

  return (
    <div className="fade-up" style={{ padding: "20px 28px", display: "flex", flexDirection: "column", gap: 16, minHeight: "100%", overflow: "auto" }}>
      <div style={{ display: "flex", alignItems: "flex-end", justifyContent: "space-between", gap: 16 }}>
        <div>
          <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>Declarative Manifests</div>
          <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0", letterSpacing: "-0.01em" }}>Job Definitions</h1>
          <p style={{ margin: "4px 0 0", color: "hsl(var(--text-3))", fontSize: 13 }}>Lint, diff, and apply YAML manifests · <code className="mono" style={{ color: "hsl(var(--cyan-glow))" }}>caesium job apply</code></p>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <B3 variant="outline" size="md" icon={I3.upload}>Upload</B3>
          <B3 variant="outline" size="md" icon={I3.git}>Git sync</B3>
          <B3 variant="primary" size="md" icon={applying ? I3.spinner : I3.play} onClick={apply} disabled={lint.errors.length > 0 || applying}>
            {applying ? "Applying…" : "Apply definition"}
          </B3>
        </div>
      </div>

      {/* Tabs */}
      <div style={{ display: "flex", gap: 2, padding: 3, background: "hsl(var(--obsidian))", border: "1px solid hsl(var(--graphite))", borderRadius: 8, width: "fit-content" }}>
        {[
          { k: "editor", l: "Editor", c: lint.errors.length, ctone: "danger" },
          { k: "diff", l: "Diff vs server", c: diff.changes.length, ctone: "gold" },
          { k: "history", l: "History" },
        ].map((t) => {
          const on = tab === t.k;
          return (
            <button key={t.k} onClick={() => setTab(t.k)} style={{
              padding: "6px 14px", borderRadius: 6,
              background: on ? "hsl(var(--cyan) / 0.15)" : "transparent",
              color: on ? "hsl(var(--cyan-glow))" : "hsl(var(--text-2))",
              border: "1px solid", borderColor: on ? "hsl(var(--cyan) / 0.3)" : "transparent",
              fontSize: 12, fontWeight: on ? 500 : 400, cursor: "pointer",
              display: "flex", alignItems: "center", gap: 8,
            }}>
              {t.l}
              {t.c != null && t.c > 0 ? (
                <span className="mono tnum" style={{
                  fontSize: 10, padding: "1px 6px", borderRadius: 8,
                  background: t.ctone === "danger" ? "hsl(var(--danger) / 0.2)" : "hsl(var(--gold) / 0.2)",
                  color: t.ctone === "danger" ? "hsl(var(--danger))" : "hsl(var(--gold))",
                }}>{t.c}</span>
              ) : null}
            </button>
          );
        })}
      </div>

      {/* Body */}
      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1fr) 320px", gap: 14, alignItems: "start" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {tab === "editor" ? (
            <>
              <div className="surface-elev" style={{ overflow: "hidden" }}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "10px 14px", borderBottom: "1px solid hsl(var(--graphite))", background: "hsl(var(--obsidian) / 0.5)" }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <I3.file width={13} height={13} style={{ color: "hsl(var(--text-3))" }} />
                    <span className="mono" style={{ fontSize: 12, color: "hsl(var(--text-2))" }}>nightly-etl.job.yaml</span>
                  </div>
                  <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
                    <span className="mono tnum" style={{ fontSize: 10, color: "hsl(var(--text-4))" }}>{lineCount} lines · {(yaml.length / 1024).toFixed(1)} KB</span>
                    <button onClick={() => setYaml(EXAMPLE_YAML)} style={{ background: "transparent", border: "none", color: "hsl(var(--text-3))", fontSize: 11, cursor: "pointer" }}>Reset example</button>
                  </div>
                </div>
                <YamlEditor value={yaml} onChange={setYaml} lint={lint} />
              </div>

              {/* Lint feedback */}
              <div className="surface-elev">
                <div style={{ padding: "10px 14px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                  <div style={{ fontSize: 12, fontWeight: 500, display: "flex", alignItems: "center", gap: 8 }}>
                    {lint.errors.length === 0 ? (
                      <><I3.check width={13} height={13} style={{ color: "hsl(var(--success))" }} /><span>Schema valid · {lint.summary.steps} steps detected</span></>
                    ) : (
                      <><I3.x width={13} height={13} style={{ color: "hsl(var(--danger))" }} /><span>{lint.errors.length} validation {lint.errors.length === 1 ? "error" : "errors"}</span></>
                    )}
                  </div>
                  <span className="mono tnum" style={{ fontSize: 10, color: "hsl(var(--text-3))" }}>linted in 12ms</span>
                </div>
                <div style={{ padding: "8px 14px", display: "flex", flexDirection: "column", gap: 6 }}>
                  {lint.notes.map((n, i) => (
                    <div key={i} style={{ display: "flex", alignItems: "center", gap: 10, fontSize: 12 }}>
                      <span style={{ width: 5, height: 5, borderRadius: "50%", background: n.tone === "warn" ? "hsl(var(--gold))" : "hsl(var(--success))" }} />
                      <span style={{ color: n.tone === "warn" ? "hsl(var(--gold))" : "hsl(var(--text-2))" }}>{n.msg}</span>
                      <span className="mono" style={{ marginLeft: "auto", fontSize: 10, color: "hsl(var(--text-4))" }}>{n.line ? `line ${n.line}` : ""}</span>
                    </div>
                  ))}
                </div>
              </div>

              {result ? (
                <div className="surface-elev" style={{
                  padding: 14, borderColor: "hsl(var(--success) / 0.3)",
                  background: "linear-gradient(90deg, hsl(var(--success) / 0.08), transparent)",
                }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                    <I3.check width={16} height={16} style={{ color: "hsl(var(--success))" }} />
                    <span style={{ fontSize: 13, fontWeight: 500 }}>Applied successfully</span>
                    <span className="mono tnum" style={{ marginLeft: "auto", fontSize: 11, color: "hsl(var(--text-3))" }}>{result.time}ms</span>
                  </div>
                  <div style={{ marginTop: 10, display: "flex", gap: 18, fontSize: 12 }}>
                    <span><span className="mono tnum" style={{ color: "hsl(var(--cyan-glow))" }}>{result.summary.jobs}</span> <span style={{ color: "hsl(var(--text-3))" }}>job</span></span>
                    <span><span className="mono tnum" style={{ color: "hsl(var(--cyan-glow))" }}>{result.summary.atoms}</span> <span style={{ color: "hsl(var(--text-3))" }}>atoms</span></span>
                    <span><span className="mono tnum" style={{ color: "hsl(var(--cyan-glow))" }}>{result.summary.triggers}</span> <span style={{ color: "hsl(var(--text-3))" }}>trigger</span></span>
                  </div>
                </div>
              ) : null}
            </>
          ) : tab === "diff" ? (
            <DiffView diff={diff} />
          ) : (
            <HistoryView />
          )}
        </div>

        {/* Right rail: schema reference + tips */}
        <div style={{ display: "flex", flexDirection: "column", gap: 12, position: "sticky", top: 0 }}>
          <div className="surface-elev" style={{ padding: 14 }}>
            <div className="eyebrow" style={{ marginBottom: 10 }}>Schema reference</div>
            <RefBlock title="Top-level fields" code={`apiVersion: v1
kind: Job
metadata:
  alias: string     # required
trigger:
  type: cron|http
steps: [...]`} />
            <RefBlock title="DAG edges" code={`# Prerequisites (recommended)
dependsOn: [extract.users]

# Or explicit successors
next: [transform.users]`} />
            <RefBlock title="Callbacks" code={`callbacks:
  - type: notification
    configuration:
      webhook_url: "https://…"
      channel: "#alerts"`} />
          </div>
          <div className="surface-elev" style={{ padding: 14 }}>
            <div className="eyebrow" style={{ marginBottom: 10 }}>Tips</div>
            <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 8, fontSize: 12, color: "hsl(var(--text-2))", lineHeight: 1.5 }}>
              <li><span style={{ color: "hsl(var(--cyan-glow))" }}>·</span> Apply is <strong>idempotent</strong> — re-applying updates existing resources.</li>
              <li><span style={{ color: "hsl(var(--cyan-glow))" }}>·</span> Multiple resources can share one manifest.</li>
              <li><span style={{ color: "hsl(var(--cyan-glow))" }}>·</span> Use <code className="mono" style={{ background: "hsl(var(--obsidian))", padding: "1px 4px", borderRadius: 3, fontSize: 11 }}>caesium job lint</code> in CI.</li>
              <li><span style={{ color: "hsl(var(--cyan-glow))" }}>·</span> Without edges, steps run sequentially.</li>
            </ul>
          </div>
        </div>
      </div>
    </div>
  );
}

function YamlEditor({ value, onChange, lint }) {
  const lines = value.split("\n");
  const errLines = new Set(lint.errors.map(e => e.line));
  return (
    <div style={{ position: "relative", minHeight: 420, background: "hsl(var(--void))" }}>
      <div aria-hidden style={{
        position: "absolute", inset: 0, padding: "12px 14px 12px 56px",
        fontFamily: "var(--font-mono)", fontSize: 12.5, lineHeight: "20px",
        whiteSpace: "pre", pointerEvents: "none", color: "transparent",
      }}>
        {lines.map((ln, i) => (
          <div key={i} style={{ background: errLines.has(i + 1) ? "hsl(var(--danger) / 0.08)" : "transparent" }}>
            {syntaxHl(ln)}
          </div>
        ))}
      </div>
      <div aria-hidden className="mono" style={{
        position: "absolute", left: 0, top: 0, bottom: 0, width: 44,
        padding: "12px 0", textAlign: "right", paddingRight: 8,
        fontSize: 11, color: "hsl(var(--text-4))", lineHeight: "20px",
        background: "hsl(var(--obsidian) / 0.4)", borderRight: "1px solid hsl(var(--graphite))",
        userSelect: "none",
      }}>
        {lines.map((_, i) => (
          <div key={i} style={{ color: errLines.has(i + 1) ? "hsl(var(--danger))" : undefined }}>{i + 1}</div>
        ))}
      </div>
      <textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
        style={{
          width: "100%", minHeight: 420, padding: "12px 14px 12px 56px",
          fontFamily: "var(--font-mono)", fontSize: 12.5, lineHeight: "20px",
          background: "transparent", color: "hsl(var(--text-1))",
          border: "none", outline: "none", resize: "vertical",
          caretColor: "hsl(var(--cyan))",
        }}
        onKeyDown={(e) => {
          if (e.key === "Tab") {
            e.preventDefault();
            const s = e.target.selectionStart, en = e.target.selectionEnd;
            const next = value.substring(0, s) + "  " + value.substring(en);
            onChange(next);
            requestAnimationFrame(() => { e.target.selectionStart = e.target.selectionEnd = s + 2; });
          }
        }}
      />
    </div>
  );
}

function syntaxHl(line) {
  // Render highlighted spans behind the textarea, character-aligned.
  const parts = [];
  const m = line.match(/^(\s*)(#.*)$/);
  if (m) {
    parts.push(<span key="i" style={{ color: "transparent" }}>{m[1]}</span>);
    parts.push(<span key="c" style={{ color: "hsl(var(--text-4))", fontStyle: "italic" }}>{m[2]}</span>);
    return parts;
  }
  const km = line.match(/^(\s*-?\s*)([\w.-]+)(:)(\s*)(.*)$/);
  if (km) {
    parts.push(<span key="lead" style={{ color: "transparent" }}>{km[1]}</span>);
    parts.push(<span key="key" style={{ color: "hsl(var(--cyan-glow))" }}>{km[2]}</span>);
    parts.push(<span key="col" style={{ color: "hsl(var(--text-3))" }}>{km[3]}</span>);
    parts.push(<span key="sp" style={{ color: "transparent" }}>{km[4]}</span>);
    if (km[5]) {
      const v = km[5];
      const isStr = v.startsWith('"') || v.startsWith("'");
      parts.push(<span key="val" style={{ color: isStr ? "hsl(var(--gold))" : v.match(/^\d/) ? "hsl(38 80% 65%)" : "hsl(var(--text-1))" }}>{v}</span>);
    }
    return parts;
  }
  return <span style={{ color: "hsl(var(--text-2))" }}>{line || " "}</span>;
}

function lintYaml(yaml) {
  const errors = [];
  const notes = [];
  const lines = yaml.split("\n");
  let stepCount = 0;
  let hasApi = false, hasKind = false, hasAlias = false, hasTrigger = false;
  lines.forEach((l, i) => {
    if (l.match(/^apiVersion:/)) hasApi = true;
    if (l.match(/^kind:/)) hasKind = true;
    if (l.match(/alias:/)) hasAlias = true;
    if (l.match(/^trigger:/)) hasTrigger = true;
    if (l.match(/^\s*-\s+name:/)) stepCount++;
    if (l.match(/\t/)) errors.push({ line: i + 1, msg: "tabs are not allowed in YAML" });
  });
  if (!hasApi) errors.push({ line: 1, msg: "missing apiVersion" });
  if (!hasKind) errors.push({ line: 1, msg: "missing kind" });
  if (!hasAlias) errors.push({ line: 1, msg: "missing metadata.alias" });
  if (!hasTrigger) notes.push({ msg: "no trigger defined — job will be manual-only", tone: "warn" });
  if (stepCount === 0) errors.push({ line: 1, msg: "at least one step is required" });
  notes.push({ msg: `${stepCount} steps detected`, tone: "ok" });
  if (stepCount > 0) notes.push({ msg: "DAG edges resolved via dependsOn", tone: "ok" });
  return { errors, notes, summary: { steps: stepCount } };
}

function synthDiff() {
  // Mock diff against server state
  return {
    changes: [
      { type: "modify", path: "metadata.labels.tier", from: "standard", to: "critical" },
      { type: "add", path: "steps[3].command", value: '["snowsql", "-f", "/sql/load.sql"]' },
      { type: "add", path: "steps[*].dependsOn", value: "(restructured DAG)" },
    ],
  };
}

function DiffView({ diff }) {
  const colors = { add: "hsl(var(--success))", modify: "hsl(var(--gold))", remove: "hsl(var(--danger))" };
  const labels = { add: "+", modify: "~", remove: "-" };
  return (
    <div className="surface-elev">
      <div style={{ padding: "12px 16px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", justifyContent: "space-between" }}>
        <div>
          <div style={{ fontSize: 13, fontWeight: 500 }}>Diff vs server state</div>
          <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>{diff.changes.length} changes pending apply</div>
        </div>
        <div style={{ display: "flex", gap: 12, fontSize: 11 }}>
          <span style={{ color: "hsl(var(--success))" }}>+ {diff.changes.filter(c => c.type === "add").length} added</span>
          <span style={{ color: "hsl(var(--gold))" }}>~ {diff.changes.filter(c => c.type === "modify").length} modified</span>
          <span style={{ color: "hsl(var(--danger))" }}>- {diff.changes.filter(c => c.type === "remove").length} removed</span>
        </div>
      </div>
      <div style={{ padding: 14, fontFamily: "var(--font-mono)", fontSize: 12, lineHeight: 1.7 }}>
        {diff.changes.map((c, i) => (
          <div key={i} style={{ display: "flex", gap: 12, padding: "6px 0", borderBottom: i === diff.changes.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.3)" }}>
            <span style={{ color: colors[c.type], fontWeight: 700, width: 20 }}>{labels[c.type]}</span>
            <span style={{ color: "hsl(var(--cyan-glow))", flexShrink: 0 }}>{c.path}</span>
            {c.from ? (
              <>
                <span style={{ color: "hsl(var(--danger))", textDecoration: "line-through" }}>{c.from}</span>
                <span style={{ color: "hsl(var(--text-4))" }}>→</span>
                <span style={{ color: "hsl(var(--success))" }}>{c.to}</span>
              </>
            ) : (
              <span style={{ color: "hsl(var(--text-2))" }}>{c.value || c.to}</span>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

function HistoryView() {
  const items = [
    { time: ta3(Date.now() - 1000 * 60 * 8),    actor: "alice@cs.io",   action: "applied", summary: "5 atoms updated, 1 trigger created", ok: true },
    { time: ta3(Date.now() - 1000 * 60 * 60 * 4), actor: "ci-bot",      action: "applied", summary: "git sync from main@a4f7b2", ok: true },
    { time: ta3(Date.now() - 1000 * 60 * 60 * 28), actor: "bob@cs.io",   action: "rejected", summary: "schema validation failed", ok: false },
    { time: ta3(Date.now() - 1000 * 60 * 60 * 72), actor: "ci-bot",      action: "applied", summary: "git sync from main@9c1de4", ok: true },
  ];
  return (
    <div className="surface-elev">
      <div style={{ padding: "12px 16px", borderBottom: "1px solid hsl(var(--graphite))" }}>
        <div style={{ fontSize: 13, fontWeight: 500 }}>Apply history</div>
        <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>Recent manifest applies for this job</div>
      </div>
      {items.map((it, i) => (
        <div key={i} style={{ display: "grid", gridTemplateColumns: "120px 180px 1fr 90px", padding: "12px 16px", alignItems: "center", borderBottom: i === items.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.4)", fontSize: 12 }}>
          <span className="mono" style={{ color: "hsl(var(--text-3))" }}>{it.time}</span>
          <span className="mono" style={{ color: "hsl(var(--text-2))" }}>{it.actor}</span>
          <span style={{ color: "hsl(var(--text-2))" }}>{it.summary}</span>
          <span style={{ display: "inline-flex", padding: "2px 8px", borderRadius: 4, fontSize: 10, fontWeight: 600, letterSpacing: "0.1em", textTransform: "uppercase", background: it.ok ? "hsl(var(--success) / 0.15)" : "hsl(var(--danger) / 0.15)", color: it.ok ? "hsl(var(--success))" : "hsl(var(--danger))", width: "fit-content" }}>{it.action}</span>
        </div>
      ))}
    </div>
  );
}

function RefBlock({ title, code }) {
  return (
    <div style={{ marginBottom: 12 }}>
      <div style={{ fontSize: 11, fontWeight: 500, color: "hsl(var(--text-2))", marginBottom: 5 }}>{title}</div>
      <pre className="mono" style={{
        margin: 0, padding: 9, borderRadius: 5,
        background: "hsl(var(--void))", border: "1px solid hsl(var(--graphite) / 0.6)",
        fontSize: 10.5, lineHeight: 1.6, color: "hsl(var(--text-2))",
        whiteSpace: "pre-wrap", overflow: "hidden",
      }}>{code}</pre>
    </div>
  );
}

window.PAGES3 = { SystemPage, JobDefsPage };
