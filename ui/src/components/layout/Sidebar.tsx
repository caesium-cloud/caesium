import { Link } from "@tanstack/react-router";
import { LayoutDashboard, BarChart, Box, Activity, Zap, FileCode2 } from "lucide-react";
import { CaesiumLogo } from "@/components/caesium-logo";

export function Sidebar() {
  const navItems = [
    { to: "/jobs", label: "Jobs", icon: LayoutDashboard },
    { to: "/stats", label: "Stats", icon: BarChart },
    { to: "/atoms", label: "Atoms", icon: Box },
    { to: "/triggers", label: "Triggers", icon: Zap },
    { to: "/jobdefs", label: "Job Defs", icon: FileCode2 },
    { to: "/system", label: "System", icon: Activity },
  ];

  return (
    <aside className="w-64 border-r bg-card flex flex-col">
      <div className="h-14 flex items-center px-6 border-b gap-2.5">
        <CaesiumLogo className="h-7 w-7 shrink-0" />
        <span className="font-light text-lg tracking-widest">caesium</span>
      </div>
      <nav className="flex-1 p-4 space-y-2">
        {navItems.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            activeProps={{ className: "bg-secondary text-primary" }}
            inactiveProps={{ className: "text-muted-foreground hover:bg-muted" }}
            className="flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-all"
          >
            <item.icon className="h-4 w-4" />
            {item.label}
          </Link>
        ))}
      </nav>
    </aside>
  );
}
