import { AtomLogo } from "@/components/brand/atom-logo";
import { Link } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface NotFoundStateProps {
  title?: string;
  subtitle?: string;
  className?: string;
}

export function NotFoundState({
  title = "~/404",
  subtitle = "Nothing is scheduled here.",
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
      <AtomLogo size={80} animated={false} />
      <div className="space-y-1.5">
        <h1 aria-label={title === "~/404" ? "Page not found" : undefined} className="text-2xl font-bold lowercase text-text-1">{title}</h1>
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
