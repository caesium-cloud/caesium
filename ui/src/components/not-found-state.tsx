import { Link } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface NotFoundStateProps {
  title?: string;
  subtitle?: string;
  className?: string;
}

export function NotFoundState({
  title = "Page not found",
  subtitle = "The route you opened does not exist in the Caesium console.",
  className,
}: NotFoundStateProps) {
  return (
    <section
      aria-label="Not found"
      data-testid="not-found-state"
      className={cn(
        "flex min-h-[50vh] flex-col items-center justify-center gap-4 px-5 py-16 text-center",
        className,
      )}
    >
      <div className="rounded-md border border-border/70 bg-muted/40 px-3 py-1.5 font-mono text-xs font-semibold tracking-[0.2em] text-text-3">
        404
      </div>
      <div className="space-y-1.5">
        <h1 className="text-lg font-semibold text-text-1">{title}</h1>
        <p className="max-w-sm text-[13px] text-text-3">{subtitle}</p>
      </div>
      <Button asChild variant="outline" size="sm">
        <Link to="/jobs">Back to jobs</Link>
      </Button>
    </section>
  );
}

export function ConsoleNotFound() {
  return <NotFoundState />;
}
