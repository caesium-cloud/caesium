/* === Caesium Refresh — Layout (Sidebar + Header) === */

const { useState: useStateL, useEffect: useEffectL } = React;

function Sidebar({ route, onNavigate, density }) {
  const items = [
    { to: "jobs",     label: "Jobs",     icon: window.UI.I.jobs,     count: 8 },
    { to: "triggers", label: "Triggers", icon: window.UI.I.triggers, count: 8 },
    { to: "atoms",    label: "Atoms",    icon: window.UI.I.atoms,    count: 24 },
    { to: "stats",    label: "Stats",    icon: window.UI.I.stats },
    { to: "system",   label: "System",   icon: window.UI.I.system },
    { to: "jobdefs",  label: "JobDefs",  icon: window.UI.I.jobdefs,  count: 12 },
  ];
  const w = density === "compact" ? 220 : 248;
  const active = route.split("/")[0];
  return (
    <aside style={{
      width: w, flexShrink: 0,
      borderRight: "1px solid hsl(var(--graphite))",
      background: "linear-gradient(180deg, hsl(var(--midnight)), hsl(var(--void)))",
      display: "flex", flexDirection: "column",
      position: "relative",
    }}>
      {/* gold accent rail */}
      <div style={{ position: "absolute", left: 0, top: "10%", bottom: "10%", width: 1.5, background: "linear-gradient(180deg, transparent, hsl(var(--gold) / 0.5), transparent)" }} />

      <div style={{ padding: "18px 18px 16px", borderBottom: "1px solid hsl(var(--graphite))" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <window.UI.AtomLogo size={42} />
          <div style={{ minWidth: 0 }}>
            <div className="eyebrow" style={{ color: "hsl(var(--gold) / 0.85)", fontSize: 9 }}>Control Plane</div>
            <div style={{ fontSize: 17, fontWeight: 300, letterSpacing: "0.34em", textTransform: "uppercase", color: "hsl(var(--text-1))", marginTop: 2 }}>caesium</div>
          </div>
        </div>
        <div className="mono" style={{ marginTop: 12, fontSize: 10, color: "hsl(var(--text-3))", display: "flex", justifyContent: "space-between" }}>
          <span>v0.14.2</span>
          <span style={{ color: "hsl(var(--success))" }}>● 3 nodes</span>
        </div>
      </div>

      <nav style={{ padding: 10, flex: 1, display: "flex", flexDirection: "column", gap: 2 }}>
        {items.map((it) => {
          const isActive = active === it.to;
          return (
            <button key={it.to} onClick={() => onNavigate(it.to)}
              style={{
                display: "flex", alignItems: "center", gap: 11,
                padding: "9px 12px", borderRadius: 8,
                background: isActive ? "linear-gradient(90deg, hsl(var(--cyan) / 0.14), hsl(var(--cyan) / 0.04))" : "transparent",
                border: "1px solid", borderColor: isActive ? "hsl(var(--cyan) / 0.3)" : "transparent",
                color: isActive ? "hsl(var(--text-1))" : "hsl(var(--text-2))",
                fontSize: 13, fontWeight: isActive ? 500 : 400,
                cursor: "pointer", textAlign: "left", width: "100%",
                position: "relative",
                transition: "all 140ms",
              }}
              onMouseEnter={(e) => !isActive && (e.currentTarget.style.background = "hsl(var(--obsidian))", e.currentTarget.style.color = "hsl(var(--text-1))")}
              onMouseLeave={(e) => !isActive && (e.currentTarget.style.background = "transparent", e.currentTarget.style.color = "hsl(var(--text-2))")}
            >
              {isActive ? <span style={{ position: "absolute", left: -10, top: 8, bottom: 8, width: 2, background: "hsl(var(--gold))", borderRadius: 2, boxShadow: "0 0 8px hsl(var(--gold) / 0.7)" }} /> : null}
              <it.icon width={16} height={16} style={{ color: isActive ? "hsl(var(--cyan-glow))" : "hsl(var(--text-3))" }} />
              <span style={{ flex: 1 }}>{it.label}</span>
              {it.count != null ? (
                <span className="mono tnum" style={{
                  fontSize: 10, color: "hsl(var(--text-3))",
                  padding: "2px 6px", borderRadius: 4,
                  background: "hsl(var(--obsidian))",
                  border: "1px solid hsl(var(--graphite))",
                }}>{it.count}</span>
              ) : null}
            </button>
          );
        })}
      </nav>

      <div style={{ padding: 14, borderTop: "1px solid hsl(var(--graphite))" }}>
        <div className="eyebrow" style={{ marginBottom: 8, fontSize: 9 }}>Cluster Health</div>
        <div style={{ display: "flex", flexDirection: "column", gap: 6, fontSize: 11 }}>
          <Row k="API" v="healthy" tone="success" />
          <Row k="Workers" v="3 / 3" tone="success" />
          <Row k="Queue depth" v="2" tone="text" />
        </div>
      </div>
    </aside>
  );
}
function Row({ k, v, tone }) {
  const colors = { success: "hsl(var(--success))", warn: "hsl(var(--gold))", danger: "hsl(var(--danger))", text: "hsl(var(--text-2))" };
  return (
    <div style={{ display: "flex", justifyContent: "space-between" }}>
      <span style={{ color: "hsl(var(--text-3))" }}>{k}</span>
      <span className="mono tnum" style={{ color: colors[tone] }}>{v}</span>
    </div>
  );
}

function Header({ theme, setTheme, onSearch, route }) {
  const crumbs = route.split("/").filter(Boolean);
  return (
    <header style={{
      height: 54, flexShrink: 0,
      borderBottom: "1px solid hsl(var(--graphite))",
      background: "hsl(var(--void) / 0.7)",
      backdropFilter: "blur(12px)",
      display: "flex", alignItems: "center", padding: "0 22px", gap: 16,
    }}>
      {/* Breadcrumb */}
      <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12, color: "hsl(var(--text-3))" }}>
        <span className="eyebrow" style={{ color: "hsl(var(--gold) / 0.7)" }}>Operator</span>
        <span style={{ color: "hsl(var(--text-4))" }}>/</span>
        {crumbs.map((c, i) => (
          <React.Fragment key={i}>
            <span style={{ color: i === crumbs.length - 1 ? "hsl(var(--text-1))" : "hsl(var(--text-3))" }}>{c}</span>
            {i < crumbs.length - 1 ? <span style={{ color: "hsl(var(--text-4))" }}>/</span> : null}
          </React.Fragment>
        ))}
      </div>

      {/* Command bar */}
      <button onClick={onSearch} style={{
        marginLeft: 10, flex: 1, maxWidth: 460,
        display: "flex", alignItems: "center", gap: 10,
        height: 32, padding: "0 12px",
        borderRadius: 6,
        background: "hsl(var(--obsidian) / 0.6)",
        border: "1px solid hsl(var(--graphite))",
        color: "hsl(var(--text-3))", fontSize: 12,
        cursor: "pointer", textAlign: "left",
      }}>
        <window.UI.I.search width={13} height={13} />
        <span>Search jobs, runs, atoms…</span>
        <span style={{ marginLeft: "auto", display: "flex", gap: 3 }}>
          <kbd style={{ padding: "1px 6px", fontSize: 10, borderRadius: 3, background: "hsl(var(--graphite))", color: "hsl(var(--text-2))", fontFamily: "var(--font-mono)" }}>⌘</kbd>
          <kbd style={{ padding: "1px 6px", fontSize: 10, borderRadius: 3, background: "hsl(var(--graphite))", color: "hsl(var(--text-2))", fontFamily: "var(--font-mono)" }}>K</kbd>
        </span>
      </button>

      <div style={{ flex: 1 }} />

      {/* Live UTC clock */}
      <window.UI.UTCClock />

      <div style={{ width: 1, height: 22, background: "hsl(var(--graphite))" }} />

      {/* Theme toggle */}
      <button onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
        style={{ width: 32, height: 32, borderRadius: 6, background: "transparent", border: "1px solid hsl(var(--graphite))", color: "hsl(var(--text-2))", cursor: "pointer", display: "flex", alignItems: "center", justifyContent: "center" }}>
        {theme === "dark" ? <window.UI.I.sun width={14} height={14} /> : <window.UI.I.moon width={14} height={14} />}
      </button>

      {/* Notifications */}
      <button style={{ width: 32, height: 32, borderRadius: 6, background: "transparent", border: "1px solid hsl(var(--graphite))", color: "hsl(var(--text-2))", cursor: "pointer", display: "flex", alignItems: "center", justifyContent: "center", position: "relative" }}>
        <window.UI.I.bell width={14} height={14} />
        <span style={{ position: "absolute", top: 6, right: 6, width: 6, height: 6, borderRadius: 99, background: "hsl(var(--danger))", boxShadow: "0 0 8px hsl(var(--danger) / 0.7)" }} />
      </button>
    </header>
  );
}

window.LAYOUT = { Sidebar, Header };
