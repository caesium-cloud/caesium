import { Link } from "@tanstack/react-router";
import { BarChart, Database, FileCode2, LayoutDashboard, Radio, Server, TerminalSquare } from "lucide-react";
import { CaesiumLogo } from "@/components/caesium-logo";

export function Sidebar() {
  const navItems = [
    { to: "/jobs", label: "Jobs", icon: LayoutDashboard },
    { to: "/triggers", label: "Triggers", icon: Radio },
    { to: "/atoms", label: "Atoms", icon: Database },
    { to: "/database", label: "Database", icon: TerminalSquare },
    { to: "/stats", label: "Stats", icon: BarChart },
    { to: "/system", label: "System", icon: Server },
    { to: "/jobdefs", label: "JobDefs", icon: FileCode2 },
  ];

  return (
    <aside className="flex w-72 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground shadow-2xl shadow-sidebar/30">
      <div className="border-b border-sidebar-border px-6 py-5">
        <div className="flex items-center gap-3">
          <CaesiumLogo className="h-10 w-10 shrink-0 drop-shadow-[0_0_24px_rgba(0,180,216,0.35)]" />
          <div className="min-w-0">
            <div className="text-[0.62rem] font-medium uppercase tracking-[0.38em] text-caesium-gold/80">Control Plane</div>
            <div className="truncate text-lg font-semibold uppercase tracking-[0.34em] text-sidebar-foreground">Caesium</div>
          </div>
        </div>
      </div>
      <nav className="flex-1 space-y-2 p-4">
        {navItems.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            activeProps={{ className: "bg-sidebar-accent text-sidebar-foreground shadow-[inset_0_0_0_1px_rgba(0,180,216,0.25)]" }}
            inactiveProps={{ className: "text-sidebar-muted hover:bg-sidebar-accent/50 hover:text-sidebar-foreground" }}
            className="flex items-center gap-3 rounded-xl px-3 py-2.5 text-sm font-medium transition-all"
          >
            <item.icon className="h-4 w-4 text-caesium-gold" />
            {item.label}
          </Link>
        ))}
      </nav>
    </aside>
  );
}
