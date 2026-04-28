/* === Caesium Refresh — Pages: Stats, Triggers, Run Detail, System === */

const { useState: usS, useEffect: usE, useMemo: usM } = React;
const { Btn: B2, StatusBadge: SB2, I: I2, AtomLogo: AL2, EmptyState: ES2, timeAgo: ta2, fmtDuration: fd2, Sparkline: SP2 } = window.UI;

/* ================================================================== */
/* STATS PAGE                                                         */
/* ================================================================== */
function StatsPage() {
  const s = window.MOCK.STATS;
  return (
    <div className="fade-up" style={{ padding: "20px 28px", display: "flex", flexDirection: "column", gap: 18, minHeight: "100%", overflow: "auto" }}>
      <div>
        <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>Telemetry</div>
        <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0", letterSpacing: "-0.01em" }}>Statistics</h1>
        <p style={{ margin: "4px 0 0", color: "hsl(var(--text-3))", fontSize: 13 }}>Fleet-level health and historical trends</p>
      </div>

      {/* KPI cards */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 14 }}>
        <Kpi label="Total jobs" value={s.totals.jobs} accent="cyan" />
        <Kpi label="Runs / 24h" value={s.totals.recent_runs_24h.toLocaleString()} delta="+8.4%" />
        <Kpi label="Success rate" value={`${(s.totals.success_rate * 100).toFixed(1)}%`} accent="success" delta="+0.3pp" />
        <Kpi label="Avg duration" value={`${s.totals.avg_duration_s.toFixed(1)}s`} accent="gold" delta="-12.2s" />
      </div>

      {/* Charts */}
      <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: 14 }}>
        <Card title="Run volume / success rate (30d)" subtitle="Daily aggregate across all jobs">
          <DualChart data={s.trend} />
        </Card>
        <Card title="Failure distribution">
          <FailDist data={s.top_failing} />
        </Card>
      </div>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 14 }}>
        <Card title="Top failing jobs">
          <Lst items={s.top_failing.map((j) => ({ k: j.alias, v: j.count, suf: "fails", tone: "danger" }))} />
        </Card>
        <Card title="Slowest jobs (avg duration)">
          <Lst items={s.slowest.map((j) => ({ k: j.alias, v: fd2(j.avg), tone: "gold" }))} />
        </Card>
      </div>
    </div>
  );
}
function Kpi({ label, value, delta, accent }) {
  const colors = { cyan: "hsl(var(--cyan-glow))", success: "hsl(var(--success))", gold: "hsl(var(--gold))" };
  return (
    <div className="surface-elev" style={{ padding: 16, position: "relative", overflow: "hidden" }}>
      <div style={{ position: "absolute", top: -20, right: -20, width: 80, height: 80, opacity: 0.04 }}>
        <AL2 size={80} animated={false} />
      </div>
      <div className="eyebrow">{label}</div>
      <div className="tnum" style={{ marginTop: 6, fontSize: 28, fontWeight: 500, color: accent ? colors[accent] : "hsl(var(--text-1))", letterSpacing: "-0.02em" }}>{value}</div>
      {delta ? <div className="mono tnum" style={{ marginTop: 4, fontSize: 11, color: delta.startsWith("-") ? "hsl(var(--success))" : "hsl(var(--success))" }}>{delta}</div> : null}
    </div>
  );
}
function Card({ title, subtitle, children }) {
  return (
    <div className="surface-elev" style={{ padding: 16 }}>
      <div style={{ marginBottom: 12 }}>
        <div style={{ fontSize: 13, fontWeight: 500 }}>{title}</div>
        {subtitle ? <div style={{ fontSize: 11, color: "hsl(var(--text-3))", marginTop: 2 }}>{subtitle}</div> : null}
      </div>
      {children}
    </div>
  );
}
function DualChart({ data }) {
  const w = 600, h = 200, pad = 24;
  const maxR = Math.max(...data.map((d) => d.runs));
  const xs = (i) => pad + (i / (data.length - 1)) * (w - pad * 2);
  const yR = (v) => h - pad - (v / maxR) * (h - pad * 2);
  const yS = (v) => h - pad - ((v - 0.85) / 0.15) * (h - pad * 2);
  const path = data.map((d, i) => `${i === 0 ? "M" : "L"} ${xs(i)} ${yS(d.success)}`).join(" ");
  const barW = (w - pad * 2) / data.length - 2;
  return (
    <svg viewBox={`0 0 ${w} ${h}`} style={{ width: "100%", height: 200 }}>
      <defs>
        <linearGradient id="run-bar" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="hsl(var(--cyan))" stopOpacity="0.5" />
          <stop offset="100%" stopColor="hsl(var(--cyan))" stopOpacity="0.05" />
        </linearGradient>
      </defs>
      {/* gridlines */}
      {[0, 0.25, 0.5, 0.75, 1].map((p, i) => (
        <line key={i} x1={pad} x2={w - pad} y1={pad + p * (h - pad * 2)} y2={pad + p * (h - pad * 2)} stroke="hsl(var(--graphite) / 0.4)" strokeDasharray="2 4" />
      ))}
      {/* bars (run volume) */}
      {data.map((d, i) => {
        const yy = yR(d.runs);
        return <rect key={i} x={xs(i) - barW / 2} y={yy} width={barW} height={h - pad - yy} rx={1} fill="url(#run-bar)" />;
      })}
      {/* line (success) */}
      <path d={path} fill="none" stroke="hsl(var(--gold))" strokeWidth={2} />
      {data.map((d, i) => <circle key={i} cx={xs(i)} cy={yS(d.success)} r={2.5} fill="hsl(var(--gold))" />)}
      {/* legend */}
      <g transform={`translate(${pad}, 8)`}>
        <rect x={0} y={0} width={10} height={4} fill="hsl(var(--cyan))" />
        <text x={16} y={6} fontSize={10} fill="hsl(var(--text-3))">runs/day</text>
        <line x1={86} x2={96} y1={2} y2={2} stroke="hsl(var(--gold))" strokeWidth={2} />
        <text x={102} y={6} fontSize={10} fill="hsl(var(--text-3))">success rate</text>
      </g>
    </svg>
  );
}
function FailDist({ data }) {
  const total = data.reduce((a, d) => a + d.count, 0);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      {data.map((d, i) => {
        const pct = (d.count / total) * 100;
        return (
          <div key={i}>
            <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, marginBottom: 4 }}>
              <span style={{ color: "hsl(var(--text-1))" }}>{d.alias}</span>
              <span className="mono tnum" style={{ color: "hsl(var(--text-3))" }}>{d.count} · {pct.toFixed(0)}%</span>
            </div>
            <div style={{ height: 6, background: "hsl(var(--graphite))", borderRadius: 3, overflow: "hidden" }}>
              <div style={{ width: `${pct}%`, height: "100%", background: "linear-gradient(90deg, hsl(var(--danger)), hsl(var(--gold)))", borderRadius: 3 }} />
            </div>
          </div>
        );
      })}
    </div>
  );
}
function Lst({ items }) {
  const colors = { danger: "hsl(var(--danger))", gold: "hsl(var(--gold))" };
  return (
    <div style={{ display: "flex", flexDirection: "column" }}>
      {items.map((x, i) => (
        <div key={i} style={{ display: "flex", justifyContent: "space-between", alignItems: "center", padding: "10px 0", borderBottom: i === items.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.5)" }}>
          <span style={{ fontSize: 13 }}>{x.k}</span>
          <span className="mono tnum" style={{ fontSize: 12, color: colors[x.tone] || "hsl(var(--text-1))" }}>{x.v} <span style={{ color: "hsl(var(--text-3))" }}>{x.suf || ""}</span></span>
        </div>
      ))}
    </div>
  );
}

/* ================================================================== */
/* TRIGGERS PAGE                                                      */
/* ================================================================== */
function TriggersPage() {
  return (
    <div className="fade-up" style={{ padding: "20px 28px", minHeight: "100%", overflow: "auto" }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-end", marginBottom: 20 }}>
        <div>
          <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>Schedules & Webhooks</div>
          <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0" }}>Triggers</h1>
          <p style={{ margin: "4px 0 0", color: "hsl(var(--text-3))", fontSize: 13 }}>Cron schedules and webhook receivers</p>
        </div>
        <B2 variant="primary" icon={I2.zap}>New trigger</B2>
      </div>
      <div className="surface-elev" style={{ overflow: "hidden" }}>
        <div style={{ display: "grid", gridTemplateColumns: "100px 1.2fr 1fr 1.4fr 1fr 80px", padding: "10px 18px", background: "hsl(var(--obsidian) / 0.5)", borderBottom: "1px solid hsl(var(--graphite))", fontSize: 10, fontWeight: 500, letterSpacing: "0.18em", textTransform: "uppercase", color: "hsl(var(--text-3))" }}>
          <span>Type</span><span>Alias</span><span>Schedule</span><span>Job</span><span>Next fire</span><span>State</span>
        </div>
        {window.MOCK.TRIGGERS.map((t, i) => (
          <div key={t.id} style={{ display: "grid", gridTemplateColumns: "100px 1.2fr 1fr 1.4fr 1fr 80px", padding: "14px 18px", borderBottom: i === window.MOCK.TRIGGERS.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.5)", alignItems: "center" }}>
            <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: 11, padding: "3px 8px", borderRadius: 4, background: t.type === "cron" ? "hsl(var(--cyan) / 0.1)" : "hsl(var(--gold) / 0.1)", color: t.type === "cron" ? "hsl(var(--cyan-glow))" : "hsl(var(--gold))", border: `1px solid ${t.type === "cron" ? "hsl(var(--cyan) / 0.3)" : "hsl(var(--gold) / 0.3)"}`, width: "fit-content", textTransform: "uppercase", letterSpacing: "0.1em", fontWeight: 600 }}>
              {t.type === "cron" ? <I2.cron width={11} height={11} /> : <I2.webhook width={11} height={11} />}
              {t.type}
            </span>
            <span style={{ fontSize: 13 }}>{t.alias}</span>
            <span className="mono tnum" style={{ fontSize: 12, color: "hsl(var(--text-2))" }}>{t.config}</span>
            <span style={{ fontSize: 13, color: "hsl(var(--cyan-glow))" }}>{t.target}</span>
            <span className="mono tnum" style={{ fontSize: 12, color: "hsl(var(--text-2))" }}>{t.type === "cron" ? `in ${(Math.random() * 60).toFixed(0)}m` : "—"}</span>
            <span>{t.paused ? <SB2 status="paused" size="sm" /> : <SB2 status="succeeded" size="sm" />}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

/* ================================================================== */
/* RUN DETAIL — Log viewer                                            */
/* ================================================================== */
function RunDetailPage({ jobId, runId, onBack }) {
  const job = window.MOCK.JOBS.find((j) => j.id === jobId) || window.MOCK.JOBS[0];
  const run = job.last_runs.find((r) => r.id === runId) || job.last_runs[0];
  const [filter, setFilter] = usS("");
  const [level, setLevel] = usS("ALL");
  const [follow, setFollow] = usS(true);
  const lines = window.MOCK.LOG_LINES.filter((l) =>
    (level === "ALL" || l.level === level) && (!filter || l.msg.toLowerCase().includes(filter.toLowerCase()))
  );
  const ref = React.useRef(null);
  usE(() => { if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight; }, [lines, follow]);

  return (
    <div className="fade-up" style={{ display: "flex", flexDirection: "column", height: "100%", overflow: "hidden" }}>
      <div style={{ padding: "18px 28px 14px", borderBottom: "1px solid hsl(var(--graphite))" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6, fontSize: 12 }}>
          <button onClick={onBack} style={{ background: "transparent", border: "none", color: "hsl(var(--text-3))", cursor: "pointer", display: "flex", alignItems: "center", gap: 4 }}>
            <I2.back width={14} height={14} /> {job.alias}
          </button>
          <span style={{ color: "hsl(var(--text-4))" }}>/</span>
          <span className="mono" style={{ color: "hsl(var(--text-2))" }}>{run.id}</span>
        </div>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", flexWrap: "wrap", gap: 12 }}>
          <div>
            <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
              <h1 style={{ fontSize: 22, fontWeight: 500, margin: 0 }}>Run <span className="mono">{run.id}</span></h1>
              <SB2 status={run.status} />
            </div>
            <div style={{ marginTop: 8, display: "flex", gap: 18, fontSize: 11, color: "hsl(var(--text-3))" }}>
              <Stat2 label="Started" value={ta2(run.started_at)} />
              <Stat2 label="Duration" value={fd2(run.duration)} mono />
              <Stat2 label="Tasks" value={`${run.total_tasks - run.cache_hits} executed · ${run.cache_hits} cached`} />
              <Stat2 label="Engine" value="docker" />
            </div>
          </div>
          <div style={{ display: "flex", gap: 8 }}>
            <B2 variant="outline" size="md" icon={I2.history}>All runs</B2>
            <B2 variant="outline" size="md">Re-run</B2>
            <B2 variant="danger" size="md">Cancel</B2>
          </div>
        </div>
      </div>

      {/* Timeline */}
      <div style={{ padding: "14px 28px", borderBottom: "1px solid hsl(var(--graphite))", background: "hsl(var(--obsidian) / 0.4)" }}>
        <div className="eyebrow" style={{ marginBottom: 10 }}>Run timeline</div>
        <Timeline />
      </div>

      {/* Log toolbar */}
      <div style={{ padding: "10px 28px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", alignItems: "center", gap: 10 }}>
        <div style={{ position: "relative", flex: 1, maxWidth: 360 }}>
          <I2.search width={12} height={12} style={{ position: "absolute", left: 10, top: 9, color: "hsl(var(--text-3))" }} />
          <input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter logs…"
            style={{ width: "100%", height: 30, padding: "0 10px 0 28px", borderRadius: 6, background: "hsl(var(--obsidian))", border: "1px solid hsl(var(--graphite))", color: "hsl(var(--text-1))", fontSize: 12, outline: "none" }} />
        </div>
        <div style={{ display: "flex", gap: 2, padding: 2, background: "hsl(var(--obsidian))", border: "1px solid hsl(var(--graphite))", borderRadius: 6 }}>
          {["ALL", "INFO", "WARN", "DEBUG"].map((l) => (
            <button key={l} onClick={() => setLevel(l)} className="mono"
              style={{ padding: "4px 10px", borderRadius: 4, background: level === l ? "hsl(var(--cyan) / 0.15)" : "transparent", color: level === l ? "hsl(var(--cyan-glow))" : "hsl(var(--text-3))", border: "none", fontSize: 11, cursor: "pointer", fontWeight: 600 }}>{l}</button>
          ))}
        </div>
        <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, color: "hsl(var(--text-2))", cursor: "pointer" }}>
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} style={{ accentColor: "hsl(var(--cyan))" }} />
          Follow tail
        </label>
        <span style={{ flex: 1 }} />
        <span className="mono tnum" style={{ fontSize: 11, color: "hsl(var(--text-3))" }}>{lines.length} lines</span>
      </div>

      {/* Log body */}
      <div ref={ref} style={{ flex: 1, overflow: "auto", padding: "12px 28px", fontFamily: "var(--font-mono)", fontSize: 12, lineHeight: 1.75, background: "hsl(var(--void))" }}>
        {lines.map((l, i) => {
          const lvlColor = l.level === "WARN" ? "hsl(var(--gold))" : l.level === "DEBUG" ? "hsl(var(--text-3))" : l.level === "ERROR" ? "hsl(var(--danger))" : "hsl(var(--cyan-glow))";
          return (
            <div key={i} style={{ display: "grid", gridTemplateColumns: "60px 70px 60px 1fr", gap: 12, color: "hsl(var(--text-2))" }}>
              <span style={{ color: "hsl(var(--text-4))" }}>{(i + 1).toString().padStart(4, "0")}</span>
              <span className="tnum" style={{ color: "hsl(var(--text-3))" }}>{ta2(l.t)}</span>
              <span style={{ color: lvlColor, fontWeight: 600 }}>{l.level}</span>
              <span>{l.msg}</span>
            </div>
          );
        })}
        {run.status === "running" ? (
          <div style={{ display: "flex", alignItems: "center", gap: 10, marginTop: 8, color: "hsl(var(--cyan))" }}>
            <span className="dot running" style={{ width: 7, height: 7 }} />
            waiting for next line…
          </div>
        ) : null}
      </div>
    </div>
  );
}
function Stat2({ label, value, mono }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 1 }}>
      <span className="eyebrow" style={{ fontSize: 9 }}>{label}</span>
      <span className={mono ? "mono tnum" : ""} style={{ fontSize: 12, color: "hsl(var(--text-1))", fontWeight: 500 }}>{value}</span>
    </div>
  );
}
function Timeline() {
  const tasks = window.MOCK.DAG.nodes;
  const total = 180; // seconds
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {tasks.slice(0, 6).map((t) => {
        const start = (Math.random() * 0.4) * 100;
        const end = Math.min(100, start + (t.duration ? (t.duration / total) * 100 : 18));
        const color = t.status === "succeeded" ? "hsl(var(--success))" : t.status === "running" ? "hsl(var(--cyan))" : t.status === "failed" ? "hsl(var(--danger))" : "hsl(var(--text-4))";
        return (
          <div key={t.id} style={{ display: "grid", gridTemplateColumns: "180px 1fr 60px", gap: 10, alignItems: "center", fontSize: 11 }}>
            <span style={{ color: "hsl(var(--text-2))", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{t.label}</span>
            <div style={{ position: "relative", height: 12, background: "hsl(var(--graphite) / 0.3)", borderRadius: 3 }}>
              <div style={{ position: "absolute", left: `${start}%`, width: `${end - start}%`, top: 0, bottom: 0, background: color, borderRadius: 3, opacity: 0.85 }}>
                {t.status === "running" ? <div style={{ position: "absolute", inset: 0, background: "linear-gradient(90deg, transparent, hsl(0 0% 100% / 0.3), transparent)", animation: "scan 2s linear infinite" }} /> : null}
              </div>
            </div>
            <span className="mono tnum" style={{ color: "hsl(var(--text-3))", textAlign: "right" }}>{t.duration ? `${t.duration}s` : "…"}</span>
          </div>
        );
      })}
    </div>
  );
}

/* ================================================================== */
/* SYSTEM / ATOMS / JOBDEFS — placeholders                            */
/* ================================================================== */
function PlaceholderPage({ title, eyebrow, hint }) {
  return (
    <div className="fade-up" style={{ padding: "20px 28px", minHeight: "100%", display: "flex", flexDirection: "column" }}>
      <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>{eyebrow}</div>
      <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0" }}>{title}</h1>
      <div style={{ flex: 1, display: "flex", alignItems: "center", justifyContent: "center" }}>
        <ES2 title={`${title} — work in progress`} subtitle={hint} action={<B2 variant="outline">Browse docs</B2>} />
      </div>
    </div>
  );
}

window.PAGES2 = { StatsPage, TriggersPage, RunDetailPage, PlaceholderPage };
