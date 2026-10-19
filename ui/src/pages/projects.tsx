import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  ArrowLeft,
  Brain,
  Clock3,
  DollarSign,
  ExternalLink,
  FileText,
  FolderGit2,
  Lightbulb,
  MessageSquareText,
  Search,
  Sparkles,
} from "lucide-react";
import { projects } from "@/lib/api";
import type {
  ProjectKnowledgeEntry,
  ProjectParticipant,
  RepositoryProjectSummary,
} from "@/lib/types";
import { formatDate, formatDetailCost, formatTokens, timeAgo } from "@/lib/utils";
import { PageContainer, PageHeading } from "@/components/page";
import { Markdown } from "@/components/markdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Progress } from "@/components/ui/progress";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

function repositoryName(project: Pick<RepositoryProjectSummary, "canonical_repo_key">) {
  const parts = project.canonical_repo_key.split("/");
  return parts.at(-1) || project.canonical_repo_key;
}

function repositoryURL(repoKey: string) {
  return `https://${repoKey}`;
}

function participantName(participant: Pick<ProjectParticipant, "display_name" | "email">) {
  return participant.display_name || participant.email;
}

function shortID(value: string) {
  return value.length > 18 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value;
}

function coveragePercent(attributed: number, total: number) {
	return total > 0 ? (attributed / total) * 100 : 0;
}

function MetricCard({
  label,
  value,
  detail,
  icon: Icon,
}: {
  label: string;
  value: string;
  detail?: string;
  icon: React.ComponentType<{ className?: string }>;
}) {
  return (
    <Card>
      <CardContent className="flex items-start justify-between gap-3 p-4">
        <div className="space-y-1">
          <p className="eyebrow">{label}</p>
          <p className="font-mono text-2xl font-semibold tracking-tight">{value}</p>
          {detail && <p className="text-xs text-muted-foreground">{detail}</p>}
        </div>
        <Icon className="mt-1 size-4 text-muted-foreground/60" />
      </CardContent>
    </Card>
  );
}

export function ProjectsPage() {
  const { projectId } = useParams();
  return projectId ? <ProjectDetailPage projectId={projectId} /> : <ProjectsListPage />;
}

