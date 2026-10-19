import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDownToLine, ArrowLeft, ArrowUpToLine, Brain, ChevronRight, Loader2, MessageSquare, RefreshCw } from "lucide-react";
import { conversationPayloads, sessionArchive } from "@/lib/api";
import type { SessionDisplayItem, SessionTranscriptTurn } from "@/lib/types";
import { PageContainer, PageHeading } from "@/components/page";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { formatDateFull, formatDetailCost, timeAgo } from "@/lib/utils";
import {
  DEFAULT_TRANSCRIPT_FILTER,
  countHidden,
  formatBytes,
  isVisibleItem,
  mergeTranscriptPages,
  sessionTitle,
  toolCallSummary,
  turnNotice,
  type TranscriptFilter,
} from "@/lib/transcript";

const TRANSCRIPT_PAGE_TURNS = 50;
const REBUILD_POLL_MS = 3000;

// SessionDetailPage renders one session from the session archive. The header,
// stats and turn list come from Postgres in a single request; transcript pages
// load on demand; raw request/response bodies load only when a turn's Raw
// section is opened. Nothing re-downloads the conversation per request.
export function SessionDetailPage({ conversationId, initialTenantId }: { conversationId: string; initialTenantId?: string }) {
  return <SessionDetailView key={`${conversationId}:${initialTenantId ?? ""}`} conversationId={conversationId} initialTenantId={initialTenantId} />;
}

