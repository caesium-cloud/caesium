import { Link } from "@tanstack/react-router";
import { BarChart, Database, FileCode2, LayoutDashboard, Radio, Server } from "lucide-react";

export function Sidebar() {
  const navItems = [
    { to: "/jobs", label: "Jobs", icon: LayoutDashboard },
    { to: "/triggers", label: "Triggers", icon: Radio },
    { to: "/atoms", label: "Atoms", icon: Database },
    { to: "/stats", label: "Stats", icon: BarChart },
    { to: "/system", label: "System", icon: Server },
    { to: "/jobdefs", label: "JobDefs", icon: FileCode2 },
  ];

  return (
    <aside className="w-64 border-r bg-card flex flex-col">
      <div className="h-14 flex items-center px-6 border-b">
        <span className="font-bold text-lg">Caesium</span>
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