function ProjectsListPage() {
  const [search, setSearch] = useState("");
  const query = useQuery({ queryKey: ["projects"], queryFn: () => projects.list() });
  const rows = useMemo(() => query.data?.projects ?? [], [query.data]);
	const attribution = query.data?.attribution_coverage;
  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (!needle) return rows;
    return rows.filter((project) =>
      [project.canonical_repo_key, project.slug, project.display_name, project.tenant_name]
        .some((value) => value.toLowerCase().includes(needle)),
    );
  }, [rows, search]);

  const totals = useMemo(() => rows.reduce(
    (sum, project) => ({
      sessions: sum.sessions + project.metrics.session_count,
      insights: sum.insights + project.knowledge.insight_count,
      cost: sum.cost + project.metrics.total_cost_cents,
		unavailable: sum.unavailable + (project.knowledge.available ? 0 : 1),
		requests: sum.requests + project.metrics.request_count,
    }),
	    { sessions: 0, insights: 0, cost: 0, unavailable: 0, requests: 0 },
  ), [rows]);

  return (
    <PageContainer>
      <PageHeading
        title="Projects"
        description="Repository-level memory, generated insight, and agent activity. Normalized Git remotes are the project identity, so different checkout folders converge on the same project."
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
        <MetricCard label="Repositories" value={rows.length.toLocaleString()} detail="Git-backed projects" icon={FolderGit2} />
        <MetricCard label="Sessions" value={totals.sessions.toLocaleString()} detail="Uploaded agent sessions" icon={Activity} />
        <MetricCard
          label="Insights"
          value={`${totals.insights.toLocaleString()}${totals.unavailable > 0 ? "+" : ""}`}
          detail={totals.unavailable > 0 ? `Known total; ${totals.unavailable} knowledge stores unavailable` : "Durable generated observations"}
          icon={Lightbulb}
        />
        <MetricCard label="Recorded cost" value={formatDetailCost(totals.cost)} detail="From project sessions" icon={DollarSign} />
		<MetricCard label="Model requests" value={totals.requests.toLocaleString()} detail="Server-observed and project-attributed" icon={Activity} />
      </div>

	  {attribution && (
		<Card className="border-amber-500/30 bg-amber-500/5">
		  <CardHeader className="pb-3">
			<CardTitle className="text-base">Attribution coverage</CardTitle>
			<CardDescription>Coverage is shown explicitly because canonical projects do not yet represent all historical activity.</CardDescription>
		  </CardHeader>
		  <CardContent className="grid gap-5 lg:grid-cols-2">
			<div className="space-y-2">
			  <div className="flex items-center justify-between gap-3 text-sm">
				<span>Session rows mapped to canonical repositories</span>
				<span className="font-mono">{attribution.canonical_project_sessions.toLocaleString()} / {attribution.total_sessions.toLocaleString()} ({coveragePercent(attribution.canonical_project_sessions, attribution.total_sessions).toFixed(1)}%)</span>
			  </div>
			  <Progress value={coveragePercent(attribution.canonical_project_sessions, attribution.total_sessions)} />
			  <p className="text-xs text-muted-foreground">{attribution.legacy_project_sessions.toLocaleString()} legacy project rows · {attribution.unattributed_sessions.toLocaleString()} unresolved rows</p>
			</div>
			<div className="space-y-2">
			  <div className="flex items-center justify-between gap-3 text-sm">
				<span>Model requests carrying a canonical project ID</span>
				<span className="font-mono">{attribution.project_attributed_requests.toLocaleString()} / {attribution.total_model_requests.toLocaleString()} ({coveragePercent(attribution.project_attributed_requests, attribution.total_model_requests).toFixed(1)}%)</span>
			  </div>
			  <Progress value={coveragePercent(attribution.project_attributed_requests, attribution.total_model_requests)} />
			  <p className="text-xs text-muted-foreground">New extension traffic is attributed from accepted proxy requests; older request rows remain historical gaps.</p>
			</div>
		  </CardContent>
		</Card>
	  )}

      <Card>
        <CardHeader className="gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <CardTitle>Git repositories</CardTitle>
            <CardDescription>
              {filtered.length === rows.length ? `${rows.length} projects` : `${filtered.length} of ${rows.length} projects`}
            </CardDescription>
          </div>
          <div className="relative w-full sm:w-80">
            <Search className="absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Search repository or tenant…"
              aria-label="Search projects"
              className="pl-9"
            />
          </div>
        </CardHeader>
        <CardContent>
          {query.isLoading && <p className="py-8 text-center text-sm text-muted-foreground">Loading projects…</p>}
          {query.error instanceof Error && <p className="text-sm text-destructive">{query.error.message}</p>}
          {!query.isLoading && rows.length === 0 && (
            <div className="py-12 text-center">
              <FolderGit2 className="mx-auto mb-3 size-10 text-muted-foreground/40" />
              <p className="font-medium">No repository-backed projects yet</p>
              <p className="mt-1 text-sm text-muted-foreground">
                A project appears after an authenticated Pi session reports a valid Git remote.
              </p>
            </div>
          )}
          {rows.length > 0 && filtered.length === 0 && (
            <p className="py-8 text-center text-sm text-muted-foreground">No projects match “{search}”.</p>
          )}
          {filtered.length > 0 && (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Repository</TableHead>
                    <TableHead>Tenant</TableHead>
                    <TableHead>People</TableHead>
                    <TableHead>Knowledge</TableHead>
                    <TableHead className="text-right">Sessions</TableHead>
					<TableHead className="text-right">Requests</TableHead>
                    <TableHead className="text-right">Messages</TableHead>
                    <TableHead>Last activity</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.map((project) => (
                    <TableRow key={project.id} className="hover:bg-muted/40">
                      <TableCell>
                        <Link to={`/projects/${encodeURIComponent(project.id)}`} className="group block">
                          <span className="font-medium text-foreground group-hover:text-primary">{repositoryName(project)}</span>
                          <code className="mt-0.5 block max-w-[320px] truncate text-[11px] text-muted-foreground">
                            {project.canonical_repo_key}
                          </code>
                        </Link>
                      </TableCell>
                      <TableCell>
                        <span className="text-sm">{project.tenant_name}</span>
                        <span className="block text-[11px] text-muted-foreground">{project.slug}</span>
                      </TableCell>
                      <TableCell>
                        {project.participants.length > 0 ? (
                          <span className="block max-w-44 text-xs text-muted-foreground" title={project.participants.map((participant) => participant.name).join(", ")}>
                            {project.participants.slice(0, 2).map((participant) => participant.name).join(", ")}
                            {project.participants.length > 2 ? ` +${project.participants.length - 2}` : ""}
                          </span>
                        ) : <span className="text-xs text-muted-foreground">No identified users</span>}
                      </TableCell>
                      <TableCell>
                        {!project.knowledge.available ? (
                          <Badge variant="destructive">Unavailable</Badge>
                        ) : (
                          <div className="flex flex-wrap gap-1.5">
                            <Badge variant={project.knowledge.has_memory ? "info" : "outline"}>
                              {project.knowledge.has_memory ? `${formatTokens(project.knowledge.memory_tokens)} memory tok` : "No curated memory"}
                            </Badge>
                            <Badge variant={project.knowledge.insight_count > 0 ? "success" : "outline"}>
                              {project.knowledge.insight_count} insights
                            </Badge>
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-right font-mono">{project.metrics.session_count.toLocaleString()}</TableCell>
					  <TableCell className="text-right font-mono">{project.metrics.request_count.toLocaleString()}</TableCell>
                      <TableCell className="text-right font-mono">{project.metrics.message_count.toLocaleString()}</TableCell>
					  <TableCell className="whitespace-nowrap text-sm text-muted-foreground" title={`${formatDate(project.metrics.last_activity_at)} · ${project.metrics.last_activity_source === "proxy_request" ? "server-observed model request" : "client session submission"}`}>
                        {timeAgo(project.metrics.last_activity_at)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}

function ProjectDetailPage({ projectId }: { projectId: string }) {
  const query = useQuery({
    queryKey: ["projects", projectId],
    queryFn: () => projects.get(projectId),
  });

  if (query.isLoading) {
    return <PageContainer><p className="py-12 text-sm text-muted-foreground">Loading project…</p></PageContainer>;
  }
  if (query.error instanceof Error || !query.data) {
    return (
      <PageContainer>
        <Button asChild variant="ghost" size="sm"><Link to="/projects"><ArrowLeft className="mr-1.5 size-4" />Projects</Link></Button>
        <p className="text-sm text-destructive">{query.error instanceof Error ? query.error.message : "Project not found"}</p>
      </PageContainer>
    );
  }

  const project = query.data;
  const completed = project.metrics.distilled_sessions + project.metrics.skipped_sessions;
  const coverage = project.metrics.session_count > 0 ? (completed / project.metrics.session_count) * 100 : 0;

  return (
    <PageContainer>
      <div className="space-y-3">
        <Link to="/projects" className="inline-flex items-center text-sm text-muted-foreground hover:text-foreground">
          <ArrowLeft className="mr-1.5 size-4" /> Back to projects
        </Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-2xl font-bold tracking-tight">{repositoryName(project)}</h1>
              <Badge variant="outline">{project.tenant_name}</Badge>
            </div>
            <a
              href={repositoryURL(project.canonical_repo_key)}
              target="_blank"
              rel="noreferrer"
              className="mt-1 inline-flex items-center gap-1 font-mono text-xs text-primary hover:underline"
            >
              {project.canonical_repo_key}<ExternalLink className="size-3" />
            </a>
            <p className="mt-1 text-xs text-muted-foreground">Stable ID {project.id} · observed as {project.slug}</p>
          </div>
          <Button asChild variant="outline" size="sm"><Link to="/memory"><Brain className="mr-1.5 size-4" />Memory controls</Link></Button>
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
		<MetricCard label="Sessions" value={project.metrics.session_count.toLocaleString()} detail={`${project.metrics.request_count.toLocaleString()} server-observed requests`} icon={Activity} />
        <MetricCard label="Tokens" value={formatTokens(project.metrics.total_tokens)} detail="Session-reported total" icon={MessageSquareText} />
        <MetricCard label="Cost" value={formatDetailCost(project.metrics.total_cost_cents)} detail="Session-reported total" icon={DollarSign} />
        <MetricCard label="Memory" value={!project.knowledge.available ? "—" : project.knowledge.has_memory ? formatTokens(project.knowledge.memory_tokens) : "0"} detail={!project.knowledge.available ? "knowledge store unavailable" : project.knowledge.has_memory ? "curated tokens" : "not generated yet"} icon={FileText} />
        <MetricCard label="Insights" value={project.knowledge.available ? project.knowledge.insight_count.toLocaleString() : "—"} detail={project.knowledge.available ? "agent and distiller observations" : "knowledge store unavailable"} icon={Lightbulb} />
        <MetricCard label="Prompt feedback" value={project.knowledge.available ? project.knowledge.prompt_reflection_count.toLocaleString() : "—"} detail={project.knowledge.available ? "generated reflections" : "knowledge store unavailable"} icon={Sparkles} />
      </div>

      <Card>
        <CardHeader className="pb-3">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div>
              <CardTitle className="text-base">Distillation coverage</CardTitle>
              <CardDescription>How many uploaded sessions have completed the insight-generation pass.</CardDescription>
            </div>
            <Badge variant={project.metrics.failed_sessions > 0 ? "destructive" : coverage === 100 ? "success" : "outline"}>
              {coverage.toFixed(0)}% complete
            </Badge>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          <Progress value={coverage} indicatorClassName={project.metrics.failed_sessions > 0 ? "bg-destructive" : "bg-success"} />
          <div className="flex flex-wrap gap-x-5 gap-y-1 text-xs text-muted-foreground">
            <span>{project.metrics.distilled_sessions} distilled</span>
            <span>{project.metrics.skipped_sessions} skipped</span>
            <span>{project.metrics.pending_sessions} pending</span>
            <span className={project.metrics.failed_sessions > 0 ? "text-destructive" : ""}>{project.metrics.failed_sessions} retrying/failed</span>
			<span className="ml-auto inline-flex items-center gap-1" title={project.metrics.last_activity_source === "proxy_request" ? "Server-observed model request" : "Client session submission"}><Clock3 className="size-3" />{timeAgo(project.metrics.last_activity_at)}</span>
          </div>
          {project.metrics.failed_sessions > 0 && (
            <div className="flex gap-2 rounded-md border border-destructive/30 bg-destructive/10 p-3 text-xs text-destructive">
              <AlertTriangle className="size-4 shrink-0" />
              Some sessions have a distillation error and have not produced a completed result yet.
            </div>
          )}
        </CardContent>
      </Card>

      <ProjectParticipantsCard
        participants={project.participants}
        unattributedSessions={project.metrics.unattributed_sessions}
      />

      {!project.knowledge.available ? (
        <Card className="border-destructive/30 bg-destructive/5">
          <CardContent className="flex gap-3 p-4 text-sm text-destructive">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" />
            Project knowledge is temporarily unavailable. Memory and insight counts are intentionally hidden rather than shown as zero.
          </CardContent>
        </Card>
      ) : <Tabs defaultValue="memory">
        <TabsList className="h-auto flex-wrap justify-start">
          <TabsTrigger value="memory"><FileText className="mr-1.5 size-4" />Memory</TabsTrigger>
          <TabsTrigger value="insights"><Lightbulb className="mr-1.5 size-4" />Insights ({project.insights.length})</TabsTrigger>
          <TabsTrigger value="prompts"><Sparkles className="mr-1.5 size-4" />Prompt feedback ({project.prompt_reflections.length})</TabsTrigger>
          <TabsTrigger value="identity"><FolderGit2 className="mr-1.5 size-4" />Repository identity</TabsTrigger>
        </TabsList>

        <TabsContent value="memory">
          <KnowledgeCard title="Curated project memory" description="The compacted knowledge injected into new agent context." empty="No curated memory has been generated for this repository yet.">
            {project.knowledge.has_memory && project.memory && <Markdown content={project.memory} />}
          </KnowledgeCard>
        </TabsContent>
        <TabsContent value="insights">
          <KnowledgeEntriesCard
            title="Generated insights"
            description="Durable decisions, lessons, mistakes, and context recorded by agents or extracted by the distiller."
            entries={project.insights}
            empty="No durable insights have been generated for this repository yet."
          />
        </TabsContent>
        <TabsContent value="prompts">
          <KnowledgeEntriesCard
            title="Prompt feedback"
            description="Generated reflections on request clarity and concrete ways to reduce rework."
            entries={project.prompt_reflections}
            empty="No prompt feedback has been generated for this repository yet."
          />
        </TabsContent>
        <TabsContent value="identity">
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Repository identity</CardTitle>
              <CardDescription>All observed remote forms below resolve to this single project within the tenant boundary.</CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              <div>
                <p className="eyebrow mb-1">Canonical key</p>
                <code className="text-sm">{project.canonical_repo_key}</code>
              </div>
              <div>
                <p className="eyebrow mb-1">Normalized remotes</p>
                <div className="space-y-1">
                  {project.remotes.map((remote) => <code key={remote} className="block text-xs text-muted-foreground">{remote}</code>)}
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                SSH and HTTPS credentials, query strings, fragments, letter casing, and a trailing .git do not create duplicate projects.
              </p>
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>}
    </PageContainer>
  );
}

function ProjectParticipantsCard({
  participants,
  unattributedSessions,
}: {
  participants: ProjectParticipant[];
  unattributedSessions: number;
}) {
  return (
    <Card>
      <CardHeader className="gap-2 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <CardTitle className="text-base">Project participants</CardTitle>
          <CardDescription>Users with repository-linked session activity for this tenant.</CardDescription>
        </div>
        <Badge variant="outline">{participants.length} identified</Badge>
      </CardHeader>
      <CardContent>
        {participants.length === 0 ? (
          <p className="py-5 text-center text-sm text-muted-foreground">No sessions on this project are tied to a user yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>User</TableHead>
                  <TableHead className="text-right">Sessions</TableHead>
                  <TableHead className="text-right">Messages</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                  <TableHead>Last activity</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {participants.map((participant) => (
                  <TableRow key={participant.user_id}>
                    <TableCell>
                      <span className="font-medium">{participantName(participant)}</span>
                      {participant.display_name && participant.display_name !== participant.email && (
                        <span className="block text-xs text-muted-foreground">{participant.email}</span>
                      )}
                    </TableCell>
                    <TableCell className="text-right font-mono">{participant.session_count.toLocaleString()}</TableCell>
                    <TableCell className="text-right font-mono">{participant.message_count.toLocaleString()}</TableCell>
                    <TableCell className="text-right font-mono">{formatDetailCost(participant.total_cost_cents)}</TableCell>
                    <TableCell className="whitespace-nowrap text-sm text-muted-foreground" title={formatDate(participant.last_activity_at)}>
                      {timeAgo(participant.last_activity_at)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        {unattributedSessions > 0 && (
          <p className="mt-3 text-xs text-muted-foreground">
            {unattributedSessions.toLocaleString()} additional session{unattributedSessions === 1 ? " is" : "s are"} not tied to a user.
          </p>
        )}
      </CardContent>
    </Card>
  );
}

function KnowledgeCard({
  title,
  description,
  empty,
  children,
}: {
  title: string;
  description: string;
  empty: string;
  children?: React.ReactNode;
}) {
  return (
    <Card>
      <CardHeader><CardTitle className="text-base">{title}</CardTitle><CardDescription>{description}</CardDescription></CardHeader>
      <CardContent>
        {children || <p className="py-8 text-center text-sm text-muted-foreground">{empty}</p>}
      </CardContent>
    </Card>
  );
}

function KnowledgeEntriesCard({
  title,
  description,
  entries,
  empty,
}: {
  title: string;
  description: string;
  entries: ProjectKnowledgeEntry[];
  empty: string;
}) {
  return (
    <KnowledgeCard title={title} description={description} empty={empty}>
      {entries.length > 0 ? (
        <div className="space-y-3">
          {[...entries].reverse().map((entry, index) => (
            <div key={`${entry.timestamp}-${index}`} className="rounded-lg border border-border/60 bg-muted/15 p-4">
              <div className="mb-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <span>{entry.timestamp || "Timestamp unavailable"}</span>
                {entry.attribution?.source && <Badge variant="outline">{entry.attribution.source}</Badge>}
                {entry.attribution?.user && (
                  <span title={entry.attribution.user.email}>
                    {entry.attribution.user.display_name || entry.attribution.user.email}
                  </span>
                )}
                {entry.attribution && !entry.attribution.user && (
                  <span>User unavailable</span>
                )}
                {entry.attribution?.conversation_id && (
                  <code title={entry.attribution.conversation_id}>session {shortID(entry.attribution.conversation_id)}</code>
                )}
              </div>
              <Markdown content={entry.content} />
            </div>
          ))}
        </div>
      ) : undefined}
    </KnowledgeCard>
  );
}