function SessionDetailView({ conversationId, initialTenantId }: { conversationId: string; initialTenantId?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [tenantId, setTenantId] = useState<string | undefined>(initialTenantId);
  const [filter, setFilter] = useState<TranscriptFilter>(DEFAULT_TRANSCRIPT_FILTER);
  const [showLatest, setShowLatest] = useState(false);
  const [scrollTarget, setScrollTarget] = useState<"start" | "end" | null>(null);
  const transcriptStart = useRef<HTMLDivElement>(null);
  const transcriptEnd = useRef<HTMLDivElement>(null);

  const sessionQuery = useQuery({
    queryKey: ["session-archive", conversationId, tenantId],
    queryFn: () => sessionArchive.get(conversationId, { tenant_id: tenantId }),
    // A missing or stale transcript is rebuilt server-side; poll until done.
    refetchInterval: (query) => (query.state.data?.rebuilding ? REBUILD_POLL_MS : false),
  });
  const data = sessionQuery.data;
  const session = data?.session ?? null;
  const transcriptReadable = session !== null && data?.transcript_readable !== false;
  const firstSeq = showLatest ? Math.max(1, (session?.last_seq ?? 1) - TRANSCRIPT_PAGE_TURNS + 1) : 1;

  const transcriptQuery = useInfiniteQuery({
    queryKey: ["session-transcript", conversationId, data?.tenant_id, session?.generation, session?.last_seq, firstSeq],
    enabled: transcriptReadable,
    initialPageParam: firstSeq,
    queryFn: ({ pageParam }) =>
      sessionArchive.transcript(conversationId, {
        tenant_id: data?.tenant_id,
        from_seq: pageParam,
        limit: TRANSCRIPT_PAGE_TURNS,
      }),
    getNextPageParam: (lastPage) => lastPage.next_seq ?? undefined,
  });

  const rebuild = useMutation({
    mutationFn: () => sessionArchive.rebuild(conversationId, { tenant_id: data?.tenant_id }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["session-archive", conversationId] }),
  });

  const turns = useMemo(() => mergeTranscriptPages(transcriptQuery.data?.pages ?? []), [transcriptQuery.data]);
  const hidden = useMemo(() => countHidden(turns, filter), [turns, filter]);
  const tools = useMemo(() => toolCallSummary(data?.turns ?? []), [data?.turns]);
  const costByRequest = useMemo(
    () => new Map((data?.turns ?? []).map((t) => [t.request_id, t.cost_total_cents])),
    [data?.turns],
  );

  // Wait for the requested page to render before scrolling. Jumping to the end
  // reads only the final page, rather than downloading every intervening turn.
  useEffect(() => {
    if (!scrollTarget || !transcriptQuery.isSuccess) return;
    const target = scrollTarget === "end" ? transcriptEnd.current : transcriptStart.current;
    target?.scrollIntoView({ block: scrollTarget === "end" ? "end" : "start", behavior: "instant" });
    setScrollTarget(null);
  }, [scrollTarget, transcriptQuery.isSuccess, transcriptQuery.data]);

  function jumpTo(target: "start" | "end") {
    setShowLatest(target === "end");
    setScrollTarget(target);
    if (transcriptQuery.isError) void transcriptQuery.refetch();
  }

  const back = (
    <Button variant="ghost" size="sm" className="-ml-2 mb-2" onClick={() => navigate("/sessions")}>
      <ArrowLeft className="mr-1.5 h-4 w-4" />
      Back to sessions
    </Button>
  );

  if (sessionQuery.isLoading) {
    return (
      <div className="flex items-center justify-center py-20">
        <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        <span className="ml-2 text-muted-foreground">Loading session…</span>
      </div>
    );
  }

  if (sessionQuery.error instanceof Error) {
    return (
      <PageContainer>
        <div>{back}</div>
        <p className="text-destructive">{sessionQuery.error.message}</p>
      </PageContainer>
    );
  }

  const matches = data?.matches ?? [];
  const tenantSwitcher = matches.length > 1 && (
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
      <span>This conversation id exists under {matches.length} tenants:</span>
      {matches.map((m) => (
        <Button
          key={m.tenant_id || "unauthenticated"}
          size="sm"
          variant={m.tenant_id === data?.tenant_id ? "default" : "outline"}
          className="h-7"
          onClick={() => {
            setTenantId(m.tenant_id);
            setShowLatest(false);
            setScrollTarget(null);
          }}
        >
          {m.tenant_name || m.tenant_id || "Unauthenticated"}
        </Button>
      ))}
    </div>
  );

  if (!session) {
    const requestCount = matches.find((m) => m.tenant_id === data?.tenant_id)?.request_count;
    return (
      <PageContainer>
        <div>
          {back}
          <PageHeading title="Session" description={`Conversation ${conversationId}`} />
        </div>
        {tenantSwitcher}
        <Card>
          <CardContent className="flex items-center gap-3 py-10 text-sm text-muted-foreground">
            {data?.rebuilding ? (
              <>
                <Loader2 className="h-5 w-5 shrink-0 animate-spin" />
                <span>
                  Building the transcript from {requestCount?.toLocaleString() ?? "the"} captured requests. Large sessions can
                  take a few minutes; this page updates automatically.
                </span>
              </>
            ) : (
              <span>No transcript is available for this conversation yet.</span>
            )}
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  const owner = session.user_display_name || session.user_email || (session.key_id ? `key ${session.key_id}` : "Unauthenticated");
  const ownerDetail = [session.user_email, session.key_id && `key ${session.key_id}`].filter(Boolean).join(" · ");

  return (
    <PageContainer className="pb-20">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          {back}
          <PageHeading
            title={sessionTitle(session)}
            description={`${owner} · ${session.tenant_name || session.tenant_id || "No tenant"} · ${session.model || "unknown model"}`}
          />
        </div>
        <div className="flex items-center gap-2">
          {data?.rebuilding ? (
            <Badge variant="info">
              <Loader2 className="mr-1 h-3 w-3 animate-spin" />
              rebuilding
            </Badge>
          ) : session.needs_rebuild ? (
            <Badge variant="warning" title={session.last_error}>
              needs rebuild
            </Badge>
          ) : null}
          <Button
            variant="outline"
            size="sm"
            disabled={rebuild.isPending || data?.rebuilding}
            onClick={() => rebuild.mutate()}
            title="Regenerate the transcript from the raw captured requests"
          >
            <RefreshCw className="mr-1.5 h-4 w-4" />
            Rebuild
          </Button>
        </div>
      </div>
      {tenantSwitcher}

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <SummaryMetric label="User" value={owner} title={ownerDetail} />
        <SummaryMetric label="Requests" value={session.request_count.toLocaleString()} />
        <SummaryMetric label="User turns" value={session.user_turn_count.toLocaleString()} />
        <SummaryMetric label="Tool calls" value={session.tool_call_count.toLocaleString()} />
        <SummaryMetric label="Cost" value={formatDetailCost(session.cost_total_cents)} />
        <SummaryMetric label="Started" value={formatDateFull(session.started_at)} />
        <SummaryMetric label="Last activity" value={formatDateFull(session.last_activity_at)} />
        <SummaryMetric
          label="Transcript"
          value={`${formatBytes(session.transcript_bytes)} · ${session.objects_location || "—"}`}
        />
        <SummaryMetric label="Resets · errors" value={`${session.reset_count} · ${session.error_count}`} />
        {(data?.client_ips?.length ?? 0) > 0 && (
          <div className="rounded-md border border-border bg-card/70 p-3 sm:col-span-2 lg:col-span-4">
            <div className="text-xs font-medium uppercase text-muted-foreground">Client IPs</div>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {data?.client_ips?.map((ip) => (
                <Badge
                  key={ip.ip}
                  variant="outline"
                  className="font-mono text-xs"
                  title={`${ip.requests.toLocaleString()} requests · first ${formatDateFull(ip.first_seen)} · last ${formatDateFull(ip.last_seen)}`}
                >
                  {ip.ip}
                  {(data.client_ips?.length ?? 0) > 1 ? ` ×${ip.requests.toLocaleString()}` : ""}
                </Badge>
              ))}
              <span className="text-xs text-muted-foreground">
                source addresses the proxy observed for this session&rsquo;s requests
              </span>
            </div>
          </div>
        )}
      </div>

      {session.opening_prompt && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium text-muted-foreground">Opening prompt</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="max-h-80 overflow-auto whitespace-pre-wrap break-words text-sm leading-6">{session.opening_prompt}</p>
          </CardContent>
        </Card>
      )}

      {tools.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          <span className="mr-1">Tools used:</span>
          {tools.slice(0, 12).map((t) => (
            <Badge key={t.name} variant="outline" className="font-mono text-xs">
              {t.name} × {t.count}
            </Badge>
          ))}
        </div>
      )}

      <div ref={transcriptStart} className="scroll-mt-4" />
      <Card>
        <CardHeader className="pb-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
                <MessageSquare className="h-4 w-4" />
                Conversation
              </CardTitle>
              <CardDescription>
                {turns.length > 0
                  ? `Requests ${turns[0]!.seq.toLocaleString()}–${turns[turns.length - 1]!.seq.toLocaleString()} of ${session.last_seq.toLocaleString()}`
                  : `0 of ${session.last_seq.toLocaleString()} requests loaded`}
              </CardDescription>
            </div>
            <div className="flex flex-wrap gap-2">
              <FilterToggle
                label="tool messages"
                active={filter.showTools}
                hiddenCount={hidden.tools}
                onClick={() => setFilter((f) => ({ ...f, showTools: !f.showTools }))}
              />
              <FilterToggle
                label="reasoning"
                active={filter.showReasoning}
                hiddenCount={hidden.reasoning}
                onClick={() => setFilter((f) => ({ ...f, showReasoning: !f.showReasoning }))}
              />
              <FilterToggle
                label="repeated output"
                active={filter.showEchoes}
                hiddenCount={hidden.echoes}
                onClick={() => setFilter((f) => ({ ...f, showEchoes: !f.showEchoes }))}
              />
            </div>
          </div>
        </CardHeader>
        <CardContent className="space-y-5">
          {firstSeq > 1 && (
            <p className="text-sm text-muted-foreground">
              Showing the last {TRANSCRIPT_PAGE_TURNS} requests. Use Back to start to read from the beginning.
            </p>
          )}
          {!transcriptReadable && (
            <div className="flex items-center gap-3 py-6 text-sm text-muted-foreground">
              <Loader2 className="h-5 w-5 shrink-0 animate-spin" />
              <span>
                Rebuilding this transcript from the {session.request_count.toLocaleString()} captured requests (it was
                stored before the object storage change). This page updates automatically.
              </span>
            </div>
          )}
          {transcriptQuery.isLoading && <MessageBlock role="loading" text="Loading transcript…" />}
          {transcriptQuery.error instanceof Error && (
            <p className="text-sm text-destructive">{transcriptQuery.error.message}</p>
          )}
          {turns.map((turn) => (
            <TranscriptTurnView key={turn.seq} turn={turn} filter={filter} costCents={costByRequest.get(turn.request_id)} />
          ))}
          {transcriptQuery.hasNextPage && (
            <div className="flex justify-center">
              <Button
                variant="outline"
                size="sm"
                disabled={transcriptQuery.isFetchingNextPage}
                onClick={() => transcriptQuery.fetchNextPage()}
              >
                {transcriptQuery.isFetchingNextPage && <Loader2 className="mr-1.5 h-4 w-4 animate-spin" />}
                Load more
              </Button>
            </div>
          )}
          <div ref={transcriptEnd} className="scroll-mb-24" />
        </CardContent>
      </Card>
      {transcriptReadable && session.last_seq > 0 && (
        <nav
          aria-label="Conversation navigation"
          className="fixed bottom-4 right-4 z-30 flex gap-2 rounded-lg border border-border bg-card p-2 shadow-lg sm:bottom-6 sm:right-6"
        >
          <Button variant="outline" size="sm" onClick={() => jumpTo("start")}>
            <ArrowUpToLine aria-hidden="true" />
            Back to start
          </Button>
          <Button size="sm" disabled={scrollTarget === "end" && transcriptQuery.isFetching} onClick={() => jumpTo("end")}>
            {scrollTarget === "end" && transcriptQuery.isFetching
              ? <Loader2 aria-hidden="true" className="animate-spin" />
              : <ArrowDownToLine aria-hidden="true" />}
            {scrollTarget === "end" && transcriptQuery.isFetching ? "Loading end…" : "Jump to end"}
          </Button>
        </nav>
      )}
    </PageContainer>
  );
}

