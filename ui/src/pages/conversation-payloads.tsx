import { belongsToCapturedSession, type CapturedSessionRef as SessionRef } from "@/lib/captured-session";
import { RawPayloadDetails } from "./session-detail";
import { useMemo, useState } from "react";
import { Navigate, useLocation, useNavigate, useParams } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { conversationPayloads as payloadsApi, users as usersApi } from "@/lib/api";
import { useTenantNames } from "@/hooks/use-queries";
import { PageContainer, PageHeading } from "@/components/page";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { timeAgo, formatDate, formatDetailCost, encodeSessionRef } from "@/lib/utils";
import {
  ArrowLeft, Loader2, MessageSquare, Search, User,
} from "lucide-react";
import type { ConversationPayloadSummary, ConversationSessionSummary, User as TenantUser } from "@/lib/types";

export function ConversationPayloadsPage() {
  const params = useParams();
  const location = useLocation();
  if (location.pathname.startsWith("/conversation-payloads")) {
    return <Navigate replace to={params.chatKey ? `/sessions/captured/${encodeURIComponent(params.chatKey)}` : "/sessions"} />;
  }
  if (params.chatKey) {
    return <ConversationChatPage key={params.chatKey} chatKey={params.chatKey} />;
  }
  return <Navigate replace to="/sessions" />;
}

const PAGE_SIZE = 100;

