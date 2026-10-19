import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { providers as providersApi, credentials as credsApi } from "@/lib/api";
import { Plus, Blocks, Trash2, MoreHorizontal, Settings2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { PageContainer, PageHeading } from "@/components/page";
import { useConfirm } from "@/components/confirm";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
  DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { formatDate, timeAgo } from "@/lib/utils";
import { formatProviderBalance, providerBillingLabel } from "@/lib/provider-balance";
import { toast } from "sonner";

type ProviderPreset = {
  value: string;
  label: string;
  protocol: string;
  defaultBaseURL: string;
  apiKeyPlaceholder: string;
  credentialKind?: "api_key" | "oauth";
  oauthTokenURL?: string;
  oauthClientID?: string;
  oauthScope?: string;
};

function catalogPreset(catalog: {
  provider_key: string;
  display_name: string;
  api_type: string;
  base_url: string;
}): ProviderPreset {
  const oauth = catalog.api_type === "openai-codex-responses";
  return {
    value: catalog.provider_key,
    label: catalog.display_name,
    protocol: catalog.api_type,
    defaultBaseURL: catalog.base_url,
    apiKeyPlaceholder: "...",
    ...(oauth
      ? {
          credentialKind: "oauth" as const,
          oauthTokenURL: "https://auth.openai.com/oauth/token",
          oauthClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
        }
      : {}),
  };
}

function defaultForm() {
  return {
    provider: "deepseek",
    name: "",
    base_url: "",
    api_key: "",
    access_token: "",
    refresh_token: "",
    account_id: "",
    expires_at: "",
    oauth_token_url: "",
    oauth_client_id: "",
    oauth_scope: "",
    billing_mode: "pay_as_you_go" as "subscription" | "pay_as_you_go",
  };
}

function isoDateTime(value: string): string | undefined {
  if (!value.trim()) return undefined;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value.trim() : parsed.toISOString();
}

export function ProvidersListPage() {
  const qc = useQueryClient();
  const confirm = useConfirm();
  const [showCreate, setShowCreate] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [form, setForm] = useState(defaultForm);

  const { data, isLoading } = useQuery({
    queryKey: ["provider-accounts"],
    queryFn: () => providersApi.list(),
    refetchInterval: 60_000,
  });
  const { data: catalogData, isLoading: isCatalogLoading } = useQuery({
    queryKey: ["provider-catalogs"],
    queryFn: () => providersApi.listCatalogs(),
  });
  const providerPresets = (catalogData?.provider_catalogs ?? [])
    .map(catalogPreset)
    .sort((a, b) => a.label.localeCompare(b.label, undefined, { sensitivity: "base" }));
  const selProvider = providerPresets.find(p => p.value === form.provider);
  const sortedProviderAccounts = [...(data?.provider_accounts ?? [])].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
  );

  const createMut = useMutation({
    mutationFn: async () => {
      const def = providerPresets.find(p => p.value === form.provider);
      const baseURL = form.base_url || def?.defaultBaseURL || "";
      const displayName = form.name || def?.label || form.provider;
      // Always send the API protocol type, not the Pi provider key,
      // so the backend's providerTypeFromDB maps it to a known handler.
      const providerType = def?.protocol || form.provider;
      const account = await providersApi.create({
        provider: providerType,
        name: displayName,
        config: {
          catalog_namespace: "pi",
          catalog_key: form.provider,
          upstream_base_url: baseURL,
          ...(def?.oauthTokenURL ? { oauth_token_url: def.oauthTokenURL } : {}),
          ...(def?.oauthClientID ? { oauth_client_id: def.oauthClientID } : {}),
          ...(def?.oauthScope ? { oauth_scope: def.oauthScope } : {}),
          billing: {
            mode: form.billing_mode === "subscription" ? "subscription" : "default",
            currency: "USD",
            ...(providerType === "openai-codex-responses" ? { monthly_cost_cents: 20000 } : {}),
          },
          enabled: true,
        },
      });
      if (def?.credentialKind === "oauth") {
        const accessToken = form.access_token.trim();
        const refreshToken = form.refresh_token.trim();
        if (accessToken || refreshToken) {
          await credsApi.create(account.id, {
            access_token: accessToken,
            refresh_token: refreshToken,
            account_id: form.account_id.trim() || undefined,
            oauth_token_url: form.oauth_token_url.trim() || def.oauthTokenURL,
            oauth_client_id: form.oauth_client_id.trim() || def.oauthClientID,
            oauth_scope: form.oauth_scope.trim() || def.oauthScope,
            expires_at: isoDateTime(form.expires_at),
          });
        }
      } else if (form.api_key.trim()) {
        await credsApi.create(account.id, { api_key: form.api_key.trim() });
      }
      return account;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-accounts"] });
      setShowCreate(false);
      setAdvanced(false);
      setForm(defaultForm());
      toast.success("Provider account created");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => providersApi.delete(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-accounts"] });
      toast.success("Provider account deleted");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  return (
    <PageContainer>
      <PageHeading
        title="Provider Accounts"
        description="Manage upstream LLM provider connections"
      >
        <Button onClick={() => setShowCreate(true)}>
          <Plus className="h-4 w-4" />
          New Provider
        </Button>
      </PageHeading>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {data?.provider_accounts?.length ?? 0} Accounts
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Provider</TableHead>
                <TableHead>Billing type</TableHead>
                <TableHead>Available</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Checked</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow>
                  <TableCell colSpan={7} className="text-center py-8 text-muted-foreground">Loading...</TableCell>
                </TableRow>
              ) : data?.provider_accounts?.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="text-center py-8 text-muted-foreground">
                    <Blocks className="mx-auto h-8 w-8 mb-2 opacity-40" />
                    No provider accounts configured
                  </TableCell>
                </TableRow>
              ) : (
                sortedProviderAccounts.map((p) => (
                  <TableRow key={p.id} className="data-row">
                    <TableCell>
                      <Link to={`/providers/${p.id}`} className="font-medium hover:text-primary transition-colors">
                        {p.name}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <Badge variant="info">{p.provider}</Badge>
                    </TableCell>
                    <TableCell>
                      <Badge variant={providerBillingLabel(p) === "Subscription" ? "warning" : "outline"}>
                        {providerBillingLabel(p)}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <span className={p.balance?.status === "error" ? "text-destructive" : "font-medium"}>
                        {formatProviderBalance(p.balance)}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Badge variant={p.status === "active" ? "success" : "destructive"}>{p.status}</Badge>
                    </TableCell>
                    <TableCell className="text-muted-foreground text-sm" title={p.balance?.observed_at ? formatDate(p.balance.observed_at) : undefined}>
                      {p.balance?.observed_at ? timeAgo(p.balance.observed_at) : "—"}
                    </TableCell>
                    <TableCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon" className="h-8 w-8">
                            <MoreHorizontal className="h-4 w-4" />
                            <span className="sr-only">Actions</span>
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" className="min-w-[10rem]">
                          <DropdownMenuItem asChild>
                            <Link to={`/providers/${p.id}`} className="cursor-pointer">
                              <Settings2 className="h-4 w-4" />
                              Manage
                            </Link>
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            className="text-destructive focus:text-destructive"
                            onClick={async () => {
                              if (
                                await confirm({
                                  title: "Delete provider account?",
                                  description:
                                    "This removes the provider account and its credentials. This action cannot be undone.",
                                  confirmLabel: "Delete",
                                  destructive: true,
                                })
                              ) {
                                deleteMut.mutate(p.id);
                              }
                            }}
                          >
                            <Trash2 className="h-4 w-4" />
                            Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog open={showCreate} onOpenChange={(open) => { setShowCreate(open); if (!open) { setAdvanced(false); setForm(defaultForm()); } }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add Provider</DialogTitle>
            <DialogDescription>
              {advanced
                ? "Configure a custom upstream endpoint."
                : "Choose a provider and add its credential."}
            </DialogDescription>
          </DialogHeader>

          {!advanced ? (
            /* ---- Simple mode: provider dropdown + key only ---- */
            <div className="space-y-4 py-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Provider</label>
                <Select
                  value={form.provider}
                  onValueChange={(v) => {
                    const def = providerPresets.find(p => p.value === v);
                    setForm({
                      ...defaultForm(),
                      provider: v,
                      billing_mode: def?.credentialKind === "oauth" ? "subscription" : "pay_as_you_go",
                      base_url: def?.defaultBaseURL ?? "",
                      oauth_token_url: def?.oauthTokenURL ?? "",
                      oauth_client_id: def?.oauthClientID ?? "",
                      oauth_scope: def?.oauthScope ?? "",
                    });
                  }}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {providerPresets.map((t) => (
                      <SelectItem key={t.value} value={t.value}>{t.label}</SelectItem>
                    ))}
                    {!isCatalogLoading && providerPresets.length === 0 && (
                      <SelectItem value="__none" disabled>No supported providers found</SelectItem>
                    )}
                  </SelectContent>
                </Select>
              </div>

              {form.provider && (
                <div className="rounded-md bg-muted/50 px-3 py-2 text-xs text-muted-foreground flex items-center gap-2">
                  <span className="font-mono">{selProvider?.defaultBaseURL}</span>
                  <span className="text-muted-foreground/60">— upstream endpoint</span>
                </div>
              )}

              <div className="space-y-2">
                <label className="text-sm font-medium">Billing type</label>
                <Select value={form.billing_mode} onValueChange={(value) => setForm({ ...form, billing_mode: value as "subscription" | "pay_as_you_go" })}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="subscription">Subscription</SelectItem>
                    <SelectItem value="pay_as_you_go">Pay as you go</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Subscriptions show remaining usage; pay-as-you-go providers show available credit when supported.
                </p>
              </div>

              {selProvider?.credentialKind === "oauth" ? (
                <>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Access Token</label>
                    <Input
                      type="password"
                      value={form.access_token}
                      onChange={(e) => setForm({ ...form, access_token: e.target.value })}
                      className="font-mono"
                      autoFocus
                    />
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Refresh Token</label>
                    <Input
                      type="password"
                      value={form.refresh_token}
                      onChange={(e) => setForm({ ...form, refresh_token: e.target.value })}
                      className="font-mono"
                    />
                  </div>
                </>
              ) : (
                <div className="space-y-2">
                  <label className="text-sm font-medium">API Key</label>
                  <Input
                    type="password"
                    placeholder={selProvider?.apiKeyPlaceholder ?? "sk-..."}
                    value={form.api_key}
                    onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                    className="font-mono"
                    autoFocus
                  />
                </div>
              )}

              <button
                type="button"
                onClick={() => setAdvanced(true)}
                className="text-xs text-muted-foreground hover:text-foreground transition-colors flex items-center gap-1"
              >
                <Settings2 className="h-3 w-3" />
                Advanced settings
              </button>
            </div>
          ) : (
            /* ---- Advanced mode: full form ---- */
            <div className="space-y-4 py-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Provider Type</label>
                <Select
                  value={form.provider}
                  onValueChange={(v) => {
                    const def = providerPresets.find(p => p.value === v);
                    setForm({
                      ...form,
                      provider: v,
                      billing_mode: def?.credentialKind === "oauth" ? "subscription" : "pay_as_you_go",
                      base_url: def?.defaultBaseURL ?? "",
                      oauth_token_url: def?.oauthTokenURL ?? form.oauth_token_url,
                      oauth_client_id: def?.oauthClientID ?? form.oauth_client_id,
                      oauth_scope: def?.oauthScope ?? form.oauth_scope,
                    });
                  }}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {providerPresets.map((t) => (
                      <SelectItem key={t.value} value={t.value}>{t.label}</SelectItem>
                    ))}
                    {!isCatalogLoading && providerPresets.length === 0 && (
                      <SelectItem value="__none" disabled>No supported providers found</SelectItem>
                    )}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Display Name</label>
                <Input placeholder="e.g., OpenAI Prod" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Billing type</label>
                <Select value={form.billing_mode} onValueChange={(value) => setForm({ ...form, billing_mode: value as "subscription" | "pay_as_you_go" })}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="subscription">Subscription</SelectItem>
                    <SelectItem value="pay_as_you_go">Pay as you go</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Upstream Base URL</label>
                <Input placeholder="https://api.deepseek.com" value={form.base_url} onChange={(e) => setForm({ ...form, base_url: e.target.value })} />
              </div>
              {selProvider?.credentialKind === "oauth" ? (
                <>
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Access Token <span className="text-muted-foreground font-normal">(optional)</span></label>
                      <Input type="password" value={form.access_token} onChange={(e) => setForm({ ...form, access_token: e.target.value })} className="font-mono" />
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Refresh Token <span className="text-muted-foreground font-normal">(optional)</span></label>
                      <Input type="password" value={form.refresh_token} onChange={(e) => setForm({ ...form, refresh_token: e.target.value })} className="font-mono" />
                    </div>
                  </div>
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Account ID</label>
                      <Input value={form.account_id} onChange={(e) => setForm({ ...form, account_id: e.target.value })} className="font-mono" />
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium">Expires At</label>
                      <Input type="datetime-local" value={form.expires_at} onChange={(e) => setForm({ ...form, expires_at: e.target.value })} />
                    </div>
                  </div>
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                    <div className="space-y-2">
                      <label className="text-sm font-medium">OAuth Token URL</label>
                      <Input value={form.oauth_token_url} onChange={(e) => setForm({ ...form, oauth_token_url: e.target.value })} className="font-mono" />
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium">OAuth Client ID</label>
                      <Input value={form.oauth_client_id} onChange={(e) => setForm({ ...form, oauth_client_id: e.target.value })} className="font-mono" />
                    </div>
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">OAuth Scope</label>
                    <Input value={form.oauth_scope} onChange={(e) => setForm({ ...form, oauth_scope: e.target.value })} className="font-mono" />
                  </div>
                </>
              ) : (
                <div className="space-y-2">
                  <label className="text-sm font-medium">API Key <span className="text-muted-foreground font-normal">(optional)</span></label>
                  <Input
                    type="password"
                    placeholder={selProvider?.apiKeyPlaceholder ?? "sk-..."}
                    value={form.api_key}
                    onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                    className="font-mono"
                  />
                </div>
              )}

              <button
                type="button"
                onClick={() => setAdvanced(false)}
                className="text-xs text-muted-foreground hover:text-foreground transition-colors"
              >
                ← Simple setup
              </button>
            </div>
          )}

          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button onClick={() => createMut.mutate()} disabled={createMut.isPending}>
              {createMut.isPending ? "Adding..." : "Add Provider"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </PageContainer>
  );
}
