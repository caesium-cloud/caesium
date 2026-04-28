/* === Caesium Refresh — Shared Components & Hooks === */

const { useState, useEffect, useRef, useMemo, useCallback, createContext, useContext } = React;

/* ---- Atom logo (animated) ---- */
function AtomLogo({ size = 40, animated = true, className = "" }) {
  return (
    <svg viewBox="0 0 512 512" width={size} height={size} className={className} style={{ display: "block" }}>
      <defs>
        <radialGradient id="atom-nuc-glow" cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="hsl(191 100% 60%)" stopOpacity="0.55" />
          <stop offset="100%" stopColor="hsl(191 100% 60%)" stopOpacity="0" />
        </radialGradient>
      </defs>
      <circle cx="256" cy="256" r="76" fill="url(#atom-nuc-glow)" />
      <g style={animated ? { transformOrigin: "256px 256px", animation: "orbit-spin 22s linear infinite" } : null}>
        <ellipse cx="256" cy="256" rx="210" ry="70" stroke="hsl(var(--cyan))" strokeWidth="3.5" opacity="0.9" transform="rotate(-60 256 256)" />
      </g>
      <g style={animated ? { transformOrigin: "256px 256px", animation: "orbit-spin 30s linear infinite reverse" } : null}>
        <ellipse cx="256" cy="256" rx="210" ry="70" stroke="hsl(var(--cyan))" strokeWidth="3.5" opacity="0.85" />
      </g>
      <g style={animated ? { transformOrigin: "256px 256px", animation: "orbit-spin 38s linear infinite" } : null}>
        <ellipse cx="256" cy="256" rx="210" ry="70" stroke="hsl(var(--cyan))" strokeWidth="3.5" opacity="0.9" transform="rotate(60 256 256)" />
      </g>
      <circle cx="256" cy="256" r="20" fill="hsl(var(--cyan))" style={animated ? { transformOrigin: "256px 256px", animation: "nucleus-pulse 2.4s ease-in-out infinite" } : null} />
      <circle cx="361" cy="74"  r="10" fill="hsl(var(--gold))" />
      <circle cx="46"  cy="256" r="10" fill="hsl(var(--gold))" />
      <circle cx="361" cy="438" r="10" fill="hsl(var(--gold))" />
    </svg>
  );
}

