import { Link } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { InsufficientAccess } from "./InsufficientAccess";

/**
 * Shown in place of a job or run page when the server denies the read (403): a
 * deep link to a job, or to a run whose persisted namespace is outside the
 * principal's grants. The text is fixed on purpose. The 403 body can name the
 * namespaces the caller lacks, which a page must not echo.
 */
export const jobAccessDeniedMessage = "You do not have access to this job's namespace.";

export function JobAccessDenied() {
  return (
    <section
      aria-label="Access denied"
      data-testid="job-insufficient-access"
      className="flex min-h-[40vh] flex-col items-center justify-center gap-4 px-5 py-16 text-center"
    >
      <InsufficientAccess message={jobAccessDeniedMessage} />
      <Button asChild variant="outline" size="sm">
        <Link to="/jobs">Back to jobs</Link>
      </Button>
    </section>
  );
}
