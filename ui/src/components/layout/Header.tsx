import { ModeToggle } from "../mode-toggle";
import { CommandMenu } from "../command-menu";

export function Header() {
  return (
    <header className="h-14 border-b flex items-center justify-between px-6 bg-card">
      <div className="flex items-center gap-4">
        <CommandMenu />
      </div>
      <div className="flex items-center gap-2">
        <ModeToggle />
      </div>
    </header>
  );
}
