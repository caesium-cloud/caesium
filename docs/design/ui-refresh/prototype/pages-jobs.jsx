/* === Caesium Refresh — Pages: Jobs, JobDetail, Stats, Triggers, Run Detail, System === */

const { useState: uS, useEffect: uE, useMemo: uM, useRef: uR } = React;
const { Btn, StatusBadge, Sparkline, I, AtomLogo, EmptyState, timeAgo, fmtDuration } = window.UI;

/* ================================================================== */
/* JOBS PAGE                                                          */
/* ================================================================== */
function JobsPage({ onOpen, density, badgeStyle, accent, onTrigger, onPause }) {
  const [filter, setFilter] = uS("all");
  const [query, setQuery] = uS("");
  const jobs = window.MOCK.JOBS.filter((j) => {
    if (filter === "running" && !j.last_runs.some(r => r.status === "running")) return false;
    if (filter === "failing" && !j.last_runs.some(r => r.status === "failed")) return false;
    if (filter === "paused" && !j.paused) return false;
    if (query && !j.alias.toLowerCase().includes(query.toLowerCase())) return false;
    return true;
  });

  const rowH = density === "compact" ? 50 : density === "cozy" ? 60 : 70;

  return (
    <div className="fade-up" style={{ padding: "20px 28px", display: "flex", flexDirection: "column", gap: 18, minHeight: "100%", overflow: "auto" }}>
      {/* Page title */}
      <div style={{ display: "flex", alignItems: "flex-end", justifyContent: "space-between", gap: 16 }}>
        <div>
          <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)" }}>Pipelines</div>
          <h1 style={{ fontSize: 28, fontWeight: 500, margin: "4px 0 0", letterSpacing: "-0.01em" }}>Jobs</h1>
          <p style={{ margin: "4px 0 0", color: "hsl(var(--text-3))", fontSize: 13 }}>
            {jobs.length} of {window.MOCK.JOBS.length} jobs · {jobs.reduce((a, j) => a + (j.last_runs[0]?.status === "running" ? 1 : 0), 0)} running now
          </p>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <Btn variant="outline" size="md" icon={I.calendar}>Backfill</Btn>
          <Btn variant="primary" size="md" icon={I.play}>New Job</Btn>
        </div>
      </div>

      {/* Filter bar */}
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <div style={{ display: "flex", gap: 2, padding: 3, background: "hsl(var(--obsidian))", border: "1px solid hsl(var(--graphite))", borderRadius: 8 }}>
          {[
            { k: "all", label: "All", count: window.MOCK.JOBS.length },
            { k: "running", label: "Running", count: window.MOCK.JOBS.filter(j => j.last_runs[0]?.status === "running").length },
            { k: "failing", label: "Recent failures", count: window.MOCK.JOBS.filter(j => j.last_runs.slice(0, 3).some(r => r.status === "failed")).length },
            { k: "paused", label: "Paused", count: window.MOCK.JOBS.filter(j => j.paused).length },
          ].map((f) => {
            const on = filter === f.k;
            return (
              <button key={f.k} onClick={() => setFilter(f.k)} style={{
                padding: "6px 12px", borderRadius: 6,
                background: on ? "hsl(var(--cyan) / 0.15)" : "transparent",
                color: on ? "hsl(var(--cyan-glow))" : "hsl(var(--text-2))",
                border: "1px solid", borderColor: on ? "hsl(var(--cyan) / 0.3)" : "transparent",
                fontSize: 12, fontWeight: on ? 500 : 400, cursor: "pointer",
                display: "flex", alignItems: "center", gap: 6,
              }}>
                {f.label}
                <span className="mono tnum" style={{ fontSize: 10, color: on ? "hsl(var(--cyan))" : "hsl(var(--text-3))" }}>{f.count}</span>
              </button>
            );
          })}
        </div>
        <div style={{ flex: 1, position: "relative", maxWidth: 320 }}>
          <I.search width={13} height={13} style={{ position: "absolute", left: 10, top: 9, color: "hsl(var(--text-3))" }} />
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Filter by alias…"
            style={{ width: "100%", height: 32, padding: "0 12px 0 30px", borderRadius: 6,
                     background: "hsl(var(--obsidian))", border: "1px solid hsl(var(--graphite))",
                     color: "hsl(var(--text-1))", fontSize: 12, outline: "none" }} />
        </div>
      </div>

      {/* Table */}
      <div className="surface-elev" style={{ overflow: "hidden", flexShrink: 0 }}>
        <div style={{
          display: "grid",
          gridTemplateColumns: "minmax(280px, 1.6fr) 130px 110px 130px 120px 120px",
          padding: "10px 18px",
          background: "hsl(var(--obsidian) / 0.5)",
          borderBottom: "1px solid hsl(var(--graphite))",
          fontSize: 10, fontWeight: 500, letterSpacing: "0.18em", textTransform: "uppercase",
          color: "hsl(var(--text-3))",
        }}>
          <span>Alias / Description</span>
          <span>State</span>
          <span>7d Activity</span>
          <span>Last run</span>
          <span>Duration</span>
          <span style={{ textAlign: "right" }}>Actions</span>
        </div>

        {jobs.length === 0 ? <EmptyState title="No jobs match" subtitle="Try clearing filters or check the running tab." /> : null}

        {jobs.map((job, idx) => {
          const latest = job.last_runs[0];
          const isRunning = latest?.status === "running";
          const isQueued = latest?.status === "queued";
          const failureRecent = job.last_runs.slice(0, 5).some(r => r.status === "failed");

          return (
            <div key={job.id}
              onClick={() => onOpen(job.id)}
              style={{
                display: "grid",
                gridTemplateColumns: "minmax(280px, 1.6fr) 130px 110px 130px 120px 120px",
                padding: `${density === "compact" ? "10px" : "14px"} 18px`,
                borderBottom: idx === jobs.length - 1 ? "none" : "1px solid hsl(var(--graphite) / 0.5)",
                cursor: "pointer", alignItems: "center",
                background: isRunning ? "linear-gradient(90deg, hsl(var(--cyan) / 0.04), transparent)" : job.paused ? "hsl(var(--gold) / 0.025)" : "transparent",
                position: "relative",
                transition: "background 140ms",
              }}
              onMouseEnter={(e) => e.currentTarget.style.background = "hsl(var(--obsidian) / 0.6)"}
              onMouseLeave={(e) => e.currentTarget.style.background = isRunning ? "linear-gradient(90deg, hsl(var(--cyan) / 0.04), transparent)" : job.paused ? "hsl(var(--gold) / 0.025)" : "transparent"}
            >
              {/* Running scan line */}
              {isRunning ? (
                <span style={{ position: "absolute", left: 0, top: 0, bottom: 0, width: 2, background: "hsl(var(--cyan))", boxShadow: "0 0 12px hsl(var(--cyan) / 0.7)" }} />
              ) : null}

              {/* Alias */}
              <div style={{ minWidth: 0 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <span style={{ fontSize: 14, fontWeight: 500, color: "hsl(var(--text-1))" }}>{job.alias}</span>
                  {job.paused ? <StatusBadge status="paused" size="sm" /> : null}
                  {failureRecent && !job.paused ? (
                    <span title="Recent failures" style={{ fontSize: 9, padding: "2px 6px", borderRadius: 99, background: "hsl(var(--danger) / 0.12)", color: "hsl(var(--danger))", border: "1px solid hsl(var(--danger) / 0.3)", letterSpacing: "0.08em", fontWeight: 600, textTransform: "uppercase" }}>flaky</span>
                  ) : null}
                </div>
                <div style={{ fontSize: 12, color: "hsl(var(--text-3))", marginTop: 2, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  {job.description}
                </div>
                {density !== "compact" ? (
                  <div className="mono" style={{ fontSize: 10, color: "hsl(var(--text-4))", marginTop: 3, display: "flex", gap: 10 }}>
                    <span>{job.id.slice(0, 16)}…</span>
                    <span>·</span>
                    <span>{job.schedule_human}</span>
                  </div>
                ) : null}
              </div>

              {/* State */}
              <div>
                <StatusBadge status={latest?.status || "queued"} />
                {isRunning && latest ? (
                  <div className="mono tnum" style={{ marginTop: 4, fontSize: 10, color: "hsl(var(--cyan-glow))" }}>
                    started {timeAgo(latest.started_at)}
                  </div>
                ) : null}
              </div>

              {/* Sparkline */}
              <div>
                <Sparkline runs={job.last_runs} />
                <div className="mono tnum" style={{ marginTop: 3, fontSize: 10, color: "hsl(var(--text-3))" }}>
                  {job.last_runs.filter(r => r.status === "succeeded").length}/{job.last_runs.length} ok
                </div>
              </div>

              {/* Last run */}
              <div className="mono tnum" style={{ fontSize: 12, color: "hsl(var(--text-2))" }}>
                {latest ? timeAgo(latest.started_at) : "—"}
                <div style={{ fontSize: 10, color: "hsl(var(--text-4))", marginTop: 2 }}>
                  {latest ? `${latest.id.slice(0, 11)}` : "no runs"}
                </div>
              </div>

              {/* Duration */}
              <div className="mono tnum" style={{ fontSize: 13, color: "hsl(var(--text-1))" }}>
                {fmtDuration(latest?.duration)}
                {latest?.cache_hits > 0 ? (
                  <div style={{ fontSize: 10, color: "hsl(178 60% 60%)", marginTop: 2, display: "flex", alignItems: "center", gap: 4 }}>
                    <I.cache width={9} height={9} />
                    {latest.cache_hits} cached
                  </div>
                ) : null}
              </div>

              {/* Actions */}
              <div style={{ display: "flex", justifyContent: "flex-end", gap: 4 }} onClick={(e) => e.stopPropagation()}>
                <Btn variant="ghost" size="icon" icon={I.play} disabled={job.paused} onClick={() => onTrigger(job)} title={job.paused ? "Unpause first" : "Trigger run"} />
                <Btn variant="ghost" size="icon" icon={I.pause} onClick={() => onPause(job)} title={job.paused ? "Unpause" : "Pause"} />
                <Btn variant="ghost" size="icon" icon={I.chevron} title="Open" onClick={() => onOpen(job.id)} />
              </div>
            </div>
          );
        })}
      </div>

      {/* Activity feed */}
      <div className="surface-elev" style={{ padding: 18 }}>
        <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 12 }}>
          <div className="eyebrow">Live activity</div>
          <div style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, color: "hsl(var(--text-3))" }}>
            <span className="dot running" style={{ width: 6, height: 6 }} />
            SSE connected
          </div>
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          {window.MOCK.ACTIVITY.slice(0, 6).map((a, i) => (
            <div key={i} style={{ display: "flex", alignItems: "center", gap: 12, fontSize: 12 }}>
              <span className="mono tnum" style={{ width: 70, color: "hsl(var(--text-4))" }}>{timeAgo(a.t)}</span>
              <StatusBadge status={a.kind === "started" ? "running" : a.kind === "success" ? "succeeded" : a.kind === "failed" ? "failed" : a.kind === "cached" ? "cached" : "queued"} size="sm" />
              <span style={{ color: "hsl(var(--text-2))", flex: 1 }}>{a.msg}</span>
              <span className="mono" style={{ fontSize: 10, color: "hsl(var(--text-4))" }}>{a.job}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

/* ================================================================== */
/* JOB DETAIL — DAG view                                              */
/* ================================================================== */
function JobDetailPage({ jobId, onBack, onOpenRun, dagStyle }) {
  const job = window.MOCK.JOBS.find((j) => j.id === jobId) || window.MOCK.JOBS[0];
  const dag = window.MOCK.DAG;
  const [selected, setSel] = uS(null);
  const [tab, setTab] = uS("dag");

  const featuredRun = job.last_runs[0];

  // Layout DAG by lane (column) and y-row
  const lanes = uM(() => {
    const cols = {};
    dag.nodes.forEach((n) => {
      cols[n.lane] = cols[n.lane] || [];
      cols[n.lane].push(n);
    });
    return cols;
  }, []);

  const colW = 220, nodeW = 188, nodeH = 76, gapY = 18, padX = 40, padY = 40;
  const nodePos = uM(() => {
    const pos = {};
    Object.entries(lanes).forEach(([lane, list]) => {
      list.forEach((n, i) => {
        pos[n.id] = {
          x: padX + Number(lane) * colW,
          y: padY + i * (nodeH + gapY),
        };
      });
    });
    return pos;
  }, [lanes]);
  const totalW = padX * 2 + Object.keys(lanes).length * colW;
  const totalH = padY * 2 + Math.max(...Object.values(lanes).map(l => l.length)) * (nodeH + gapY);

  return (
    <div className="fade-up" style={{ display: "flex", flexDirection: "column", height: "100%", overflow: "hidden" }}>
      {/* Header */}
      <div style={{ padding: "18px 28px 14px", borderBottom: "1px solid hsl(var(--graphite))" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
          <button onClick={onBack} style={{ background: "transparent", border: "none", color: "hsl(var(--text-3))", cursor: "pointer", display: "flex", alignItems: "center", gap: 4, fontSize: 12 }}>
            <I.back width={14} height={14} /> Jobs
          </button>
          <span style={{ color: "hsl(var(--text-4))" }}>/</span>
          <span className="mono" style={{ color: "hsl(var(--text-3))", fontSize: 12 }}>{job.id.slice(0, 22)}…</span>
        </div>

        <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", gap: 18, flexWrap: "wrap" }}>
          <div>
            <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
              <h1 style={{ fontSize: 26, fontWeight: 500, margin: 0, letterSpacing: "-0.01em" }}>{job.alias}</h1>
              <StatusBadge status={featuredRun?.status || "queued"} />
              {job.paused ? <StatusBadge status="paused" /> : null}
            </div>
            <div style={{ marginTop: 6, color: "hsl(var(--text-2))", fontSize: 13 }}>{job.description}</div>
            <div style={{ marginTop: 8, display: "flex", gap: 18, fontSize: 11, color: "hsl(var(--text-3))" }}>
              <Stat label="Schedule" value={job.schedule_human} mono />
              <Stat label="Tasks" value={dag.nodes.length} />
              <Stat label="Last run" value={featuredRun ? timeAgo(featuredRun.started_at) : "—"} />
              <Stat label="Cache hits" value={`${featuredRun?.cache_hits || 0}/${featuredRun?.total_tasks || 0}`} accent />
            </div>
          </div>

          <div style={{ display: "flex", gap: 8 }}>
            <Btn variant="outline" size="md" icon={I.history}>Runs</Btn>
            <Btn variant="outline" size="md" icon={I.list}>Tasks</Btn>
            <Btn variant="outline" size="md" icon={I.cog}>Config</Btn>
            <Btn variant="outline" size="md" icon={I.yaml}>YAML</Btn>
            <div style={{ width: 1, background: "hsl(var(--graphite))", margin: "0 4px" }} />
            <Btn variant="primary" size="md" icon={I.play} disabled={job.paused}>Trigger</Btn>
            <Btn variant="outline" size="md" icon={I.pause}>{job.paused ? "Unpause" : "Pause"}</Btn>
          </div>
        </div>
      </div>

      {/* Run overlay strip */}
      <div style={{ display: "flex", alignItems: "center", gap: 16, padding: "10px 28px", background: "hsl(var(--obsidian) / 0.4)", borderBottom: "1px solid hsl(var(--graphite))", fontSize: 12 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span className="dot running" style={{ width: 7, height: 7 }} />
          <span style={{ color: "hsl(var(--text-2))" }}>Live overlay from</span>
          <button onClick={() => onOpenRun(job.id, featuredRun.id)}
            className="mono" style={{ background: "transparent", border: "none", color: "hsl(var(--cyan-glow))", cursor: "pointer", textDecoration: "underline", textDecorationColor: "hsl(var(--cyan) / 0.4)", fontSize: 12 }}>
            {featuredRun.id}
          </button>
        </div>
        <span style={{ color: "hsl(var(--text-4))" }}>·</span>
        <span style={{ color: "hsl(var(--text-3))" }}>started <span className="mono tnum" style={{ color: "hsl(var(--text-1))" }}>{timeAgo(featuredRun.started_at)}</span></span>
        <span style={{ color: "hsl(var(--text-4))" }}>·</span>
        <span style={{ color: "hsl(var(--text-3))" }}>elapsed <span className="mono tnum" style={{ color: "hsl(var(--text-1))" }}>2m 14s</span></span>
        <span style={{ flex: 1 }} />
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <Mini count={dag.nodes.filter(n => n.status === "succeeded").length} total={dag.nodes.length} label="done" tone="success" />
          <Mini count={dag.nodes.filter(n => n.status === "running").length} total={dag.nodes.length} label="active" tone="cyan" />
          <Mini count={dag.nodes.filter(n => n.cached).length} total={dag.nodes.length} label="cached" tone="teal" />
          <Mini count={dag.nodes.filter(n => n.status === "queued").length} total={dag.nodes.length} label="queued" tone="muted" />
        </div>
      </div>

      {/* DAG canvas + side panel */}
      <div style={{ flex: 1, display: "flex", overflow: "hidden", position: "relative" }}>
        <div style={{
          flex: 1, overflow: "auto", position: "relative",
          background: `
            radial-gradient(circle at 50% 50%, hsl(var(--obsidian)) 0%, hsl(var(--void)) 100%),
            linear-gradient(hsl(var(--graphite) / 0.3) 1px, transparent 1px),
            linear-gradient(90deg, hsl(var(--graphite) / 0.3) 1px, transparent 1px)
          `,
          backgroundSize: "auto, 24px 24px, 24px 24px",
        }}>
          <svg width={totalW} height={totalH} style={{ display: "block" }}>
            <defs>
              <marker id="arrow-c" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
                <path d="M 0 0 L 10 5 L 0 10 z" fill="hsl(var(--cyan) / 0.6)" />
              </marker>
              <marker id="arrow-m" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
                <path d="M 0 0 L 10 5 L 0 10 z" fill="hsl(var(--text-4))" />
              </marker>
            </defs>
            {/* Edges */}
            {dag.edges.map((e, i) => {
              const a = nodePos[e.from], b = nodePos[e.to];
              const fromNode = dag.nodes.find(n => n.id === e.from);
              const toNode = dag.nodes.find(n => n.id === e.to);
              const x1 = a.x + nodeW, y1 = a.y + nodeH / 2;
              const x2 = b.x, y2 = b.y + nodeH / 2;
              const cx = (x1 + x2) / 2;
              const active = fromNode.status === "succeeded" && (toNode.status === "running" || toNode.status === "queued");
              const stroke = active ? "hsl(var(--cyan))" : fromNode.status === "succeeded" ? "hsl(var(--success) / 0.5)" : "hsl(var(--text-4) / 0.5)";
              return (
                <g key={i}>
                  <path d={`M${x1},${y1} C${cx},${y1} ${cx},${y2} ${x2},${y2}`}
                    fill="none" stroke={stroke} strokeWidth={1.5}
                    markerEnd={active ? "url(#arrow-c)" : "url(#arrow-m)"} />
                  {active ? (
                    <circle r="3" fill="hsl(var(--cyan-glow))">
                      <animateMotion dur="1.6s" repeatCount="indefinite" path={`M${x1},${y1} C${cx},${y1} ${cx},${y2} ${x2},${y2}`} />
                    </circle>
                  ) : null}
                </g>
              );
            })}
            {/* Nodes */}
            {dag.nodes.map((n) => {
              const p = nodePos[n.id];
              const isSel = selected === n.id;
              const tone = n.status === "succeeded" ? "hsl(var(--success))" :
                           n.status === "running" ? "hsl(var(--cyan))" :
                           n.status === "failed" ? "hsl(var(--danger))" :
                           "hsl(var(--text-4))";
              return (
                <g key={n.id} transform={`translate(${p.x}, ${p.y})`} style={{ cursor: "pointer" }} onClick={() => setSel(n.id)}>
                  <rect width={nodeW} height={nodeH} rx={8}
                    fill="hsl(var(--midnight))" stroke={isSel ? "hsl(var(--cyan))" : "hsl(var(--graphite))"} strokeWidth={isSel ? 1.5 : 1} />
                  {/* status edge */}
                  <rect x={0} y={0} width={3} height={nodeH} rx={1.5} fill={tone}>
                    {n.status === "running" ? <animate attributeName="opacity" values="0.5;1;0.5" dur="1.4s" repeatCount="indefinite"/> : null}
                  </rect>
                  {n.status === "running" ? (
                    <rect x={0} y={0} width={nodeW} height={nodeH} rx={8} fill="none" stroke="hsl(var(--cyan))" strokeWidth={1} opacity={0.6}>
                      <animate attributeName="opacity" values="0.2;0.8;0.2" dur="1.4s" repeatCount="indefinite"/>
                    </rect>
                  ) : null}
                  <text x={14} y={22} fill="hsl(var(--text-1))" fontSize={13} fontWeight={500} fontFamily="var(--font-sans)">{n.label}</text>
                  <text x={14} y={40} fill="hsl(var(--text-3))" fontSize={10.5} fontFamily="var(--font-mono)">{n.image}</text>
                  {/* status row */}
                  <g transform={`translate(14, 56)`}>
                    <circle cx={3} cy={3} r={3} fill={tone}>
                      {n.status === "running" ? <animate attributeName="opacity" values="0.4;1;0.4" dur="1.2s" repeatCount="indefinite"/> : null}
                    </circle>
                    <text x={12} y={6} fill={tone} fontSize={10} fontFamily="var(--font-sans)" fontWeight={600} letterSpacing="0.06em" style={{ textTransform: "uppercase" }}>
                      {n.status}{n.cached ? " · cached" : ""}
                    </text>
                    {n.duration ? (
                      <text x={nodeW - 28} y={6} fill="hsl(var(--text-3))" fontSize={10} fontFamily="var(--font-mono)" textAnchor="end">{n.duration}s</text>
                    ) : null}
                  </g>
                </g>
              );
            })}
          </svg>
        </div>

        {/* Side panel */}
        {selected ? (
          <div className="fade-up" style={{ width: 360, borderLeft: "1px solid hsl(var(--graphite))", background: "hsl(var(--midnight))", overflow: "auto", display: "flex", flexDirection: "column" }}>
            <TaskPanel node={dag.nodes.find(n => n.id === selected)} onClose={() => setSel(null)} />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function Stat({ label, value, mono, accent }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 1 }}>
      <span className="eyebrow" style={{ fontSize: 9 }}>{label}</span>
      <span className={mono ? "mono tnum" : "tnum"} style={{ fontSize: 12, color: accent ? "hsl(var(--gold))" : "hsl(var(--text-1))", fontWeight: 500 }}>{value}</span>
    </div>
  );
}
function Mini({ count, total, label, tone }) {
  const colors = { success: "hsl(var(--success))", cyan: "hsl(var(--cyan-glow))", teal: "hsl(178 60% 60%)", muted: "hsl(var(--text-3))" };
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
      <span className="mono tnum" style={{ color: colors[tone], fontSize: 13, fontWeight: 600 }}>{count}</span>
      <span style={{ color: "hsl(var(--text-3))", fontSize: 11 }}>{label}</span>
    </div>
  );
}
function TaskPanel({ node, onClose }) {
  if (!node) return null;
  return (
    <>
      <div style={{ padding: "16px 18px", borderBottom: "1px solid hsl(var(--graphite))", display: "flex", justifyContent: "space-between", alignItems: "flex-start" }}>
        <div>
          <div className="eyebrow" style={{ marginBottom: 4 }}>Task</div>
          <div style={{ fontSize: 16, fontWeight: 500 }}>{node.label}</div>
          <div className="mono" style={{ marginTop: 4, fontSize: 11, color: "hsl(var(--text-3))" }}>{node.image}</div>
        </div>
        <button onClick={onClose} style={{ background: "transparent", border: "none", color: "hsl(var(--text-3))", cursor: "pointer", padding: 4 }}>
          <I.close width={16} height={16} />
        </button>
      </div>
      <div style={{ padding: 18, display: "flex", flexDirection: "column", gap: 16 }}>
        <div>
          <div className="eyebrow" style={{ marginBottom: 8 }}>Status</div>
          <StatusBadge status={node.status} />
        </div>
        <div>
          <div className="eyebrow" style={{ marginBottom: 8 }}>Timing</div>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10, fontSize: 12 }}>
            <KV k="Started" v={node.status !== "queued" ? "12:42:18 UTC" : "—"} />
            <KV k="Duration" v={node.duration ? `${node.duration}s` : "—"} />
            <KV k="Engine" v="docker" />
            <KV k="Attempt" v="1 / 3" />
          </div>
        </div>
        <div>
          <div className="eyebrow" style={{ marginBottom: 8 }}>Command</div>
          <pre className="mono" style={{ margin: 0, padding: 12, background: "hsl(var(--void))", border: "1px solid hsl(var(--graphite))", borderRadius: 6, fontSize: 11, color: "hsl(var(--text-2))", overflow: "auto" }}>
{`sh -c "dbt run --select ${node.label.split(".")[1]} \\
  --target warehouse --threads 4"`}
          </pre>
        </div>
        {node.status === "running" ? (
          <div>
            <div className="eyebrow" style={{ marginBottom: 8 }}>Live tail</div>
            <div className="mono" style={{ background: "hsl(var(--void))", border: "1px solid hsl(var(--graphite))", borderRadius: 6, padding: 10, fontSize: 11, color: "hsl(var(--text-2))", maxHeight: 160, overflow: "auto", lineHeight: 1.7 }}>
              {window.MOCK.LOG_LINES.slice(0, 6).map((l, i) => (
                <div key={i}><span style={{ color: "hsl(var(--text-4))" }}>{i.toString().padStart(2, "0")}</span> <span style={{ color: l.level === "WARN" ? "hsl(var(--gold))" : l.level === "DEBUG" ? "hsl(var(--text-3))" : "hsl(var(--cyan-glow))" }}>{l.level}</span> {l.msg}</div>
              ))}
              <div style={{ color: "hsl(var(--cyan))" }}>▎</div>
            </div>
          </div>
        ) : null}
        <Btn variant="outline" size="md" style={{ width: "100%" }}>Open full task</Btn>
      </div>
    </>
  );
}
function KV({ k, v }) {
  return (
    <div>
      <div style={{ fontSize: 10, color: "hsl(var(--text-3))", textTransform: "uppercase", letterSpacing: "0.1em" }}>{k}</div>
      <div className="mono tnum" style={{ marginTop: 2, color: "hsl(var(--text-1))" }}>{v}</div>
    </div>
  );
}

window.PAGES = { JobsPage, JobDetailPage };