/* ---- Lucide-style icons (inline SVG) ---- */
const I = {
  jobs:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></svg>,
  triggers: (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M4.93 19.07a10 10 0 0 1 0-14.14"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14"/><path d="M7.76 16.24a6 6 0 0 1 0-8.48"/><path d="M16.24 7.76a6 6 0 0 1 0 8.48"/><circle cx="12" cy="12" r="2"/></svg>,
  atoms:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><ellipse cx="12" cy="12" rx="10" ry="4"/><ellipse cx="12" cy="12" rx="10" ry="4" transform="rotate(60 12 12)"/><ellipse cx="12" cy="12" rx="10" ry="4" transform="rotate(120 12 12)"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/></svg>,
  stats:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M3 3v18h18"/><path d="M7 14l4-4 3 3 5-7"/></svg>,
  system:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><circle cx="7" cy="7" r="0.6" fill="currentColor"/><circle cx="7" cy="17" r="0.6" fill="currentColor"/></svg>,
  jobdefs:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/><path d="M9 14l-2 2 2 2"/><path d="M15 14l2 2-2 2"/></svg>,
  play:     (p) => <svg {...p} viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>,
  pause:    (p) => <svg {...p} viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="5" width="4" height="14" rx="0.8"/><rect x="14" y="5" width="4" height="14" rx="0.8"/></svg>,
  search:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/></svg>,
  command:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M18 3a3 3 0 0 0-3 3v12a3 3 0 0 0 3 3 3 3 0 0 0 3-3 3 3 0 0 0-3-3H6a3 3 0 0 0-3 3 3 3 0 0 0 3 3 3 3 0 0 0 3-3V6a3 3 0 0 0-3-3 3 3 0 0 0-3 3 3 3 0 0 0 3 3h12a3 3 0 0 0 3-3 3 3 0 0 0-3-3z"/></svg>,
  bell:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 8 3 8H3s3-1 3-8"/><path d="M10 21a2 2 0 0 0 4 0"/></svg>,
  sun:      (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"/></svg>,
  moon:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>,
  back:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M15 18l-6-6 6-6"/></svg>,
  chevron:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9 18l6-6-6-6"/></svg>,
  zap:      (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M13 2L3 14h9l-1 8 10-12h-9z"/></svg>,
  cron:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>,
  webhook:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M18 16.08c-.76 0-1.44.3-1.96.77L8.91 12.7c.05-.23.09-.46.09-.7s-.04-.47-.09-.7l7.05-4.11c.54.5 1.25.81 2.04.81 1.66 0 3-1.34 3-3s-1.34-3-3-3-3 1.34-3 3c0 .24.04.47.09.7L8.04 9.81C7.5 9.31 6.79 9 6 9c-1.66 0-3 1.34-3 3s1.34 3 3 3c.79 0 1.5-.31 2.04-.81l7.12 4.16c-.05.21-.08.43-.08.65 0 1.61 1.31 2.92 2.92 2.92 1.61 0 2.92-1.31 2.92-2.92s-1.31-2.92-2.92-2.92z"/></svg>,
  cache:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/><path d="M3 12c0 1.66 4 3 9 3s9-1.34 9-3"/></svg>,
  history:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/></svg>,
  yaml:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/></svg>,
  list:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>,
  cog:      (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h0a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h0a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v0a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>,
  calendar: (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="18" rx="2"/><path d="M16 2v4M8 2v4M3 10h18"/></svg>,
  close:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M18 6L6 18M6 6l12 12"/></svg>,
  dot:      (p) => <svg {...p} viewBox="0 0 24 24" fill="currentColor"><circle cx="12" cy="12" r="3"/></svg>,
  check:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5"/></svg>,
  x:        (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><path d="M18 6L6 18M6 6l12 12"/></svg>,
  filter:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M22 3H2l8 9.46V19l4 2v-8.54L22 3z"/></svg>,
  refresh:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M3 12a9 9 0 0 1 15-6.7L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-15 6.7L3 16"/><path d="M3 21v-5h5"/></svg>,
  database: (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/><path d="M3 12c0 1.66 4 3 9 3s9-1.34 9-3"/></svg>,
  activity: (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>,
  server:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><rect x="2" y="3" width="20" height="8" rx="1.5"/><rect x="2" y="13" width="20" height="8" rx="1.5"/><path d="M6 7h.01M6 17h.01"/></svg>,
  terminal: (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M4 17l6-6-6-6"/><path d="M12 19h8"/></svg>,
  scroll:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M8 21h12a2 2 0 0 0 2-2v-2H10v2a2 2 0 1 1-4 0V5a2 2 0 1 0-4 0v3h4"/><path d="M19 17V5a2 2 0 0 0-2-2H4"/></svg>,
  broom:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M19.36 2.64a2 2 0 0 0-2.83 0L9 10.17l4.83 4.83 7.53-7.53a2 2 0 0 0 0-2.83zM9 10.17L4 15a3 3 0 0 0 4.24 4.24L13.83 15"/></svg>,
  clock:    (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>,
  spinner:  (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M12 3a9 9 0 0 1 9 9" style={{ transformOrigin: "center" }}><animateTransform attributeName="transform" type="rotate" from="0 12 12" to="360 12 12" dur="0.9s" repeatCount="indefinite"/></path></svg>,
  upload:   (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="M17 8l-5-5-5 5"/><path d="M12 3v12"/></svg>,
  git:      (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><circle cx="6" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="12" r="3"/><path d="M6 9v6"/><path d="M15 12H9a3 3 0 0 0-3 3"/></svg>,
  file:     (p) => <svg {...p} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/></svg>,
};

/* ---- Time helpers ---- */
function timeAgo(iso) {
  const d = (Date.now() - new Date(iso).getTime()) / 1000;
  if (d < 60) return `${Math.max(1, Math.floor(d))}s ago`;
  if (d < 3600) return `${Math.floor(d / 60)}m ago`;
  if (d < 86400) return `${Math.floor(d / 3600)}h ago`;
  return `${Math.floor(d / 86400)}d ago`;
}
function fmtDuration(s) {
  if (s == null) return "—";
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  if (s < 3600) return `${Math.floor(s/60)}m ${Math.round(s%60)}s`;
  return `${Math.floor(s/3600)}h ${Math.floor((s%3600)/60)}m`;
}
function utcClock(date = new Date()) {
  const pad = (n) => String(n).padStart(2, "0");
  return `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`;
}

/* ---- StatusBadge ---- */
function StatusBadge({ status, size = "md", subtle = false }) {
  const map = {
    running:   { label: "running",   bg: "hsl(var(--cyan) / 0.14)",    fg: "hsl(var(--cyan-glow))",  bd: "hsl(var(--cyan) / 0.4)",  dot: "running" },
    succeeded: { label: "succeeded", bg: "hsl(var(--success) / 0.12)", fg: "hsl(var(--success))",     bd: "hsl(var(--success) / 0.3)", dot: "success" },
    failed:    { label: "failed",    bg: "hsl(var(--danger) / 0.12)",  fg: "hsl(var(--danger))",      bd: "hsl(var(--danger) / 0.35)", dot: "failed" },
    queued:    { label: "queued",    bg: "hsl(var(--text-3) / 0.12)",  fg: "hsl(var(--text-2))",      bd: "hsl(var(--text-3) / 0.25)", dot: "queued" },
    paused:    { label: "paused",    bg: "hsl(var(--gold) / 0.12)",    fg: "hsl(var(--gold))",        bd: "hsl(var(--gold) / 0.35)",   dot: "paused" },
    cached:    { label: "cached",    bg: "hsl(178 60% 50% / 0.12)",    fg: "hsl(178 60% 60%)",        bd: "hsl(178 60% 50% / 0.3)",    dot: "cached" },
    skipped:   { label: "skipped",   bg: "hsl(var(--text-4) / 0.18)",  fg: "hsl(var(--text-3))",      bd: "hsl(var(--text-4) / 0.3)",  dot: "skipped" },
  };
  const s = map[status] || map.queued;
  const pad = size === "sm" ? "2px 7px" : "3px 9px";
  const fs = size === "sm" ? "10px" : "11px";
  return (
    <span style={{
      display: "inline-flex", alignItems: "center", gap: 6,
      padding: pad, borderRadius: 999,
      background: subtle ? "transparent" : s.bg,
      border: `1px solid ${subtle ? "transparent" : s.bd}`,
      color: s.fg,
      fontSize: fs, fontWeight: 600, letterSpacing: "0.04em",
      textTransform: "uppercase", whiteSpace: "nowrap",
    }}>
      <span className={`dot ${s.dot}`} style={{ width: 6, height: 6 }} />
      {s.label}
    </span>
  );
}

/* ---- Sparkline (run history) ---- */
function Sparkline({ runs, height = 22, width = 90 }) {
  if (!runs || !runs.length) return <span style={{ color: "hsl(var(--text-4))", fontSize: 11 }}>—</span>;
  const max = Math.max(...runs.map(r => r.duration || 0), 60);
  const barW = (width - (runs.length - 1) * 2) / runs.length;
  return (
    <svg width={width} height={height} style={{ display: "block" }}>
      {runs.slice().reverse().map((r, i) => {
        const h = r.status === "running" ? height * 0.6 : Math.max(3, ((r.duration || 0) / max) * height);
        const color =
          r.status === "succeeded" ? "hsl(var(--success))" :
          r.status === "failed"    ? "hsl(var(--danger))" :
          r.status === "running"   ? "hsl(var(--cyan))" :
          "hsl(var(--text-3))";
        return (
          <rect key={i}
            x={i * (barW + 2)} y={height - h}
            width={barW} height={h}
            rx={1.5}
            fill={color}
            opacity={r.status === "running" ? 0.95 : 0.85}>
            {r.status === "running" ? <animate attributeName="opacity" values="0.6;1;0.6" dur="1.4s" repeatCount="indefinite"/> : null}
          </rect>
        );
      })}
    </svg>
  );
}

/* ---- Button ---- */
function Btn({ variant = "default", size = "md", children, onClick, disabled, title, icon: Icon, style, ...rest }) {
  const sizes = {
    sm:  { h: 28, px: 10, fs: 12, gap: 6 },
    md:  { h: 32, px: 12, fs: 13, gap: 6 },
    lg:  { h: 38, px: 16, fs: 14, gap: 8 },
    icon:{ h: 32, w: 32, fs: 13, gap: 0 },
  };
  const s = sizes[size];
  const base = {
    display: "inline-flex", alignItems: "center", justifyContent: "center",
    gap: s.gap, height: s.h, padding: size === "icon" ? 0 : `0 ${s.px}px`,
    width: size === "icon" ? s.w : undefined,
    borderRadius: 6, fontSize: s.fs, fontWeight: 500,
    cursor: disabled ? "not-allowed" : "pointer", opacity: disabled ? 0.5 : 1,
    border: "1px solid transparent", whiteSpace: "nowrap",
    transition: "background 120ms, color 120ms, border-color 120ms, transform 120ms",
  };
  const variants = {
    default:  { background: "hsl(var(--cyan))", color: "hsl(var(--void))", borderColor: "hsl(var(--cyan))" },
    outline:  { background: "transparent", color: "hsl(var(--text-1))", borderColor: "hsl(var(--graphite))" },
    ghost:    { background: "transparent", color: "hsl(var(--text-2))", borderColor: "transparent" },
    danger:   { background: "hsl(var(--danger) / 0.12)", color: "hsl(var(--danger))", borderColor: "hsl(var(--danger) / 0.4)" },
    primary:  { background: "hsl(var(--cyan))", color: "hsl(var(--void))", borderColor: "hsl(var(--cyan))" },
  };
  return (
    <button onClick={onClick} disabled={disabled} title={title}
      style={{ ...base, ...variants[variant], ...style }}
      onMouseEnter={(e) => !disabled && (e.currentTarget.style.transform = "translateY(-1px)")}
      onMouseLeave={(e) => (e.currentTarget.style.transform = "translateY(0)")}
      {...rest}>
      {Icon ? <Icon width={size === "sm" ? 12 : 14} height={size === "sm" ? 12 : 14} /> : null}
      {children}
    </button>
  );
}

/* ---- Toast (sonner-ish) ---- */
const ToastContext = createContext({ push: () => {} });
function ToastProvider({ children }) {
  const [list, setList] = useState([]);
  const push = useCallback((msg, kind = "default") => {
    const id = Math.random().toString(36).slice(2);
    setList((l) => [...l, { id, msg, kind }]);
    setTimeout(() => setList((l) => l.filter((t) => t.id !== id)), 3200);
  }, []);
  return (
    <ToastContext.Provider value={{ push }}>
      {children}
      <div style={{ position: "fixed", right: 20, bottom: 20, zIndex: 90, display: "flex", flexDirection: "column", gap: 8 }}>
        {list.map((t) => (
          <div key={t.id} className="fade-up" style={{
            minWidth: 280, padding: "10px 14px",
            background: "hsl(var(--obsidian))",
            border: `1px solid ${t.kind === "error" ? "hsl(var(--danger) / 0.5)" : t.kind === "success" ? "hsl(var(--success) / 0.5)" : "hsl(var(--graphite))"}`,
            borderRadius: 8, fontSize: 13,
            display: "flex", alignItems: "center", gap: 10,
            boxShadow: "0 14px 30px -10px hsl(var(--void) / 0.6)",
          }}>
            <span className={`dot ${t.kind === "error" ? "failed" : t.kind === "success" ? "success" : "running"}`} />
            {t.msg}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
const useToast = () => useContext(ToastContext);

/* ---- Live UTC clock with tick ---- */
function UTCClock() {
  const [t, setT] = useState(new Date());
  useEffect(() => { const i = setInterval(() => setT(new Date()), 500); return () => clearInterval(i); }, []);
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
      <span style={{ width: 7, height: 7, borderRadius: 99, background: "hsl(var(--gold))", display: "inline-block",
                     boxShadow: "0 0 12px hsl(var(--gold) / 0.7)",
                     animation: "gold-pulse 1.6s ease-out infinite" }} />
      <span className="mono tnum" style={{ fontSize: 11, letterSpacing: "0.12em", color: "hsl(var(--text-2))" }}>
        {utcClock(t)} UTC
      </span>
    </div>
  );
}

/* ---- Empty state with orbit motif ---- */
function EmptyState({ title, subtitle, action }) {
  return (
    <div style={{ padding: "60px 20px", textAlign: "center", display: "flex", flexDirection: "column", alignItems: "center", gap: 14 }}>
      <div style={{ width: 80, height: 80, opacity: 0.7 }}>
        <AtomLogo size={80} />
      </div>
      <div style={{ fontSize: 16, fontWeight: 500 }}>{title}</div>
      {subtitle ? <div style={{ color: "hsl(var(--text-3))", fontSize: 13, maxWidth: 380 }}>{subtitle}</div> : null}
      {action || null}
    </div>
  );
}

window.UI = { AtomLogo, I, StatusBadge, Sparkline, Btn, ToastProvider, useToast, UTCClock, EmptyState, timeAgo, fmtDuration };