function SummaryMetric({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <div className="rounded-md border border-border bg-card/70 p-3">
      <div className="text-xs font-medium uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 truncate text-sm font-semibold" title={title || value}>
        {value}
      </div>
    </div>
  );
}

function FilterToggle({
  label,
  active,
  hiddenCount,
  onClick,
}: {
  label: string;
  active: boolean;
  hiddenCount: number;
  onClick: () => void;
}) {
  return (
    <Button variant="outline" size="sm" className="h-8" onClick={onClick}>
      {active ? `Hide ${label}` : `Show ${label}${hiddenCount > 0 ? ` (${hiddenCount})` : ""}`}
    </Button>
  );
}

function TranscriptTurnView({
  turn,
  filter,
  costCents,
}: {
  turn: SessionTranscriptTurn;
  filter: TranscriptFilter;
  costCents?: number;
}) {
  const notice = turnNotice(turn);
  const items = turn.items.filter((item) => isVisibleItem(item, filter));
  const output = turn.output.filter((item) => isVisibleItem(item, filter));

  return (
    <section className="space-y-3 rounded-md border border-border bg-background/40 p-4">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <Badge variant="outline" className="font-mono text-xs">
          #{turn.seq}
        </Badge>
        <span className="font-mono" title={formatDateFull(turn.created_at)}>
          {timeAgo(turn.created_at)}
        </span>
        <Badge variant="outline" className="font-mono text-xs">
          {turn.model}
        </Badge>
        {turn.status !== "success" && <Badge variant="destructive">{turn.status}</Badge>}
        {turn.kind !== "extend" && <Badge variant="warning">{turn.kind}</Badge>}
        {costCents ? <span className="font-mono">{formatDetailCost(costCents)}</span> : null}
        <span className="ml-auto font-mono text-[10px]">{turn.request_id}</span>
      </div>
      {notice && <p className="text-xs text-warning">{notice}</p>}
      {turn.system !== undefined && (
        <CollapsibleBlock
          label={turn.seq === 1 ? "system prompt" : "system prompt changed"}
          channel="system"
          body={turn.system || "(empty)"}
        />
      )}
      {items.map((item, i) => (
        <DisplayItemBlock key={`in-${i}`} item={item} />
      ))}
      {output.map((item, i) => (
        <DisplayItemBlock key={`out-${i}`} item={item} />
      ))}
      {turn.error && <MessageBlock role="error" text={turn.error} />}
      <RawPayloadDetails requestId={turn.request_id} />
    </section>
  );
}

