import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ChevronRight, Eye, Save } from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

/**
 * MemoryPage edits the curated memory document a project's agents receive.
 *
 * The bundle preview is the important control here. It shows what the model
 * will actually be given, which is the only reliable way to catch memory that
 * has silently degraded to empty — an outage looks exactly like "this project
 * has no notes yet" unless you can see the assembled bundle.
 */
export function MemoryPage() {
  const { projectId } = useParams();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState("");
  const [lesson, setLesson] = useState("");

  // Shares the Projects page's cache, so arriving from there costs nothing.
  const projects = useQuery({ queryKey: ["projects"], queryFn: () => api.projects() });
  const project = projects.data?.projects.find((p) => p.id === projectId);

  const memory = useQuery({
    queryKey: ["memory", projectId],
    queryFn: () => api.projectMemory(projectId!),
    enabled: !!projectId,
  });

  const bundle = useQuery({
    queryKey: ["bundle", projectId],
    queryFn: () => api.bundlePreview(projectId!),
    enabled: !!projectId,
  });

  useEffect(() => {
    if (memory.data) setDraft(memory.data.memory);
  }, [memory.data]);

  const save = useMutation({
    mutationFn: (content: string) => api.saveProjectMemory(projectId!, content),
    onSuccess: () => {
      toast.success("Project memory saved");
      void queryClient.invalidateQueries({ queryKey: ["memory", projectId] });
      void queryClient.invalidateQueries({ queryKey: ["bundle", projectId] });
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const addLesson = useMutation({
    mutationFn: (content: string) => api.appendLesson(projectId!, content),
    onSuccess: () => {
      toast.success("Lesson recorded");
      setLesson("");
      void queryClient.invalidateQueries({ queryKey: ["memory", projectId] });
    },
    onError: (err: Error) => toast.error(err.message),
  });

  if (memory.isLoading) {
    return (
      <div className="mx-auto max-w-4xl px-4 py-8 sm:px-6">
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  const dirty = !!memory.data && draft !== memory.data.memory;

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl space-y-4 px-4 py-8 sm:px-6">
        <header>
          <nav aria-label="Breadcrumb" className="mb-2 flex items-center gap-1 text-xs text-muted-foreground">
            <Link to="/projects" className="transition-colors hover:text-foreground">
              Projects
            </Link>
            <ChevronRight className="h-3 w-3" />
            <span className="truncate font-mono">{project?.display_name ?? "…"}</span>
          </nav>
          <h1 className="font-display text-xl font-semibold">Project memory</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            The curated notes every session in this project starts with. Treat what is here as
            shared with your team.
          </p>
        </header>

        {memory.data?.degraded && (
          <div className="flex items-start gap-2 rounded-md bg-warning/10 px-3 py-2 text-sm text-warning">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
            <span>
              Project memory is temporarily unreachable, so this page is showing nothing rather than
              what is stored. Do not save over it until it recovers.
            </span>
          </div>
        )}

        <Tabs defaultValue="memory">
          <TabsList>
            <TabsTrigger value="memory">memory.md</TabsTrigger>
            <TabsTrigger value="lessons">
              Lessons ({memory.data?.lessons?.length ?? 0})
            </TabsTrigger>
            <TabsTrigger value="bundle">
              <Eye className="mr-1.5 h-3.5 w-3.5" />
              What the model sees
            </TabsTrigger>
          </TabsList>

          <TabsContent value="memory" className="space-y-3">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              spellCheck={false}
              aria-label="Project memory document"
              className="h-[26rem] w-full resize-y rounded-lg border border-border bg-card p-3 font-mono text-xs leading-relaxed outline-none focus:border-primary/50"
            />
            <div className="flex items-center gap-3">
              <Button
                size="sm"
                disabled={!dirty || save.isPending || memory.data?.degraded}
                onClick={() => save.mutate(draft)}
              >
                <Save className="mr-1.5 h-3.5 w-3.5" />
                {save.isPending ? "Saving…" : "Save"}
              </Button>
              <span className="font-mono text-xs text-muted-foreground">
                ~{memory.data?.memory_tokens.toLocaleString() ?? 0} tokens
              </span>
              {dirty && <span className="text-xs text-warning">Unsaved changes</span>}
            </div>
          </TabsContent>

          <TabsContent value="lessons" className="space-y-3">
            <Card>
              <CardHeader className="pb-2">
                <CardTitle className="text-sm">Add a lesson</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2">
                <textarea
                  value={lesson}
                  onChange={(e) => setLesson(e.target.value)}
                  rows={3}
                  placeholder="Something future sessions in this project should know…"
                  aria-label="New lesson"
                  className="w-full resize-y rounded-md border border-border bg-background p-2 text-sm outline-none focus:border-primary/50"
                />
                <Button
                  size="sm"
                  disabled={!lesson.trim() || addLesson.isPending}
                  onClick={() => addLesson.mutate(lesson.trim())}
                >
                  Record lesson
                </Button>
              </CardContent>
            </Card>

            <ul className="space-y-2">
              {(memory.data?.lessons ?? [])
                .slice()
                .reverse()
                .map((entry, i) => (
                  <li key={i}>
                    <Card>
                      <CardContent className="space-y-1.5 py-3">
                        {entry.Timestamp && (
                          <div className="font-mono text-[11px] text-muted-foreground">
                            {entry.Timestamp}
                          </div>
                        )}
                        <Markdown content={entry.Content} />
                      </CardContent>
                    </Card>
                  </li>
                ))}
            </ul>
          </TabsContent>

          <TabsContent value="bundle" className="space-y-3">
            {bundle.data?.degraded && (
              <div className="flex items-start gap-2 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
                <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
                <span>
                  The bundle could not be assembled
                  {bundle.data.degraded_reason ? ` — ${bundle.data.degraded_reason}` : ""}. Chats in
                  this project are currently running with no memory at all.
                </span>
              </div>
            )}
            <div className="flex items-center gap-3 text-xs text-muted-foreground">
              <span className="font-mono">
                ~{bundle.data?.token_estimate.toLocaleString() ?? 0} tokens
              </span>
              {bundle.data?.cached && <span>served from cache</span>}
            </div>
            <pre className="max-h-[26rem] overflow-auto whitespace-pre-wrap rounded-lg border border-border bg-card p-3 font-mono text-xs leading-relaxed">
              {bundle.data?.bundle || "(empty)"}
            </pre>
          </TabsContent>
        </Tabs>
      </div>
    </div>
  );
}
