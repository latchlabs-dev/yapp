import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { memoryProjects, memoryRules, memorySearch } from "@/lib/api";
import type { MemoryProjectSummary, MemoryRule } from "@/lib/types";
import { toast } from "sonner";
import {
  AlertTriangle,
  Archive,
  Eye,
  FileText,
  Pencil,
  RefreshCw,
  Search as SearchIcon,
} from "lucide-react";
import { timeAgo, formatTokens } from "@/lib/utils";
import { PageContainer, PageHeading } from "@/components/page";
import { Markdown } from "@/components/markdown";
import { useConfirm } from "@/components/confirm";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

const textareaClass =
  "w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm font-mono shadow-sm " +
  "placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring";


export function MemoryPage() {
  const qc = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);

  const projectsQuery = useQuery({
    queryKey: ["memory-projects"],
    queryFn: () => memoryProjects.list(),
  });
  const projects = useMemo(() => projectsQuery.data?.projects ?? [], [projectsQuery.data]);
  const projectNames = useMemo(() => projects.map((p) => p.name), [projects]);

  useEffect(() => {
    if (!selected && projects.length > 0) {
      const first = projects[0];
      if (first) setSelected(first.name);
    }
  }, [projects, selected]);

  const handleProjectArchived = (project: string) => {
    qc.setQueryData<{ projects: MemoryProjectSummary[] | null }>(
      ["memory-projects"],
      (current) => ({ projects: (current?.projects ?? []).filter((p) => p.name !== project) }),
    );
    setSelected((current) => (current === project ? null : current));
  };

  return (
    <PageContainer>
      <PageHeading
        title="Project Memory"
        description="Duffel-backed knowledge injected into agent sessions, now served from the embedded workspace. The curated document is maintained by the compactor; lessons are recorded by agents and the distiller. Edits here win over automation."
      />

      <MemorySearchCard onJump={(p) => p && setSelected(p)} />

      <div className="grid gap-6 lg:grid-cols-[260px_1fr]">
        <Card className="h-fit">
          <CardHeader>
            <CardTitle className="text-base">Projects</CardTitle>
            <CardDescription>{projects.length} known · click to inspect</CardDescription>
          </CardHeader>
          <CardContent className="space-y-1.5">
            {projectsQuery.isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
            {projectsQuery.error instanceof Error && (
              <p className="text-sm text-destructive">{projectsQuery.error.message}</p>
            )}
            {!projectsQuery.isLoading && projects.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No projects yet — they appear when a Pi session first fetches context.
              </p>
            )}
            {projects.map((p) => (
              <ProjectListItem
                key={p.name}
                summary={p}
                active={p.name === selected}
                onSelect={() => setSelected(p.name)}
              />
            ))}
          </CardContent>
        </Card>

        <div className="space-y-6">
          {selected && <ProjectDetail project={selected} onArchived={handleProjectArchived} />}
        </div>
      </div>

      <InjectionRules
        projects={projectNames}
        onChanged={() => qc.invalidateQueries({ queryKey: ["memory-rules"] })}
      />
    </PageContainer>
  );
}

function ProjectListItem({
  summary,
  active,
  onSelect,
}: {
  summary: MemoryProjectSummary;
  active: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={
        "w-full rounded-md border px-3 py-2 text-left transition-colors " +
        (active
          ? "border-primary/40 bg-secondary"
          : "border-transparent hover:border-border hover:bg-muted/50")
      }
    >
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-sm font-medium" title={summary.name}>
          {summary.name}
        </span>
        {!summary.has_memory && (
          <AlertTriangle className="size-3.5 shrink-0 text-warning" aria-label="no curated memory" />
        )}
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
        <span>{summary.lessons} lesson{summary.lessons === 1 ? "" : "s"}</span>
        <span>·</span>
        <span>{summary.has_memory ? `${formatTokens(summary.memory_tokens)} tok` : "empty"}</span>
        <span>·</span>
        <span>{timeAgo(summary.last_activity)}</span>
      </div>
    </button>
  );
}