export function ConversationSessionsPage() {
  const navigate = useNavigate();
  const [requestID, setRequestID] = useState("");
  const [tenantID, setTenantID] = useState("");
  const [userID, setUserID] = useState("");
  const [provider, setProvider] = useState("");
  const [offset, setOffset] = useState(0);
  const { tenants: tenantList } = useTenantNames();

  const usersQuery = useQuery({
    queryKey: ["conversation-payload-users", tenantID],
    enabled: tenantID !== "",
    queryFn: () => usersApi.list(tenantID),
  });

  // Session-level pagination: each row is a logical chat session, not a raw payload.
  const { data, isLoading, isFetching, error } = useQuery({
    queryKey: ["conversation-sessions", { tenant_id: tenantID, user_id: userID, provider, offset }],
    queryFn: () =>
      payloadsApi.sessions.list({
        tenant_id: tenantID || undefined,
        user_id: userID || undefined,
        provider: provider || undefined,
        limit: PAGE_SIZE,
        offset,
      }),
    placeholderData: (prev) => prev,
  });

  const totalCount = data?.total_count ?? 0;
  const page = Math.floor(offset / PAGE_SIZE) + 1;
  const totalPages = Math.max(1, Math.ceil(totalCount / PAGE_SIZE));

  const resetOffset = () => setOffset(0);

  // Search by request ID falls back to payload-level query.
  const requestQuery = useQuery({
    queryKey: ["conversation-request-search", requestID],
    enabled: requestID.trim().length > 3,
    queryFn: () => payloadsApi.list({ request_id: requestID, limit: 10, include_prompts: true }),
  });

  const sessions: Array<ConversationSessionSummary & { key: string }> = useMemo(() => {
    if (requestID.trim().length > 3) {
      // In request-ID search mode, build a synthetic session per payload.
      return (requestQuery.data?.conversation_payloads ?? []).map((p) => ({
        conversation_id: p.conversation_id,
        tenant_id: p.tenant_id,
        user_id: p.user_id,
        tenant_name: p.tenant_name,
        user_display_name: p.user_display_name,
        user_email: p.user_email,
        provider: p.provider,
        model: p.model,
        status: p.status,
        success_count: p.status === "success" ? 1 : 0,
        failure_count: p.status === "error" && p.error_class !== "client_closed" ? 1 : 0,
        interrupted_count: p.error_class === "client_closed" || p.status === "cancelled" ? 1 : 0,
        message_count: 1,
        last_prompt_role: p.last_prompt_role,
        last_prompt_text: p.last_prompt_text,
        cost_total_cents: p.cost_total_cents ?? 0,
        started_at: p.created_at,
        ended_at: p.created_at,
        key: encodeSessionRef({
          conversation_id: p.conversation_id,
          tenant_id: p.tenant_id,
          user_id: p.user_id,
          owner_id: p.user_id ? `user:${p.user_id}` : p.key_id ? `key:${p.key_id}` : "",
          from: p.created_at,
          to: p.created_at,
        }),
      }));
    }
    return (data?.sessions ?? []).map((s) => ({
      ...s,
      key: encodeSessionRef({
        conversation_id: s.conversation_id,
        tenant_id: s.tenant_id,
        user_id: s.user_id,
        owner_id: s.owner_id,
        transcript_state: s.transcript_state,
        from: s.started_at,
        to: s.ended_at,
      }),
    }));
  }, [data?.sessions, requestID, requestQuery.data]);

  const isLoadingSessions = requestID.trim().length > 3 ? requestQuery.isLoading : isLoading;
  const listError = requestID.trim().length > 3 ? requestQuery.error : error;
  const users = usersQuery.data?.users ?? [];

  return (
    <PageContainer>
      {listError instanceof Error && <p role="alert" className="text-destructive">{listError.message}</p>}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <Search className="h-4 w-4" />
            Search
          </CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex flex-wrap items-end gap-4">
            <div className="space-y-1.5">
              <Label className="text-xs">Request ID</Label>
              <Input
                placeholder="req_..."
                value={requestID}
                onChange={(e) => { setRequestID(e.target.value); resetOffset(); }}
                className="w-56 font-mono text-xs"
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Tenant</Label>
              <Select
                value={tenantID || "__all__"}
                onValueChange={(v) => {
                  setTenantID(v === "__all__" ? "" : v);
                  setUserID("");
                  resetOffset();
                }}
              >
                <SelectTrigger className="w-56">
                  <SelectValue placeholder="All tenants" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="__all__">All tenants</SelectItem>
                  {tenantList.map((t) => (
                    <SelectItem key={t.id} value={t.id}>{t.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">User</Label>
              <Select
                value={userID || "__all__"}
                onValueChange={(v) => { setUserID(v === "__all__" ? "" : v); resetOffset(); }}
                disabled={!tenantID}
              >
                <SelectTrigger className="w-56">
                  <SelectValue placeholder={tenantID ? "All users" : "Select tenant first"} />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="__all__">All users</SelectItem>
                  {users.map((u) => (
                    <SelectItem key={u.id} value={u.id}>{userLabel(u)}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs">Provider</Label>
              <Input
                placeholder="deepseek-completions"
                value={provider}
                onChange={(e) => { setProvider(e.target.value); resetOffset(); }}
                className="w-48 font-mono text-xs"
              />
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            Conversations ({sessions.length})
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Started</TableHead>
                <TableHead>User</TableHead>
                <TableHead>Tenant</TableHead>
                <TableHead>Requests</TableHead>
                <TableHead>Preview</TableHead>
                                <TableHead>Model</TableHead>
                <TableHead>Transcript / upload</TableHead>
                  <TableHead>Request outcomes</TableHead>
                <TableHead className="text-right">Open</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoadingSessions ? (
                <TableRow>
                  <TableCell colSpan={9} className="py-8 text-center text-muted-foreground">
                    <Loader2 className="mx-auto mb-1 h-5 w-5 animate-spin" />
                    Loading...
                  </TableCell>
                </TableRow>
              ) : sessions.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={9} className="py-12 text-center text-muted-foreground">
                    <MessageSquare className="mx-auto mb-2 h-8 w-8 opacity-40" />
                    No chats found. Try adjusting your filters.
                  </TableCell>
                </TableRow>
              ) : (
                sessions.map((session) => (
                  <TableRow
                    key={session.key}
                    className="data-row cursor-pointer"
                    onClick={() => navigate(`/sessions/captured/${encodeURIComponent(session.key)}`)}
                  >
                    <TableCell className="text-xs font-mono text-muted-foreground" title={session.started_at}>
                      {timeAgo(session.started_at)}
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <User className="h-3.5 w-3.5 text-muted-foreground" />
                        <div>
                          <div className="text-sm font-medium">{session.user_display_name || "Unauthenticated"}</div>
                          {session.user_email && (
                            <div className="max-w-[150px] truncate text-xs text-muted-foreground" title={session.user_email}>{session.user_email}</div>
                          )}
                        </div>
                      </div>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">{session.tenant_name || "Unknown"}</TableCell>
                    <TableCell>
                      <Badge variant="outline" className="font-mono text-xs">{session.message_count}</Badge>
                    </TableCell>
                    <TableCell
                      className="max-w-[220px] truncate text-sm text-muted-foreground"
                      title={session.last_prompt_text || ""}
                    >
                      {session.last_prompt_text || "Preview not available"}
                    </TableCell>
                    <TableCell className="max-w-[150px] text-xs text-muted-foreground">
                      <div className="truncate font-mono" title={session.model}>{session.model}</div>
                      <div className="truncate" title={session.provider}>{session.provider}</div>
                    </TableCell>
                    <TableCell><SessionAvailability session={session} /></TableCell>
                    <TableCell><RequestOutcomes session={session} /></TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" className="h-7 px-2" aria-label="Open chat">
                        <MessageSquare className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Pagination — session-level count */}
      {!requestID.trim() && totalCount > PAGE_SIZE && (
        <div className="flex items-center justify-between gap-4 text-sm text-muted-foreground">
          <p>
            {totalCount.toLocaleString()} sessions · page {page} of {totalPages}
          </p>
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              disabled={offset <= 0 || isFetching}
              onClick={() => setOffset((o) => Math.max(0, o - PAGE_SIZE))}
            >
              Previous
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={offset + PAGE_SIZE >= totalCount || isFetching}
              onClick={() => setOffset((o) => o + PAGE_SIZE)}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </PageContainer>
  );
}

function ConversationChatPage({ chatKey }: { chatKey: string }) {
  const navigate = useNavigate();
  const [showTools, setShowTools] = useState(false);
  const sessionRef = decodeSessionKey(chatKey);
  const listQuery = useInfiniteQuery({
    queryKey: ["conversation-chat-list", sessionRef],
    enabled: Boolean(sessionRef),
    initialPageParam: 0,
    queryFn: ({ pageParam }) => payloadsApi.list({
      session_scope: JSON.stringify(sessionRef),
      include_prompts: true,
      limit: 100,
      offset: pageParam,
    }),
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((sum, page) => sum + (page.conversation_payloads?.length ?? 0), 0);
      return loaded < last.total_count && (last.conversation_payloads?.length ?? 0) > 0 ? loaded : undefined;
    },
  });

  const summaries = useMemo(() =>
    (listQuery.data?.pages.flatMap((page) => page.conversation_payloads ?? []) ?? [])
      .filter((payload) => sessionRef !== null && belongsToCapturedSession(payload, sessionRef))
      .sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at)),
    [listQuery.data, chatKey]);

  const first = summaries[0];
  const isLoading = listQuery.isLoading;
  const sessionStats = useMemo(() => buildSessionStats(summaries), [summaries]);
  const visibleSummaries = useMemo(
    () => summaries.filter((summary) => showTools || !isToolRole(summary.last_prompt_role)),
    [showTools, summaries],
  );
  const hiddenToolCount = summaries.length - visibleSummaries.length;

  return (
    <PageContainer>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <Button variant="ghost" size="sm" className="-ml-2 mb-2" onClick={() => navigate("/sessions")}>
            <ArrowLeft className="mr-1.5 h-4 w-4" />
            Back
          </Button>
          <PageHeading
            title="Session"
            description={first ? `${displayUser(first)} · ${first.tenant_name || "Unknown tenant"} · ${summaries.length} of ${listQuery.data?.pages[0]?.total_count ?? summaries.length} requests` : undefined}
          />
        </div>
      </div>

      {sessionRef?.conversation_id && sessionRef.transcript_state === "available" && (
        <Button variant="outline" onClick={() => navigate(`/sessions/${encodeURIComponent(sessionRef.conversation_id!)}?tenant_id=${encodeURIComponent(sessionRef.tenant_id ?? "")}`)}>
          Open full conversation transcript
        </Button>
      )}
      {listQuery.hasNextPage && (
        <Button variant="outline" disabled={listQuery.isFetchingNextPage} onClick={() => void listQuery.fetchNextPage()}>
          {listQuery.isFetchingNextPage ? "Loading…" : "Load older requests"}
        </Button>
      )}
      {sessionStats && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          <SummaryMetric label="Loaded request cost" value={formatDetailCost(sessionStats.costCents)} />
          <SummaryMetric label="Start" value={formatDate(sessionStats.start)} />
          <SummaryMetric label="End" value={formatDate(sessionStats.end)} />
          <SummaryMetric label="Duration" value={formatDuration(sessionStats.durationMs)} />
          <SummaryMetric label="Models used" value={sessionStats.models.join(", ")} title={sessionStats.models.join(", ")} />
        </div>
      )}

      <Card>
        <CardHeader className="pb-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <MessageSquare className="h-4 w-4" />
              Parsed Prompts
            </CardTitle>
            {hiddenToolCount > 0 && (
              <Button
                variant="outline"
                size="sm"
                className="h-8"
                onClick={() => setShowTools((v) => !v)}
              >
                {showTools ? "Hide tool calls" : `Show ${hiddenToolCount} tool call${hiddenToolCount === 1 ? "" : "s"}`}
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent className="space-y-5">
          {!sessionRef ? (
            <EmptyState message="Invalid chat link." />
          ) : isLoading ? (
            <div className="py-8 text-center text-muted-foreground">
              <Loader2 className="mx-auto mb-1 h-5 w-5 animate-spin" />
              Loading chat...
            </div>
          ) : listQuery.error instanceof Error ? (
            <p role="alert" className="text-destructive">{listQuery.error.message}</p>
          ) : summaries.length === 0 ? (
            <EmptyState message="No captured requests found for this session." />
          ) : (
            visibleSummaries.map((summary, idx) => (
              <PayloadPromptTurn key={summary.request_id} summary={summary} index={idx + 1} />
            ))
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}

function SummaryMetric({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <div className="rounded-md border border-border bg-card/70 p-3">
      <div className="text-xs font-medium uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 truncate text-sm font-semibold" title={title || value}>{value}</div>
    </div>
  );
}

function PayloadPromptTurn({ summary, index }: { summary: ConversationPayloadSummary; index: number }) {
  return (
    <section className="space-y-3 rounded-md border border-border bg-background/40 p-4">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <Badge variant="outline" className="font-mono text-xs">#{index}</Badge>
        <span className="font-mono">{timeAgo(summary.created_at)}</span>
        <span className="font-mono">{summary.request_id}</span>
        <StatusBadge status={summary.status} />
        <Badge variant="outline" className="font-mono text-xs">{summary.provider}</Badge>
      </div>

      <div className="space-y-3">
        {summary.last_prompt_text ? (
          <MessageBlock role={summary.last_prompt_role || "message"} text={summary.last_prompt_text} />
        ) : (
          <MessageBlock role="context" text="No user prompt text found for this request." />
        )}
      </div>
      <RawPayloadDetails requestId={summary.request_id} />
    </section>
  );
}

function MessageBlock({ role, text }: { role: string; text: string }) {
  return (
    <div className="grid gap-2 md:grid-cols-[7rem_1fr]">
      <div className="text-xs font-semibold uppercase text-muted-foreground">{role}</div>
      <div className="whitespace-pre-wrap break-words rounded-md bg-muted/70 p-3 text-sm leading-6">
        {text || <span className="text-muted-foreground">Empty message</span>}
      </div>
    </div>
  );
}

function EmptyState({ message }: { message: string }) {
  return (
    <div className="py-12 text-center text-muted-foreground">
      <MessageSquare className="mx-auto mb-2 h-8 w-8 opacity-40" />
      {message}
    </div>
  );
}

type SessionStats = {
  start: string;
  end: string;
  durationMs: number;
  costCents: number;
  models: string[];
};

function buildSessionStats(payloads: ConversationPayloadSummary[]): SessionStats | null {
  if (payloads.length === 0) return null;
  const first = payloads[0];
  if (!first) return null;
  const start = first.created_at;
  const end = payloads.reduce((latest, payload) => {
    const candidate = payload.completed_at || payload.created_at;
    return Date.parse(candidate) > Date.parse(latest) ? candidate : latest;
  }, first.completed_at || first.created_at);
  const models = Array.from(new Set(payloads.map((p) => p.model).filter(Boolean)));
  return {
    start,
    end,
    durationMs: Math.max(0, Date.parse(end) - Date.parse(start)),
    costCents: payloads.reduce((sum, payload) => sum + (payload.cost_total_cents || 0), 0),
    models,
  };
}

function isToolRole(role?: string): boolean {
  if (!role) return false;
  return ["tool", "function", "assistant_tool", "tool_result"].includes(role.toLowerCase());
}


function formatDuration(ms: number): string {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return seconds > 0 ? `${minutes}m ${seconds}s` : `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainingMinutes = minutes % 60;
  return remainingMinutes > 0 ? `${hours}h ${remainingMinutes}m` : `${hours}h`;
}

function decodeSessionKey(key: string): SessionRef | null {
  try {
    const normalized = key.replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized.padEnd(normalized.length + ((4 - (normalized.length % 4)) % 4), "=");
    const parsed = JSON.parse(atob(padded));
    if (!parsed.from || !parsed.to) return null;
    return parsed;
  } catch {
    return null;
  }
}

function displayUser(payload: ConversationPayloadSummary): string {
  return payload.user_display_name || payload.user_email || payload.user_id || "Unauthenticated";
}

function userLabel(user: TenantUser): string {
  return user.display_name || user.email || user.id;
}

const statusBadgeVariant: Record<string, "default" | "success" | "warning" | "destructive" | "info" | "outline"> = {
  success: "success",
  error: "destructive",
  cancelled: "warning",
};

function StatusBadge({ status }: { status: string }) {
  const variant = statusBadgeVariant[status] || "outline";
  return (
    <Badge variant={variant} className="font-mono text-xs">
      {status}
    </Badge>
  );
}

function SessionAvailability({ session }: { session: ConversationSessionSummary }) {
  if (!session.transcript_state || !session.upload_state) {
    return <span className="text-xs text-muted-foreground">Open session for availability</span>;
  }
  const transcript = {
    available: "Transcript ready",
    rebuilding: "Rebuild needed",
    updating: "Transcript updating",
    not_available: "Transcript not ready",
  }[session.transcript_state ?? "not_available"];
  const upload = {
    uploaded: "Session uploaded",
    pending: "Waiting for upload",
    not_uploaded: "No session upload",
  }[session.upload_state ?? "not_uploaded"];
  return (
    <div className="space-y-1 text-xs">
      <Badge variant={session.transcript_state === "available" ? "success" : "outline"}>{transcript}</Badge>
      <div className="text-muted-foreground">{upload}</div>
    </div>
  );
}

function RequestOutcomes({ session }: { session: ConversationSessionSummary }) {
  if (session.success_count === undefined) return <span className="text-xs text-muted-foreground">Not available</span>;
  return (
    <div className="flex flex-wrap gap-1">
      <Badge variant="outline">{session.success_count} succeeded</Badge>
      {!!session.interrupted_count && <Badge variant="outline" title="The client disconnected or the request was cancelled.">{session.interrupted_count} interrupted</Badge>}
      {!!session.failure_count && <Badge variant="destructive">{session.failure_count} failed</Badge>}
    </div>
  );
}
