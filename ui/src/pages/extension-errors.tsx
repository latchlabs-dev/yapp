import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Activity, KeyRound, RefreshCw, TriangleAlert } from "lucide-react";
import { extensionErrors } from "@/lib/api";
import { useTenantNames } from "@/hooks/use-queries";
import { formatDateFull, timeAgo } from "@/lib/utils";
import { PageContainer, PageHeading } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

const PAGE_SIZE = 50;

function shortID(value: string) {
  return value.length > 18 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value;
}

function operationLabel(operation: string) {
  return operation.replaceAll("_", " ");
}

function MetricCard({
  label,
  value,
  detail,
  icon: Icon,
}: {
  label: string;
  value: string;
  detail: string;
  icon: React.ComponentType<{ className?: string }>;
}) {
  return (
    <Card>
      <CardContent className="flex items-start justify-between gap-3 p-4">
        <div className="space-y-1">
          <p className="eyebrow">{label}</p>
          <p className="font-mono text-2xl font-semibold tracking-tight">{value}</p>
          <p className="text-xs text-muted-foreground">{detail}</p>
        </div>
        <Icon className="mt-1 size-4 text-muted-foreground/60" />
      </CardContent>
    </Card>
  );
}

export function ExtensionErrorsPage() {
  const [tenantFilter, setTenantFilter] = useState("");
  const [operationFilter, setOperationFilter] = useState("");
  const [offset, setOffset] = useState(0);
  const { tenants } = useTenantNames();

  const query = useQuery({
    queryKey: ["extension-errors", tenantFilter, operationFilter, offset],
    queryFn: () => extensionErrors.list({
      tenant_id: tenantFilter || undefined,
      operation: operationFilter || undefined,
      limit: PAGE_SIZE,
      offset,
    }),
    refetchInterval: 30_000,
  });

  const rows = query.data?.extension_errors ?? [];
  const total = query.data?.total_count ?? 0;
  const pageOccurrences = useMemo(
    () => rows.reduce((sum, row) => sum + row.occurrence_count, 0),
    [rows],
  );
  const affectedKeys = useMemo(
    () => new Set(rows.map((row) => row.key_id)).size,
    [rows],
  );
  const latest = rows[0]?.last_seen_at;

  return (
    <PageContainer>
      <PageHeading
        title="Errors"
        description="Aggregated extension failures reported silently by authenticated Pi clients. The list refreshes every 30 seconds."
      >
        <Button variant="outline" size="sm" onClick={() => query.refetch()} disabled={query.isFetching}>
          <RefreshCw className={`mr-1.5 size-4 ${query.isFetching ? "animate-spin" : ""}`} />
          Refresh
        </Button>
      </PageHeading>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <MetricCard
          label="Error groups"
          value={total.toLocaleString()}
          detail="Distinct signatures matching the filters"
          icon={TriangleAlert}
        />
        <MetricCard
          label="Occurrences"
          value={pageOccurrences.toLocaleString()}
          detail="Reports represented on this page"
          icon={Activity}
        />
        <MetricCard
          label="Affected keys"
          value={affectedKeys.toLocaleString()}
          detail="Unique API keys represented on this page"
          icon={KeyRound}
        />
        <MetricCard
          label="Most recent"
          value={latest ? timeAgo(latest) : "—"}
          detail={latest ? formatDateFull(latest) : "No reports received"}
          icon={RefreshCw}
        />
      </div>

      <Card>
        <CardHeader className="gap-4 lg:flex-row lg:items-end lg:justify-between">
          <div>
            <CardTitle>Extension reports</CardTitle>
            <CardDescription>
              Repeated failures with the same key, operation, error, project, and session are combined.
            </CardDescription>
          </div>
          <div className="flex flex-wrap gap-2">
            <Select
              value={tenantFilter || "__all__"}
              onValueChange={(value) => {
                setTenantFilter(value === "__all__" ? "" : value);
                setOffset(0);
              }}
            >
              <SelectTrigger className="w-56" aria-label="Filter by tenant">
                <SelectValue placeholder="All tenants" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__all__">All tenants</SelectItem>
                {tenants.map((tenant) => (
                  <SelectItem key={tenant.id} value={tenant.id}>{tenant.name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={operationFilter || "__all__"}
              onValueChange={(value) => {
                setOperationFilter(value === "__all__" ? "" : value);
                setOffset(0);
              }}
            >
              <SelectTrigger className="w-52" aria-label="Filter by operation">
                <SelectValue placeholder="All operations" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__all__">All operations</SelectItem>
                <SelectItem value="session_submit">Session submission</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </CardHeader>
        <CardContent>
          {query.isLoading && (
            <p className="py-8 text-center text-sm text-muted-foreground">Loading extension errors…</p>
          )}
          {query.error instanceof Error && (
            <p className="text-sm text-destructive">{query.error.message}</p>
          )}
          {!query.isLoading && !query.error && rows.length === 0 && (
            <div className="py-12 text-center">
              <TriangleAlert className="mx-auto mb-3 size-10 text-muted-foreground/40" />
              <p className="font-medium">No extension errors reported</p>
              <p className="mt-1 text-sm text-muted-foreground">
                New reports will appear here without interrupting the extension user.
              </p>
            </div>
          )}
          {rows.length > 0 && (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Last seen</TableHead>
                    <TableHead>Tenant / user</TableHead>
                    <TableHead>Operation</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Context</TableHead>
                    <TableHead>Extension</TableHead>
                    <TableHead className="text-right">Count</TableHead>
                    <TableHead>Message</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((row) => (
                    <TableRow key={row.id}>
                      <TableCell className="whitespace-nowrap" title={formatDateFull(row.last_seen_at)}>
                        <span className="font-medium">{timeAgo(row.last_seen_at)}</span>
                        <span className="mt-0.5 block text-xs text-muted-foreground">
                          first {timeAgo(row.first_seen_at)}
                        </span>
                      </TableCell>
                      <TableCell className="max-w-48">
                        <span className="block truncate font-medium" title={row.tenant_name || row.tenant_id}>
                          {row.tenant_name || row.tenant_id}
                        </span>
                        <span className="block truncate text-xs text-muted-foreground" title={row.user_name || row.key_id}>
                          {row.user_name || `key ${shortID(row.key_id)}`}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="whitespace-nowrap capitalize">{operationLabel(row.operation)}</span>
                        <code className="mt-0.5 block text-xs text-muted-foreground">{row.error_code}</code>
                      </TableCell>
                      <TableCell>
                        <Badge variant={row.http_status >= 400 ? "destructive" : "warning"}>
                          HTTP {row.http_status || "—"}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-xs">
                        {row.project_id ? (
                          <Link
                            to={`/projects/${encodeURIComponent(row.project_id)}`}
                            className="block text-primary hover:underline"
                            title={row.project_id}
                          >
                            project {shortID(row.project_id)}
                          </Link>
                        ) : (
                          <span className="block text-muted-foreground">no project</span>
                        )}
                        {row.conversation_id && (
                          <Link
                            to={`/sessions/${encodeURIComponent(row.conversation_id)}`}
                            className="block text-primary hover:underline"
                            title={row.conversation_id}
                          >
                            session {shortID(row.conversation_id)}
                          </Link>
                        )}
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-muted-foreground">
                        {row.extension_version || "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono font-medium">
                        {row.occurrence_count.toLocaleString()}
                      </TableCell>
                      <TableCell className="max-w-80">
                        <p className="line-clamp-3 text-sm" title={row.message}>{row.message || "—"}</p>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          {total > PAGE_SIZE && (
            <div className="mt-4 flex items-center justify-between">
              <p className="text-sm text-muted-foreground">
                {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}
              </p>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={offset === 0}
                  onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={offset + PAGE_SIZE >= total}
                  onClick={() => setOffset(offset + PAGE_SIZE)}
                >
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