function MemorySearchCard({ onJump }: { onJump: (project: string | null) => void }) {
  const [term, setTerm] = useState("");
  const [submitted, setSubmitted] = useState("");

  const searchQuery = useQuery({
    queryKey: ["memory-search", submitted],
    queryFn: () => memorySearch.query(submitted),
    enabled: submitted.trim().length > 0,
  });
  const results = searchQuery.data?.results ?? [];

  // The duffel path is projects/<name>/{memory.md,lessons.md}; pull the project
  // name out so a hit can deep-link the operator to the right project.
  const projectFromPath = (path: string): string | null => {
    const m = /^projects\/([^/]+)\//.exec(path);
    return m?.[1] ?? null;
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <SearchIcon className="size-4" /> Search memory
        </CardTitle>
        <CardDescription>
          Semantic search across all project memory via the embedded duffel index —
          find a lesson or fact without knowing which project recorded it.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setSubmitted(term);
          }}
        >
          <Input
            value={term}
            placeholder="e.g. serialization bottleneck, context injection…"
            onChange={(e) => setTerm(e.target.value)}
          />
          <Button type="submit" size="sm" disabled={!term.trim() || searchQuery.isFetching}>
            {searchQuery.isFetching ? "Searching…" : "Search"}
          </Button>
        </form>

        {searchQuery.error instanceof Error && (
          <p className="text-sm text-destructive">{searchQuery.error.message}</p>
        )}
        {submitted && !searchQuery.isFetching && results.length === 0 && !searchQuery.error && (
          <p className="text-sm text-muted-foreground">No matches for “{submitted}”.</p>
        )}
        {results.length > 0 && (
          <div className="space-y-2">
            {results.map((r) => {
              const proj = projectFromPath(r.path);
              return (
                <div key={r.path + r.title} className="rounded-md border border-border/60 p-3">
                  <div className="flex items-center justify-between gap-2">
                    <span className="truncate text-sm font-medium" title={r.title || r.path}>
                      {r.title || r.path}
                    </span>
                    <Badge variant="outline" className="shrink-0">
                      {r.score.toFixed(2)}
                    </Badge>
                  </div>
                  <p className="mt-1 line-clamp-3 text-xs text-muted-foreground">{r.snippet}</p>
                  <div className="mt-2 flex items-center gap-2">
                    <code className="text-[11px] text-muted-foreground">{r.path}</code>
                    {proj && (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-6 px-2 text-xs"
                        onClick={() => onJump(proj)}
                      >
                        Open {proj}
                      </Button>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function BundlePreviewCard({ project }: { project: string }) {
  const [hint, setHint] = useState("");
  const [activeHint, setActiveHint] = useState("");

  const bundleQuery = useQuery({
    queryKey: ["memory-bundle", project, activeHint],
    queryFn: () => memoryProjects.bundle(project, activeHint),
  });
  const bundle = bundleQuery.data;

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div>
            <CardTitle className="flex items-center gap-2 text-base">
              <Eye className="size-4" /> Injected bundle preview
            </CardTitle>
            <CardDescription>
              The exact context an agent receives for this project — curated memory,
              recent lessons, basic memory instructions, and (with a hint) semantic search.
              This is ground truth for “what the model actually sees”.
            </CardDescription>
          </div>
          <Button
            size="sm"
            variant="ghost"
            disabled={bundleQuery.isFetching}
            onClick={() => bundleQuery.refetch()}
            title="Rebuild bundle"
            aria-label="Rebuild bundle"
          >
            <RefreshCw className={"size-4 " + (bundleQuery.isFetching ? "animate-spin" : "")} />
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setActiveHint(hint);
          }}
        >
          <Input
            value={hint}
            placeholder="optional session hint (drives semantic search) — empty previews the cached base bundle"
            onChange={(e) => setHint(e.target.value)}
          />
          <Button type="submit" size="sm" variant="outline" disabled={bundleQuery.isFetching}>
            Apply hint
          </Button>
        </form>

        {bundleQuery.isLoading && <p className="text-sm text-muted-foreground">Assembling…</p>}
        {bundleQuery.error instanceof Error && (
          <p className="text-sm text-destructive">{bundleQuery.error.message}</p>
        )}

        {bundle && bundle.degraded && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" />
            <div>
              <p className="font-medium">Degraded — empty bundle injected</p>
              <p className="text-xs">
                Duffel was unreachable ({bundle.degraded_reason || "unavailable"}); agents in this
                state receive no project memory, silently. Check the embedded workspace.
              </p>
            </div>
          </div>
        )}

        {bundle && !bundle.degraded && (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant={bundle.token_estimate > 0 ? "info" : "warning"}>
                ~{bundle.token_estimate} tokens
              </Badge>
              {bundle.from_cache && <Badge variant="outline">from cache</Badge>}
              {bundle.hint && <Badge variant="outline">hint: {bundle.hint}</Badge>}
              {bundle.token_estimate === 0 && (
                <span className="text-xs text-muted-foreground">
                  Nothing injected (below the content noise floor or no memory yet).
                </span>
              )}
            </div>
            {bundle.markdown ? (
              <pre className="max-h-[28rem] overflow-auto rounded-md border border-border/60 bg-muted/30 p-3 font-mono text-xs whitespace-pre-wrap">
                {bundle.markdown}
              </pre>
            ) : (
              <p className="text-sm text-muted-foreground">Empty bundle — nothing is injected.</p>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function ProjectDetail({ project, onArchived }: { project: string; onArchived: (project: string) => void }) {
  const qc = useQueryClient();
  const confirm = useConfirm();
  const detail = useQuery({
    queryKey: ["memory-project", project],
    queryFn: () => memoryProjects.get(project),
  });

  const [memoryDraft, setMemoryDraft] = useState<string | null>(null);
  const [newLesson, setNewLesson] = useState("");
  const [mode, setMode] = useState<"view" | "edit">("view");

  useEffect(() => {
    setMemoryDraft(null);
    setNewLesson("");
    setMode("view");
  }, [project]);

  const memoryContent = memoryDraft ?? detail.data?.memory ?? "";
  const dirty = memoryDraft !== null && memoryDraft !== (detail.data?.memory ?? "");

  const invalidate = () => qc.invalidateQueries({ queryKey: ["memory-project", project] });

  const saveMut = useMutation({
    mutationFn: () => memoryProjects.saveMemory(project, memoryContent),
    onSuccess: () => {
      toast.success("Memory saved");
      setMemoryDraft(null);
      invalidate();
    },
    onError: (err: Error) => toast.error("Save failed", { description: err.message }),
  });

  const lessonMut = useMutation({
    mutationFn: () => memoryProjects.appendLesson(project, newLesson),
    onSuccess: () => {
      toast.success("Lesson recorded");
      setNewLesson("");
      invalidate();
    },
    onError: (err: Error) => toast.error("Failed", { description: err.message }),
  });

  const archiveMut = useMutation({
    mutationFn: () => memoryProjects.archiveLessons(project),
    onSuccess: () => {
      toast.success("Journal archived", { description: "Previous entries are recoverable under .archive/" });
      invalidate();
    },
    onError: (err: Error) => toast.error("Archive failed", { description: err.message }),
  });

  const archiveProjectMut = useMutation({
    mutationFn: () => memoryProjects.archive(project),
    onSuccess: (result) => {
      toast.success("Project archived", { description: `Recoverable at ${result.archived_path}` });
      qc.removeQueries({ queryKey: ["memory-project", project] });
      qc.removeQueries({ queryKey: ["memory-bundle", project] });
      onArchived(project);
      qc.invalidateQueries({ queryKey: ["memory-projects"] });
    },
    onError: (err: Error) => toast.error("Archive failed", { description: err.message }),
  });

  return (
    <>
      <Card>
        <CardHeader>
          <div className="flex items-start justify-between gap-2">
            <div>
              <CardTitle className="text-base">Curated memory — {project}</CardTitle>
              <CardDescription>
                Injected at the top of agent context bundles. The compactor folds
                lessons in here periodically; manual edits always win.
              </CardDescription>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <div className="flex rounded-md border border-border p-0.5">
                <Button
                  size="sm"
                  variant={mode === "view" ? "secondary" : "ghost"}
                  className="h-7 gap-1 px-2"
                  onClick={() => setMode("view")}
                >
                  <Eye className="size-3.5" /> View
                </Button>
                <Button
                  size="sm"
                  variant={mode === "edit" ? "secondary" : "ghost"}
                  className="h-7 gap-1 px-2"
                  onClick={() => setMode("edit")}
                >
                  <Pencil className="size-3.5" /> Edit
                </Button>
              </div>
              <Button
                size="sm"
                variant="outline"
                className="h-8 gap-1.5"
                disabled={archiveProjectMut.isPending}
                onClick={async () => {
                  if (
                    await confirm({
                      title: `Archive ${project}?`,
                      description: "All project memory moves out of the active list into Duffel's .archive directory. It remains recoverable, and may be recreated if a future agent session sees the repository again.",
                      confirmLabel: "Archive project",
                      destructive: true,
                    })
                  ) {
                    archiveProjectMut.mutate();
                  }
                }}
              >
                <Archive className="size-3.5" />
                {archiveProjectMut.isPending ? "Archiving…" : "Archive"}
              </Button>
            </div>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          {detail.isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {detail.error instanceof Error && (
            <p className="text-sm text-destructive">{detail.error.message}</p>
          )}
          {detail.data && mode === "view" && (
            <>
              {(detail.data.memory ?? "").trim() ? (
                <div className="max-h-[28rem] overflow-auto rounded-md border border-border/60 bg-muted/20 p-4">
                  <Markdown content={detail.data.memory} />
                </div>
              ) : (
                <div className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 p-3 text-sm text-warning">
                  <FileText className="size-4 shrink-0" />
                  No curated memory yet — bootstrap pending, or duffel was unreachable when this
                  project was created.
                </div>
              )}
            </>
          )}
          {detail.data && mode === "edit" && (
            <>
              <textarea
                className={textareaClass}
                rows={14}
                value={memoryContent}
                placeholder="# Project memory — empty (bootstrap pending or duffel unreachable)"
                onChange={(e) => setMemoryDraft(e.target.value)}
              />
              <div className="flex gap-2">
                <Button size="sm" disabled={!dirty || saveMut.isPending} onClick={() => saveMut.mutate()}>
                  {saveMut.isPending ? "Saving…" : "Save memory"}
                </Button>
                {dirty && (
                  <Button size="sm" variant="ghost" onClick={() => setMemoryDraft(null)}>
                    Discard changes
                  </Button>
                )}
              </div>
            </>
          )}
        </CardContent>
      </Card>

      <BundlePreviewCard project={project} />

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Lessons journal</CardTitle>
          <CardDescription>
            Append-only entries from agents (record_observation), the distiller,
            and admins. The compactor consumes and archives them over time.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {detail.data && (detail.data.lessons ?? []).length === 0 && (
            <p className="text-sm text-muted-foreground">No lessons recorded yet.</p>
          )}
          {detail.data && (detail.data.lessons ?? []).length > 0 && (
            <div className="max-h-96 space-y-3 overflow-y-auto pr-1">
              {[...(detail.data.lessons ?? [])].reverse().map((entry, i) => (
                <div key={`${entry.timestamp}-${i}`} className="rounded-md border border-border/60 p-3">
                  <p className="mb-1 text-xs text-muted-foreground">{entry.timestamp || "(no timestamp)"}</p>
                  <Markdown content={entry.content} className="text-xs" />
                </div>
              ))}
            </div>
          )}

          <Separator />

          <div className="space-y-2">
            <label className="text-sm font-medium">Record a lesson</label>
            <textarea
              className={textareaClass}
              rows={3}
              value={newLesson}
              placeholder="**[lesson]** Short, durable, project-specific knowledge…"
              onChange={(e) => setNewLesson(e.target.value)}
            />
            <div className="flex justify-between">
              <Button size="sm" disabled={!newLesson.trim() || lessonMut.isPending} onClick={() => lessonMut.mutate()}>
                Append lesson
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={archiveMut.isPending || !detail.data || (detail.data.lessons ?? []).length === 0}
                onClick={async () => {
                  if (
                    await confirm({
                      title: "Archive lessons journal?",
                      description: `Entries for "${project}" move to .archive/ and a fresh journal is started.`,
                      confirmLabel: "Archive journal",
                      destructive: true,
                    })
                  ) {
                    archiveMut.mutate();
                  }
                }}
              >
                Archive journal
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>
    </>
  );
}

function InjectionRules({ projects, onChanged }: { projects: string[]; onChanged: () => void }) {
  const qc = useQueryClient();
  const rulesQuery = useQuery({
    queryKey: ["memory-rules"],
    queryFn: () => memoryRules.list(),
  });
  const rules = rulesQuery.data?.memory_rules ?? [];

  const [project, setProject] = useState("");
  const [tenantId, setTenantId] = useState("");
  const [userId, setUserId] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [budget, setBudget] = useState("");
  const [note, setNote] = useState("");

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["memory-rules"] });
    onChanged();
  };

  const upsertMut = useMutation({
    mutationFn: (rule: Partial<MemoryRule> & { enabled: boolean }) => memoryRules.upsert(rule),
    onSuccess: () => {
      toast.success("Rule saved");
      setProject(""); setTenantId(""); setUserId(""); setBudget(""); setNote("");
      invalidate();
    },
    onError: (err: Error) => toast.error("Failed", { description: err.message }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => memoryRules.delete(id),
    onSuccess: () => { toast.success("Rule deleted"); invalidate(); },
    onError: (err: Error) => toast.error("Failed", { description: err.message }),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Context injection rules</CardTitle>
        <CardDescription>
          Control when project memory is injected. The most specific matching
          rule wins (project &gt; tenant &gt; user); empty fields match anything.
          With no rules, injection is on for everyone.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {rules.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Project</TableHead>
                <TableHead>Tenant</TableHead>
                <TableHead>User</TableHead>
                <TableHead>Injection</TableHead>
                <TableHead className="text-right">Budget (tokens)</TableHead>
                <TableHead>Note</TableHead>
                <TableHead></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.map((r) => (
                <TableRow key={r.id}>
                  <TableCell>{r.project || <span className="text-muted-foreground">any</span>}</TableCell>
                  <TableCell className="max-w-[140px] truncate" title={r.tenant_id}>
                    {r.tenant_id || <span className="text-muted-foreground">any</span>}
                  </TableCell>
                  <TableCell className="max-w-[140px] truncate" title={r.user_id}>
                    {r.user_id || <span className="text-muted-foreground">any</span>}
                  </TableCell>
                  <TableCell>
                    <Badge variant={r.enabled ? "success" : "destructive"}>
                      {r.enabled ? "enabled" : "disabled"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">{r.budget_tokens ?? "—"}</TableCell>
                  <TableCell className="max-w-[200px] truncate" title={r.note}>{r.note || "—"}</TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => upsertMut.mutate({ ...r, enabled: !r.enabled })}
                      >
                        {r.enabled ? "Disable" : "Enable"}
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => deleteMut.mutate(r.id)}>
                        Delete
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {rules.length === 0 && !rulesQuery.isLoading && (
          <p className="text-sm text-muted-foreground">
            No rules — context injection is enabled globally.
          </p>
        )}

        <Separator />

        <div className="grid gap-2 md:grid-cols-6">
          <div className="space-y-1">
            <label className="text-xs font-medium">Project</label>
            <Input list="memory-rule-projects" value={project} placeholder="any" onChange={(e) => setProject(e.target.value)} />
            <datalist id="memory-rule-projects">
              {projects.map((p) => <option key={p} value={p} />)}
            </datalist>
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">Tenant ID</label>
            <Input value={tenantId} placeholder="any" onChange={(e) => setTenantId(e.target.value)} />
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">User ID</label>
            <Input value={userId} placeholder="any" onChange={(e) => setUserId(e.target.value)} />
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">Injection</label>
            <Button
              type="button"
              variant={enabled ? "default" : "destructive"}
              className="w-full"
              size="sm"
              onClick={() => setEnabled(!enabled)}
            >
              {enabled ? "enabled" : "disabled"}
            </Button>
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">Budget tokens</label>
            <Input value={budget} placeholder="default" inputMode="numeric" onChange={(e) => setBudget(e.target.value)} />
          </div>
          <div className="space-y-1">
            <label className="text-xs font-medium">Note</label>
            <Input value={note} placeholder="why this rule exists" onChange={(e) => setNote(e.target.value)} />
          </div>
        </div>
        <Button
          size="sm"
          disabled={upsertMut.isPending}
          onClick={() => {
            const budgetNum = budget.trim() === "" ? undefined : Number(budget);
            if (budgetNum !== undefined && (!Number.isFinite(budgetNum) || budgetNum <= 0)) {
              toast.error("Budget must be a positive number");
              return;
            }
            upsertMut.mutate({
              project: project.trim() || undefined,
              tenant_id: tenantId.trim() || undefined,
              user_id: userId.trim() || undefined,
              enabled,
              budget_tokens: budgetNum,
              note: note.trim() || undefined,
            });
          }}
        >
          Add rule
        </Button>
      </CardContent>
    </Card>
  );
}