const TRUNCATED_NOTE = "Truncated for display. Open the raw request & response for the full body.";

function DisplayItemBlock({ item }: { item: SessionDisplayItem }) {
  const note = item.truncated ? TRUNCATED_NOTE : undefined;
  switch (item.kind) {
    case "reasoning":
      return <ReasoningBlock text={item.text ?? ""} encrypted={item.encrypted} />;
    case "tool_call":
      return (
        <CollapsibleBlock
          label={`call · ${item.name || "unnamed"}`}
          channel="tool"
          body={item.arguments || "(no arguments)"}
          note={note}
        />
      );
    case "tool_result":
      return <CollapsibleBlock label="result" channel="tool" body={item.text || "(empty)"} note={note} />;
    default:
      return <MessageBlock role={item.kind === "other" && item.name ? item.name : item.kind} text={item.text ?? ""} note={note} />;
  }
}

function MessageBlock({ role, text, note }: { role: string; text: string; note?: string }) {
  const accent = roleAccent(role);
  return (
    <div className="grid gap-2 md:grid-cols-[7rem_1fr]">
      <div className={`text-xs font-semibold uppercase tracking-wide ${accent.label}`}>{role}</div>
      <div className={`whitespace-pre-wrap break-words rounded-md border-l-2 ${accent.bar} bg-muted/70 p-3 text-sm leading-6`}>
        {text || <span className="text-muted-foreground">Empty message</span>}
        {note && <p className="mt-2 text-[11px] text-muted-foreground">{note}</p>}
      </div>
    </div>
  );
}

