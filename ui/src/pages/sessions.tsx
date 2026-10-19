import { useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { sessionSubmissions } from "@/lib/api";
import { PageContainer, PageHeading } from "@/components/page";
import type { SessionSubmission } from "@/lib/types";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDate, formatTokens } from "@/lib/utils";
import { User } from "lucide-react";
import { ConversationSessionsPage } from "./conversation-payloads";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { SessionDetailPage } from "./session-detail";

const PAGE_SIZE = 50;
const DISTILLER_MIN_MESSAGES = 3;
const DISTILLER_IDLE_MS = 15 * 60 * 1000;

// ── Helpers ─────────────────────────────────────────────────────────────────

function DistillBadge({ s }: { s: SessionSubmission }) {
  if (s.distilled_at) {
    if (s.distill_error?.startsWith("failed")) {
      return <Badge variant="destructive" title={s.distill_error}>failed</Badge>;
    }
    if (s.distill_error?.startsWith("skipped") || s.distill_error === "no durable observations") {
      return <Badge variant="warning" title={s.distill_error}>{s.distill_error.replace("skipped: ", "skipped: ").slice(0, 32)}</Badge>;
    }
    return <Badge variant="success" title={`distilled ${formatDate(s.distilled_at)}`}>distilled</Badge>;
  }
  if (s.distill_attempts > 0) {
    return <Badge variant="outline">retrying ({s.distill_attempts})</Badge>;
  }
  if (s.message_count < DISTILLER_MIN_MESSAGES) {
    return (
      <Badge variant="outline" title={`Distiller requires at least ${DISTILLER_MIN_MESSAGES} messages`}>
        too short
      </Badge>
    );
  }
  if (!s.ended_at) {
    return <Badge variant="outline" title="Waiting for the extension to submit session shutdown metadata">active</Badge>;
  }
  const endedAt = new Date(s.ended_at).getTime();
  if (!Number.isNaN(endedAt)) {
    const eligibleAt = endedAt + DISTILLER_IDLE_MS;
    if (Date.now() < eligibleAt) {
      return <Badge variant="outline" title={`Eligible after ${formatDate(new Date(eligibleAt).toISOString())}`}>waiting idle</Badge>;
    }
  }
  return <Badge variant="outline">pending</Badge>;
}

// ThinkingBadge renders the session's last effective Pi thinking level with
// escalating color so heavier reasoning modes stand out. Absent for sessions
// from extensions older than 1.3.25.
function ThinkingBadge({ level }: { level?: string }) {
  if (!level) return <span className="text-muted-foreground">—</span>;
  const variant =
    level === "off" || level === "minimal" || level === "low"
      ? "outline"
      : level === "medium" || level === "high"
        ? "info"
        : "warning";
  return <Badge variant={variant}>{level}</Badge>;
}

// SessionTitle shows the explicit /name session title, falling back to a
// truncated first user prompt for unnamed sessions.
function SessionTitle({ s }: { s: SessionSubmission }) {
  if (s.session_name) {
    return <span className="font-medium" title={s.session_name}>{s.session_name}</span>;
  }
  if (s.first_prompt) {
    return (
      <span className="block max-w-[280px] truncate text-muted-foreground" title={s.first_prompt}>
        {s.first_prompt}
      </span>
    );
  }
  return <span className="text-muted-foreground">—</span>;
}

// SessionUser shows who ran the session: display name, else email, else the
// raw user id. Submissions uploaded without an attached user fall back to the
// API key id, matching the session detail page's attribution.
function SessionUser({ s }: { s: SessionSubmission }) {
  const label = s.user_display_name || s.user_email || s.user_id || "";
  if (!label) {
    return (
      <span className="text-muted-foreground" title={s.key_id ? `API key ${s.key_id}` : "No user attached"}>
        {s.key_id ? "API key" : "—"}
      </span>
    );
  }
  const detail = [s.user_email, s.user_id].filter(Boolean).join(" · ");
  return (
    <div className="flex items-center gap-1.5" title={detail || label}>
      <User className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
      <span className="max-w-[140px] truncate font-medium">{label}</span>
    </div>
  );
}

// ── Router ──────────────────────────────────────────────────────────────────

export function SessionsPage() {
  const params = useParams();
  const [search] = useSearchParams();
  if (params.conversationId) {
    return <SessionDetailPage conversationId={params.conversationId} initialTenantId={search.has("tenant_id") ? search.get("tenant_id")! : undefined} />;
  }
  return <SessionsHome />;
}

