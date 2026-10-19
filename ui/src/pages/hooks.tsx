import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Play, ShieldAlert, Trash2, Webhook } from "lucide-react";
import { toast } from "sonner";
import { hooks } from "@/lib/api";
import type { HookSubscription } from "@/lib/types";
import { PageContainer, PageHeading } from "@/components/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

const emptyForm = {
  name: "",
  event_type: "user_message.pre_send",
  url: "",
  secret: "",
  tenant_id: "",
  mode: "async" as "async" | "blocking",
  fail_mode: "open" as "open" | "closed",
  include_content: false,
  allow_mutation: false,
  timeout_ms: 1500,
  priority: 100,
};

/**
 * HooksPage registers the endpoints that observe and govern model turns.
 *
 * Registration is admin-only on purpose. `include_content` turns a metrics
 * webhook into a channel that ships user prompts to a third-party URL, and
 * `allow_mutation` lets that endpoint rewrite what the model is asked and what
 * the user is shown. Neither is something a tenant should be able to grant
 * itself, so the form states plainly what each one does.
 */
export function HooksPage() {
  const queryClient = useQueryClient();
  const [form, setForm] = useState({ ...emptyForm });
  const [selected, setSelected] = useState<string | undefined>();

  const eventTypes = useQuery({ queryKey: ["hook-event-types"], queryFn: () => hooks.eventTypes() });
  const subscriptions = useQuery({ queryKey: ["hook-subscriptions"], queryFn: () => hooks.list() });
  const deliveries = useQuery({
    queryKey: ["hook-deliveries", selected],
    queryFn: () => hooks.deliveries(selected),
  });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["hook-subscriptions"] });
    void queryClient.invalidateQueries({ queryKey: ["hook-deliveries"] });
  };

  const create = useMutation({
    mutationFn: () => hooks.upsert(form),
    onSuccess: () => {
      toast.success(`${form.name} registered`);
      setForm({ ...emptyForm });
      invalidate();
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const toggle = useMutation({
    mutationFn: (sub: HookSubscription) => hooks.update(sub.id, { ...sub, enabled: !sub.enabled }),
    onSuccess: invalidate,
    onError: (err: Error) => toast.error(err.message),
  });

  const remove = useMutation({
    mutationFn: (id: string) => hooks.delete(id),
    onSuccess: () => {
      toast.success("Subscription removed");
      invalidate();
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const probe = useMutation({
    mutationFn: (id: string) => hooks.test(id),
    onSuccess: (res) =>
      res.delivered
        ? toast.success(`Endpoint answered in ${res.latency_ms} ms`)
        : toast.error(res.error ?? "Endpoint did not answer"),
    onError: (err: Error) => toast.error(err.message),
  });

  // An async hook is delivered after the turn is already served, so there is
  // nothing left for it to mutate or refuse. Mirror the server's rule in the
  // form rather than letting a user build something that will be rejected.
  const asyncMode = form.mode === "async";

  return (
    <PageContainer>
      <PageHeading
        title="Hook Subscriptions"
        description="Endpoints that observe — and can govern — every model turn, from the CLI and the web console alike."
      />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Webhook className="h-4 w-4" />
            Register an endpoint
          </CardTitle>
          <CardDescription>
            Blocking hooks share one total latency budget across every hook on a request, not one
            each. Leave a hook on <span className="font-mono">fail open</span> unless its whole
            purpose is to stop something leaving.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <div className="space-y-1.5">
              <Label htmlFor="hook-name">Name</Label>
              <Input
                id="hook-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="dlp-guardrail"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hook-event">Event</Label>
              <Select
                value={form.event_type}
                onValueChange={(value) => setForm({ ...form, event_type: value })}
              >
                <SelectTrigger id="hook-event">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(eventTypes.data?.event_types ?? []).map((type) => (
                    <SelectItem key={type} value={type} className="font-mono text-xs">
                      {type}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hook-tenant">Tenant (blank = all)</Label>
              <Input
                id="hook-tenant"
                value={form.tenant_id}
                onChange={(e) => setForm({ ...form, tenant_id: e.target.value })}
                placeholder="tnt_…"
                className="font-mono text-sm"
              />
            </div>
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="hook-url">HTTPS endpoint</Label>
              <Input
                id="hook-url"
                value={form.url}
                onChange={(e) => setForm({ ...form, url: e.target.value })}
                placeholder="https://guardrail.example.com/hook"
                className="font-mono text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hook-secret">Signing secret</Label>
              <Input
                id="hook-secret"
                type="password"
                value={form.secret}
                onChange={(e) => setForm({ ...form, secret: e.target.value })}
                placeholder="optional but recommended"
                className="font-mono text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hook-mode">Mode</Label>
              <Select
                value={form.mode}
                onValueChange={(value) =>
                  setForm({
                    ...form,
                    mode: value as "async" | "blocking",
                    // Switching to async drops grants it cannot honour.
                    ...(value === "async"
                      ? { allow_mutation: false, fail_mode: "open" as const }
                      : {}),
                  })
                }
              >
                <SelectTrigger id="hook-mode">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="async">async — observe only</SelectItem>
                  <SelectItem value="blocking">blocking — can refuse or rewrite</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="hook-timeout">Timeout (ms)</Label>
              <Input
                id="hook-timeout"
                type="number"
                value={form.timeout_ms}
                onChange={(e) => setForm({ ...form, timeout_ms: Number(e.target.value) })}
                className="font-mono text-sm"
              />
            </div>
          </div>

          <div className="space-y-3 rounded-md border border-warning/30 bg-warning/5 p-3">
            <div className="flex items-center gap-2 text-sm font-medium text-warning">
              <ShieldAlert className="h-4 w-4" />
              Grants
            </div>
            <label className="flex items-start gap-3 text-sm">
              <Switch
                checked={form.include_content}
                onCheckedChange={(checked) =>
                  setForm({ ...form, include_content: checked, allow_mutation: checked && form.allow_mutation })
                }
              />
              <span>
                <span className="font-medium">Send content</span>
                <span className="block text-xs text-muted-foreground">
                  Ships the prompt — and on post-send the reply — to this endpoint. Without it the
                  endpoint receives metadata only.
                </span>
              </span>
            </label>
            <label className="flex items-start gap-3 text-sm">
              <Switch
                checked={form.allow_mutation}
                disabled={asyncMode || !form.include_content}
                onCheckedChange={(checked) => setForm({ ...form, allow_mutation: checked })}
              />
              <span>
                <span className="font-medium">Allow rewriting</span>
                <span className="block text-xs text-muted-foreground">
                  Lets this endpoint change the prompt the model receives, or the reply the user
                  reads. Requires blocking mode and content. A rewriting post-send hook stops the
                  console streaming: the reply arrives all at once.
                </span>
              </span>
            </label>
            <label className="flex items-start gap-3 text-sm">
              <Switch
                checked={form.fail_mode === "closed"}
                disabled={asyncMode}
                onCheckedChange={(checked) => setForm({ ...form, fail_mode: checked ? "closed" : "open" })}
              />
              <span>
                <span className="font-medium">Fail closed</span>
                <span className="block text-xs text-muted-foreground">
                  Refuse the turn when this endpoint cannot be reached. Off by default: a broken
                  endpoint should not take model access down for a whole tenant.
                </span>
              </span>
            </label>
          </div>

          <Button
            disabled={!form.name.trim() || !form.url.trim() || create.isPending}
            onClick={() => create.mutate()}
          >
            Register hook
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Registered hooks</CardTitle>
        </CardHeader>
        <CardContent>
          {subscriptions.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (subscriptions.data?.subscriptions ?? []).length === 0 ? (
            <p className="py-4 text-sm text-muted-foreground">No hooks registered.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>Mode</TableHead>
                  <TableHead>Grants</TableHead>
                  <TableHead>Enabled</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {subscriptions.data!.subscriptions!.map((sub) => (
                  <TableRow
                    key={sub.id}
                    onClick={() => setSelected(sub.id)}
                    className="cursor-pointer"
                  >
                    <TableCell>
                      <div className="text-sm">{sub.name}</div>
                      <div className="max-w-[22rem] truncate font-mono text-[11px] text-muted-foreground">
                        {sub.url}
                      </div>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{sub.event_type}</TableCell>
                    <TableCell>
                      <Badge variant={sub.mode === "blocking" ? "warning" : "outline"}>
                        {sub.mode}
                      </Badge>
                      {sub.fail_mode === "closed" && (
                        <Badge variant="destructive" className="ml-1">
                          fail closed
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell className="space-x-1">
                      {sub.include_content && <Badge variant="warning">content</Badge>}
                      {sub.allow_mutation && <Badge variant="destructive">rewrites</Badge>}
                      {!sub.include_content && (
                        <span className="text-xs text-muted-foreground">metadata only</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <Switch checked={sub.enabled} onCheckedChange={() => toggle.mutate(sub)} />
                    </TableCell>
                    <TableCell className="space-x-1 text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={(e) => {
                          e.stopPropagation();
                          probe.mutate(sub.id);
                        }}
                        aria-label={`Test ${sub.name}`}
                      >
                        <Play className="h-3.5 w-3.5" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={(e) => {
                          e.stopPropagation();
                          remove.mutate(sub.id);
                        }}
                        aria-label={`Delete ${sub.name}`}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Recent deliveries{selected ? " — selected hook" : ""}
          </CardTitle>
          <CardDescription>
            Latency here is latency added to a real request. A blocking hook that consistently takes
            hundreds of milliseconds is costing every matching turn that much.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {deliveries.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (deliveries.data?.deliveries ?? []).length === 0 ? (
            <p className="py-4 text-sm text-muted-foreground">No deliveries recorded.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Latency</TableHead>
                  <TableHead>Error</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {deliveries.data!.deliveries!.map((d) => (
                  <TableRow key={d.id}>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {new Date(d.created_at).toLocaleString()}
                    </TableCell>
                    <TableCell className="font-mono text-xs">{d.event_type}</TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          d.status === "ok"
                            ? "success"
                            : d.status === "blocked"
                              ? "warning"
                              : "destructive"
                        }
                      >
                        {d.status}
                      </Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs tabular-nums">
                      {d.latency_ms ? `${d.latency_ms} ms` : "—"}
                    </TableCell>
                    <TableCell className="max-w-[24rem] truncate text-xs text-muted-foreground">
                      {d.error ?? "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}