// roleAccent color-codes each conversation channel so user / assistant / system /
// tool turns are visually separable at a glance, in the instrument-panel palette.
function roleAccent(role: string): { label: string; bar: string } {
  switch (role.toLowerCase()) {
    case "assistant":
      return { label: "text-primary", bar: "border-primary/50" };
    case "user":
      return { label: "text-accent", bar: "border-accent/50" };
    case "tool":
      return { label: "text-warning", bar: "border-warning/50" };
    case "error":
      return { label: "text-destructive", bar: "border-destructive/50" };
    default:
      return { label: "text-muted-foreground", bar: "border-border" };
  }
}

function CollapsibleBlock({
  label,
  body,
  note,
  channel,
}: {
  label: string;
  body: string;
  note?: string;
  channel: "tool" | "system";
}) {
  const accent = roleAccent(channel);
  return (
    <div className="grid gap-2 md:grid-cols-[7rem_1fr]">
      <div className={`text-xs font-semibold uppercase tracking-wide ${accent.label}`}>{channel}</div>
      <details className="group rounded-md border border-border bg-muted/40">
        <summary className="flex cursor-pointer list-none select-none items-center gap-2 px-3 py-2 text-xs text-muted-foreground">
          <ChevronRight className="h-3.5 w-3.5 shrink-0 transition-transform group-open:rotate-90" />
          <span className="truncate font-mono">{label}</span>
          <span className="ml-auto shrink-0 font-mono text-[10px]">{body.length.toLocaleString()} chars</span>
        </summary>
        <pre className="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words border-t border-border p-3 text-xs leading-5">
          {body}
        </pre>
        {note && <p className="border-t border-border px-3 py-1.5 text-[11px] text-muted-foreground">{note}</p>}
      </details>
    </div>
  );
}

