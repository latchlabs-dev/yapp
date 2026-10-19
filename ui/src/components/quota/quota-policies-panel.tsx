import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { quotaPolicies as qpApi } from "@/lib/api";
import type { QuotaPolicy } from "@/lib/types";
import { Plus, Trash2, RefreshCw, Pencil, Power, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
  DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";
import { toast } from "sonner";

// Standard limit keys the proxy enforces, shown when adding a new limit.
const KNOWN_LIMITS = [
  "monthly_spend_cents",
  "daily_spend_cents",
  "concurrent_requests",
  "max_input_tokens_per_request",
  "max_output_tokens_per_request",
  "requests_per_minute",
  "tokens_per_minute",
  "total_tokens_per_day",
  "total_tokens_per_month",
];

function prettyLimit(key: string): string {
  return key
    .replace(/_/g, " ")
    .replace(/\bper\b/g, "/")
    .replace(/\s+\/\s+/g, "/");
}

function formatLimitValue(key: string, value: number): string {
  if (key.endsWith("_spend_cents")) {
    return (value / 100).toLocaleString(undefined, { style: "currency", currency: "USD" });
  }
  return value.toLocaleString();
}

interface QuotaFormState {
  scope_type: string;
  scope_id: string;
  priority: number;
  enabled: boolean;
  limits: { key: string; value: number }[];
}

const emptyForm = (tenantId: string): QuotaFormState => ({
  scope_type: "tenant",
  scope_id: tenantId,
  priority: 10,
  enabled: true,
  limits: [
    { key: "monthly_spend_cents", value: 50000 },
    { key: "daily_spend_cents", value: 5000 },
  ],
});

export function QuotaPoliciesPanel({ tenantId }: { tenantId: string }) {
  const qc = useQueryClient();

  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ["quota-policies", tenantId],
    queryFn: () => qpApi.list(tenantId),
    enabled: !!tenantId,
  });

  const policies = data?.quota_policies ?? [];

  // --- Create / edit dialog state ---
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<QuotaPolicy | null>(null);
  const [form, setForm] = useState<QuotaFormState>(emptyForm(tenantId));

  const openCreate = () => {
    setEditing(null);
    setForm(emptyForm(tenantId));
    setDialogOpen(true);
  };

  const openEdit = (q: QuotaPolicy) => {
    setEditing(q);
    setForm({
      scope_type: q.scope_type,
      scope_id: q.scope_id,
      priority: q.priority,
      enabled: q.enabled,
      limits: Object.entries(q.limits ?? {}).map(([key, value]) => ({ key, value })),
    });
    setDialogOpen(true);
  };

  const buildLimits = (): Record<string, number> => {
    const out: Record<string, number> = {};
    for (const { key, value } of form.limits) {
      if (key.trim()) out[key.trim()] = value;
    }
    return out;
  };

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["quota-policies", tenantId] });

  const saveMut = useMutation({
    mutationFn: () => {
      const payload = {
        scope_type: form.scope_type,
        scope_id: form.scope_id,
        priority: form.priority,
        enabled: form.enabled,
        limits: buildLimits(),
      };
      return editing
        ? qpApi.update(tenantId, editing.id, payload)
        : qpApi.create(tenantId, payload);
    },
    onSuccess: () => {
      invalidate();
      setDialogOpen(false);
      toast.success(editing ? "Quota policy updated" : "Quota policy created");
    },
    onError: (err) => toast.error("Failed", { description: (err as Error).message }),
  });

  const toggleMut = useMutation({
    mutationFn: (q: QuotaPolicy) => qpApi.update(tenantId, q.id, { enabled: !q.enabled }),
    onSuccess: () => invalidate(),
    onError: (err) => toast.error("Failed", { description: (err as Error).message }),
  });

  const deleteMut = useMutation({
    mutationFn: (policyId: string) => qpApi.delete(tenantId, policyId),
    onSuccess: () => {
      invalidate();
      toast.success("Quota policy deleted");
    },
    onError: (err) => toast.error("Failed", { description: (err as Error).message }),
  });

  const updateLimit = (i: number, patch: Partial<{ key: string; value: number }>) =>
    setForm((f) => ({
      ...f,
      limits: f.limits.map((l, idx) => (idx === i ? { ...l, ...patch } : l)),
    }));

  const removeLimit = (i: number) =>
    setForm((f) => ({ ...f, limits: f.limits.filter((_, idx) => idx !== i) }));

  const addLimit = () => {
    const used = new Set(form.limits.map((l) => l.key));
    const next = KNOWN_LIMITS.find((k) => !used.has(k)) ?? "";
    setForm((f) => ({ ...f, limits: [...f.limits, { key: next, value: 0 }] }));
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-end gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => refetch()}
          disabled={isFetching}
        >
          <RefreshCw className={cn("h-4 w-4", isFetching && "animate-spin")} />
          Refresh
        </Button>
        <Button size="sm" onClick={openCreate}>
          <Plus className="h-4 w-4" /> Add Policy
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Scope</TableHead>
                <TableHead className="w-20">Priority</TableHead>
                <TableHead>Limits</TableHead>
                <TableHead className="w-24">Status</TableHead>
                <TableHead className="w-32 text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow><TableCell colSpan={5} className="py-10 text-center text-muted-foreground">Loading…</TableCell></TableRow>
              ) : policies.length === 0 ? (
                <TableRow><TableCell colSpan={5} className="py-10 text-center text-muted-foreground">No quota policies. Add one to start enforcing limits.</TableCell></TableRow>
              ) : (
                policies.map((q) => (
                  <TableRow key={q.id} className="data-row">
                    <TableCell className="font-mono text-xs">
                      <span className="text-primary">{q.scope_type}</span>
                      <span className="text-muted-foreground">:</span>
                      {q.scope_id}
                    </TableCell>
                    <TableCell>{q.priority}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1.5">
                        {Object.entries(q.limits ?? {}).length === 0 ? (
                          <span className="text-muted-foreground text-xs">—</span>
                        ) : (
                          Object.entries(q.limits ?? {}).map(([k, v]) => (
                            <Badge key={k} variant="outline" className="font-mono text-[11px]">
                              {prettyLimit(k)} <span className="ml-1 text-primary">{formatLimitValue(k, v)}</span>
                            </Badge>
                          ))
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <Switch
                          checked={q.enabled}
                          onCheckedChange={() => toggleMut.mutate(q)}
                          disabled={toggleMut.isPending}
                        />
                        <span className="text-xs text-muted-foreground">
                          {q.enabled ? "Enabled" : "Disabled"}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon" title="Edit" aria-label="Edit policy" onClick={() => openEdit(q)}>
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          title="Delete"
                          aria-label="Delete policy"
                          onClick={() => deleteMut.mutate(q.id)}
                          disabled={deleteMut.isPending}
                        >
                          <Trash2 className="h-3.5 w-3.5 text-muted-foreground hover:text-destructive" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Create / Edit dialog */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>{editing ? "Edit Quota Policy" : "Create Quota Policy"}</DialogTitle>
            <DialogDescription>
              Set spend budgets first; rate and token controls are optional guardrails.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-2">
            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-2">
                <label className="text-sm font-medium">Scope Type</label>
                <Select value={form.scope_type} onValueChange={(v) => setForm({ ...form, scope_type: v })}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="tenant">tenant</SelectItem>
                    <SelectItem value="user">user</SelectItem>
                    <SelectItem value="api_key">api_key</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Scope ID</label>
                <Input
                  value={form.scope_id}
                  onChange={(e) => setForm({ ...form, scope_id: e.target.value })}
                  placeholder="* for all"
                  className="font-mono text-xs"
                />
              </div>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-2">
                <label className="text-sm font-medium">Priority</label>
                <Input
                  type="number"
                  value={form.priority}
                  onChange={(e) => setForm({ ...form, priority: parseInt(e.target.value) || 0 })}
                />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Status</label>
                <button
                  type="button"
                  onClick={() => setForm({ ...form, enabled: !form.enabled })}
                  className={cn(
                    "flex h-9 w-full items-center justify-center gap-2 rounded-md border text-sm font-medium transition-colors",
                    form.enabled
                      ? "border-success/40 bg-success/10 text-success"
                      : "border-border bg-transparent text-muted-foreground",
                  )}
                >
                  <Power className="h-4 w-4" />
                  {form.enabled ? "Enabled" : "Disabled"}
                </button>
              </div>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <label className="text-sm font-medium">Limits</label>
                <Button type="button" variant="ghost" size="sm" onClick={addLimit}>
                  <Plus className="h-3.5 w-3.5" /> Add limit
                </Button>
              </div>
              <div className="space-y-2">
                {form.limits.length === 0 && (
                  <p className="text-xs text-muted-foreground">No limits — this policy will not restrict usage.</p>
                )}
                {form.limits.map((l, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Select value={l.key} onValueChange={(v) => updateLimit(i, { key: v })}>
                      <SelectTrigger className="flex-1"><SelectValue placeholder="limit" /></SelectTrigger>
                      <SelectContent>
                        {/* Keep current key selectable even if non-standard */}
                        {!KNOWN_LIMITS.includes(l.key) && l.key && (
                          <SelectItem value={l.key}>{l.key}</SelectItem>
                        )}
                        {KNOWN_LIMITS.map((k) => (
                          <SelectItem key={k} value={k}>{k}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {l.key.endsWith("_spend_cents") ? (
                      // Spend limits are stored in cents but edited in dollars.
                      <div className="relative w-36">
                        <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-xs text-muted-foreground">$</span>
                        <Input
                          type="number"
                          step="0.01"
                          min="0"
                          className="pl-5 font-mono text-xs"
                          value={l.value / 100}
                          onChange={(e) => updateLimit(i, { value: Math.round((parseFloat(e.target.value) || 0) * 100) })}
                        />
                      </div>
                    ) : (
                      <Input
                        type="number"
                        className="w-36 font-mono text-xs"
                        value={l.value}
                        onChange={(e) => updateLimit(i, { value: parseInt(e.target.value) || 0 })}
                      />
                    )}
                    <Button type="button" variant="ghost" size="icon" aria-label="Remove limit" onClick={() => removeLimit(i)}>
                      <X className="h-3.5 w-3.5 text-muted-foreground hover:text-destructive" />
                    </Button>
                  </div>
                ))}
              </div>
            </div>
          </div>

          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button onClick={() => saveMut.mutate()} disabled={saveMut.isPending}>
              {saveMut.isPending ? "Saving…" : editing ? "Save Changes" : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
