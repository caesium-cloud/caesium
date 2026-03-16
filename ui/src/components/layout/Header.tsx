import { ModeToggle } from "../mode-toggle";
import { CommandMenu } from "../command-menu";

export function Header() {
  return (
    <header className="flex h-14 items-center justify-between border-b border-border/70 bg-background/80 px-6 backdrop-blur">
      <div className="flex items-center gap-4">
        <div className="hidden items-center gap-2 lg:flex">
          <span className="h-2 w-2 rounded-full bg-caesium-gold shadow-[0_0_18px_rgba(245,158,11,0.45)]" />
          <span className="text-[0.62rem] font-medium uppercase tracking-[0.34em] text-muted-foreground">Operator Console</span>
        </div>
        <CommandMenu />
      </div>
      <div className="flex items-center gap-2">
        <ModeToggle />
      </div>
    </header>
  );
}