function SessionsHome() {
  const [search, setSearch] = useSearchParams();
  const view = search.get("view") === "uploaded" ? "uploaded" : "captured";
  return (
    <PageContainer>
      <PageHeading title="Sessions" description="Browse captured conversations and uploaded agent sessions." />
      <Tabs value={view} onValueChange={(value) => setSearch(value === "uploaded" ? { view: value } : {})}>
        <TabsList>
          <TabsTrigger value="captured">Captured conversations</TabsTrigger>
          <TabsTrigger value="uploaded">Uploaded sessions</TabsTrigger>
        </TabsList>
        <TabsContent value="captured"><ConversationSessionsPage /></TabsContent>
        <TabsContent value="uploaded"><SessionsListPage /></TabsContent>
      </Tabs>
    </PageContainer>
  );
}

// ── Session list ────────────────────────────────────────────────────────────

function SessionsListPage() {
  const navigate = useNavigate();
  const [offset, setOffset] = useState(0);
  const { data, isLoading, error } = useQuery({
    queryKey: ["session-submissions", offset],
    queryFn: () => sessionSubmissions.list({ limit: PAGE_SIZE, offset }),
  });

  const rows = data?.session_submissions ?? [];
  const total = data?.total_count ?? 0;

  return (
    <PageContainer>
      <Card>
        <CardHeader>
          <CardTitle>Uploaded sessions</CardTitle>
          <CardDescription>{total} total</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {error instanceof Error && (
            <p className="text-sm text-destructive">{error.message}</p>
          )}
          {!isLoading && rows.length === 0 && (
            <p className="text-sm text-muted-foreground">No sessions uploaded yet.</p>
          )}
          {rows.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Last activity</TableHead>
                  <TableHead>Session</TableHead>
                  <TableHead>User</TableHead>
                  <TableHead>Project</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Thinking</TableHead>
                  <TableHead className="text-right">Msgs</TableHead>
                  <TableHead className="text-right">Tokens</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                  <TableHead>Ext</TableHead>
                  <TableHead>Distillation</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((s) => {
                  const hasId = Boolean(s.conversation_id);

                  return (
                    <TableRow
                      key={s.id}
                      className={hasId ? "cursor-pointer hover:bg-muted/50" : ""}
                      onClick={() => {
                        if (hasId) {
                          navigate(`/sessions/${encodeURIComponent(s.conversation_id!)}`);
                        }
                      }}
                      title={hasId ? "Click to view full conversation" : "No conversation ID available"}
                    >
                      <TableCell className="whitespace-nowrap">{formatDate(s.last_activity_at || s.ended_at || s.created_at)}</TableCell>
                      <TableCell><SessionTitle s={s} /></TableCell>
                      <TableCell><SessionUser s={s} /></TableCell>
                      <TableCell>
                        {s.project ? <Badge variant="info">{s.project}</Badge> : <span className="text-muted-foreground">—</span>}
                      </TableCell>
                      <TableCell className="max-w-[180px] truncate" title={s.model}>{s.model || "—"}</TableCell>
                      <TableCell><ThinkingBadge level={s.thinking_level} /></TableCell>
                      <TableCell className="text-right">{s.message_count}</TableCell>
                      <TableCell className="text-right">{s.total_tokens ? formatTokens(s.total_tokens) : "—"}</TableCell>
                      <TableCell className="text-right">
                        {s.total_cost_cents ? `$${(s.total_cost_cents / 100).toFixed(2)}` : "—"}
                      </TableCell>
                      <TableCell
                        className="text-muted-foreground"
                        title={s.run_mode ? `run mode: ${s.run_mode}` : undefined}
                      >
                        {s.extension_version || "—"}
                        {s.run_mode && s.run_mode !== "tui" ? <span className="ml-1 text-xs">· {s.run_mode}</span> : null}
                      </TableCell>
                      <TableCell><DistillBadge s={s} /></TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}

          {total > PAGE_SIZE && (
            <div className="mt-4 flex items-center justify-between">
              <p className="text-sm text-muted-foreground">
                {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}
              </p>
              <div className="flex gap-2">
                <Button variant="outline" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
                  Previous
                </Button>
                <Button variant="outline" size="sm" disabled={offset + PAGE_SIZE >= total} onClick={() => setOffset(offset + PAGE_SIZE)}>
                  Next
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}
