import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FolderGit2 } from "lucide-react";
import { api } from "@/lib/api";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

/**
 * ProjectsPage lists the repository identities the tenant owns. These are the
 * same canonical projects the Pi CLI resolves against, so memory written here
 * is the memory CLI sessions for that repository will read.
 */
export function ProjectsPage() {
  const navigate = useNavigate();
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.projects() });

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl px-4 py-8 sm:px-6">
        <header className="mb-6">
          <h1 className="font-display text-xl font-semibold">Projects</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Repositories your team's Pi sessions have worked in. Each carries the shared memory a
            chat can draw on.
          </p>
        </header>

        {projects.isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-20 w-full" />
            <Skeleton className="h-20 w-full" />
          </div>
        ) : (projects.data?.projects ?? []).length === 0 ? (
          <Card>
            <CardContent className="py-10 text-center text-sm text-muted-foreground">
              No projects yet. A project appears the first time a Pi session runs in a Git
              repository your tenant owns.
            </CardContent>
          </Card>
        ) : (
          // One card with rows reads as a list; a stack of cards reads as
          // unrelated things that happen to be near each other.
          <Card className="overflow-hidden">
            <ul className="divide-y divide-border/70">
              {projects.data!.projects.map((project) => (
                <li key={project.id}>
                  <button
                    onClick={() => navigate(`/projects/${project.id}/memory`)}
                    className="group flex w-full items-center gap-3 px-4 py-3.5 text-left transition-colors hover:bg-secondary/40"
                  >
                    <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-secondary/70 ring-1 ring-inset ring-border">
                      <FolderGit2 className="h-4 w-4 text-muted-foreground group-hover:text-primary" />
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium">{project.display_name}</span>
                      <span className="block truncate font-mono text-xs text-muted-foreground">
                        {project.repo || project.slug}
                      </span>
                    </span>
                    <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground/60 transition-transform group-hover:translate-x-0.5 group-hover:text-foreground" />
                  </button>
                </li>
              ))}
            </ul>
          </Card>
        )}
      </div>
    </div>
  );
}
