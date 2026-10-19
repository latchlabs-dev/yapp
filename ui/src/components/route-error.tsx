import { useRouteError, isRouteErrorResponse, Link } from "react-router-dom";
import { AlertTriangle, RotateCw } from "lucide-react";
import { Button } from "@/components/ui/button";

// Route-level error boundary. React Router renders this in place of a route's
// element when it throws during render/loading, so a single page's failure
// degrades to this panel inside the shell instead of white-screening the whole
// console. Attached to every child route in router.tsx.
export function RouteError() {
  const error = useRouteError();

  let title = "Something went wrong on this page";
  let detail: string | null = null;
  if (isRouteErrorResponse(error)) {
    title = `${error.status} ${error.statusText}`;
    detail = typeof error.data === "string" ? error.data : null;
  } else if (error instanceof Error) {
    detail = error.message;
  } else if (typeof error === "string") {
    detail = error;
  }

  return (
    <div className="flex min-h-[60vh] items-center justify-center px-4">
      <div className="w-full max-w-md overflow-hidden rounded-xl border border-border bg-card shadow-sm shadow-black/20">
        <div className="flex items-center gap-2 border-b border-border bg-secondary/40 px-5 py-2.5">
          <span className="led !bg-destructive" style={{ animation: "none" }} />
          <span className="eyebrow text-destructive">Page Fault</span>
        </div>
        <div className="flex flex-col items-center gap-4 px-6 py-8 text-center">
          <span className="flex h-11 w-11 items-center justify-center rounded-lg bg-destructive/12 ring-1 ring-inset ring-destructive/30">
            <AlertTriangle className="h-5 w-5 text-destructive" />
          </span>
          <div className="space-y-1.5">
            <h2 className="font-display text-lg font-semibold tracking-tight">
              {title}
            </h2>
            {detail && (
              <p className="break-words font-mono text-xs text-muted-foreground">
                {detail}
              </p>
            )}
            <p className="pt-1 text-sm text-muted-foreground">
              The rest of the console is still running. Reload this view or head
              back to the overview.
            </p>
          </div>
          <div className="flex items-center gap-2">
            <Button onClick={() => window.location.reload()}>
              <RotateCw className="h-4 w-4" />
              Reload
            </Button>
            <Button variant="outline" asChild>
              <Link to="/">Back to overview</Link>
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}