// ReasoningBlock renders the model's chain-of-thought as its own collapsed
// channel, distinct from the assistant's answer — violet so it doesn't collide
// with the cyan (assistant) / green (user) / amber (tool) channels.
function ReasoningBlock({ text, encrypted }: { text: string; encrypted?: boolean }) {
  return (
    <div className="grid gap-2 md:grid-cols-[7rem_1fr]">
      <div className="flex items-start text-xs font-semibold uppercase tracking-wide text-violet-300/90">
        <Brain className="mr-1.5 mt-px h-3.5 w-3.5 shrink-0" />
        Reasoning
      </div>
      {encrypted || !text ? (
        <div className="rounded-md border border-violet-400/25 bg-violet-400/[0.06] px-3 py-2 text-xs italic text-muted-foreground">
          {encrypted ? "Encrypted reasoning (not readable)" : "No reasoning text"}
        </div>
      ) : (
        <details className="group rounded-md border border-violet-400/25 bg-violet-400/[0.06]">
          <summary className="flex cursor-pointer list-none select-none items-center gap-2 px-3 py-2 text-xs text-violet-200/80">
            <ChevronRight className="h-3.5 w-3.5 shrink-0 transition-transform group-open:rotate-90" />
            <span className="font-medium">Chain of thought</span>
            <span className="ml-auto font-mono text-[10px] text-muted-foreground">{text.length.toLocaleString()} chars</span>
          </summary>
          <div className="whitespace-pre-wrap break-words border-t border-violet-400/20 p-3 text-sm italic leading-6 text-muted-foreground">
            {text}
          </div>
        </details>
      )}
    </div>
  );
}

// RawPayloadDetails fetches a turn's captured request and response only when
// opened: bodies are the full cumulative conversation and can be megabytes.
export function RawPayloadDetails({ requestId }: { requestId: string }) {
  const [open, setOpen] = useState(false);
  const payload = useQuery({
    queryKey: ["conversation-payload", requestId],
    queryFn: () => conversationPayloads.get(requestId),
    enabled: open,
    staleTime: Infinity,
  });
  return (
    <details
      className="rounded-md border border-border bg-muted/30"
      onToggle={(event) => setOpen((event.currentTarget as HTMLDetailsElement).open)}
    >
      <summary className="cursor-pointer px-3 py-2 text-xs font-semibold uppercase text-muted-foreground">
        Raw request &amp; response
      </summary>
      {open && (
        <div className="space-y-2 border-t border-border p-3">
          {payload.isLoading && <p className="text-xs text-muted-foreground">Loading captured bodies…</p>}
          {payload.error instanceof Error && <p className="text-xs text-destructive">{payload.error.message}</p>}
          {payload.data && (
            <>
              <RawBody
                title={`Request${payload.data.request_truncated ? " (truncated at capture)" : ""}`}
                body={formatRawBody(payload.data.request_body)}
              />
              <RawBody
                title={`Response${payload.data.response_truncated ? " (truncated at capture)" : ""}`}
                body={payload.data.response_body || "Empty response body"}
              />
            </>
          )}
        </div>
      )}
    </details>
  );
}

function RawBody({ title, body }: { title: string; body: string }) {
  return (
    <details className="rounded-md border border-border bg-background/40">
      <summary className="cursor-pointer px-3 py-2 text-xs font-semibold uppercase text-muted-foreground">{title}</summary>
      <pre className="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words border-t border-border p-3 text-xs leading-5">
        {body}
      </pre>
    </details>
  );
}

function formatRawBody(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2);
  } catch {
    return body;
  }
}
