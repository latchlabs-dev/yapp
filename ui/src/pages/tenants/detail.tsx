import { useEffect, useMemo, useState } from "react";
import { useParams, Link, useSearchParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  tenants as tenantsApi,
  users as usersApi,
  keys as keysApi,
  usage as usageApi,
  quotaPolicies as qpApi,
  conversationPayloads as payloadsApi,
  tenantModelSettings as modelSettingsApi,
  modelCatalog as modelCatalogApi,
} from "@/lib/api";
import { useKeys, useRevokeKey } from "@/hooks/use-queries";
import type { ApiKey, OnboardingContent, TenantModelMode, User } from "@/lib/types";
import {
  Plus, Trash2, KeyRound, Copy, Check, ArrowLeft, Ban,
  Gauge, Users, FileCode, FileJson, Terminal, BarChart3, SlidersHorizontal,
} from "lucide-react";
import { UsagePanel } from "@/components/usage-panel";
import { Button } from "@/components/ui/button";
import { PageContainer } from "@/components/page";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { QuotaPoliciesPanel } from "@/components/quota/quota-policies-panel";
import { useConfirm } from "@/components/confirm";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
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
import { Separator } from "@/components/ui/separator";
import { formatDate, copyToClipboard, monthWindow } from "@/lib/utils";
import { toast } from "sonner";

const TABS = ["users", "keys", "quotas", "models", "analytics"] as const;

// Revoked keys older than this are hidden from the keys tab (they remain in
// the DB and the admin API; this is presentation only).
const REVOKED_KEY_HIDE_AFTER_MS = 30 * 24 * 60 * 60 * 1000;

function isHiddenRevokedKey(k: ApiKey): boolean {
  if (k.status !== "revoked" || !k.revoked_at) return false;
  return Date.now() - new Date(k.revoked_at).getTime() > REVOKED_KEY_HIDE_AFTER_MS;
}


// Lists a single user's API keys (the keys endpoint is scoped per-user) with a
// revoke action. Rendered once per user in the "API Keys" tab.
function UserKeysCard({
  tenantId,
  user,
  onCreate,
}: {
  tenantId: string;
  user: User;
  onCreate: () => void;
}) {
  const { data, isLoading } = useKeys(tenantId, user.id);
  const revokeMut = useRevokeKey();
  const allKeys = data?.keys ?? [];
  const keyList = allKeys.filter((k) => !isHiddenRevokedKey(k));
  const hiddenCount = allKeys.length - keyList.length;

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 py-3">
        <div>
          <CardTitle className="text-sm font-medium">{user.email}</CardTitle>
          {user.display_name && (
            <p className="text-xs text-muted-foreground">{user.display_name}</p>
          )}
        </div>
        <Button size="sm" variant="outline" onClick={onCreate}>
          <Plus className="h-4 w-4" /> New Key
        </Button>
      </CardHeader>
      <CardContent className="p-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Prefix</TableHead>
              <TableHead>Scopes</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-12"></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              <TableRow><TableCell colSpan={5} className="text-center py-6 text-muted-foreground text-sm">Loading…</TableCell></TableRow>
            ) : keyList.length === 0 ? (
              <TableRow><TableCell colSpan={5} className="text-center py-6 text-muted-foreground text-sm">No keys yet</TableCell></TableRow>
            ) : (
              keyList.map((k) => (
                <TableRow key={k.id} className="data-row">
                  <TableCell className="font-mono text-xs">{k.prefix}…</TableCell>
                  <TableCell className="text-muted-foreground text-xs">{k.scopes?.join(", ") || "—"}</TableCell>
                  <TableCell><Badge variant={k.status === "active" ? "success" : "destructive"}>{k.status}</Badge></TableCell>
                  <TableCell className="text-muted-foreground text-sm">{formatDate(k.created_at)}</TableCell>
                  <TableCell>
                    {k.status === "active" && (
                      <Button
                        variant="ghost"
                        size="icon"
                        title="Revoke key"
                        aria-label="Revoke key"
                        disabled={revokeMut.isPending}
                        onClick={() =>
                          revokeMut.mutate(
                            { tenantId, userId: user.id, keyId: k.id },
                            {
                              onSuccess: () => toast.success("Key revoked"),
                              onError: (err) => toast.error("Failed", { description: (err as Error).message }),
                            },
                          )
                        }
                      >
                        <Ban className="h-3.5 w-3.5 text-muted-foreground hover:text-destructive" />
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
        {hiddenCount > 0 && (
          <p className="px-4 py-2 text-xs text-muted-foreground">
            {hiddenCount} revoked {hiddenCount === 1 ? "key" : "keys"} hidden
            (older than 30 days)
          </p>
        )}
      </CardContent>
    </Card>
  );
}

export function TenantDetailPage() {
  const { tenantId } = useParams<{ tenantId: string }>();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const currentMonth = useMemo(monthWindow, []);

  // Drive the active tab from the URL (?tab=) so each section is shareable.
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab");
  const tab = TABS.includes(tabParam as (typeof TABS)[number]) ? tabParam! : "users";
  const setTab = (value: string) =>
    setSearchParams({ tab: value }, { replace: true });

  const { data: tenant } = useQuery({
    queryKey: ["tenants", tenantId],
    queryFn: () => tenantsApi.get(tenantId!),
    enabled: !!tenantId,
  });

  const updateDistillationMut = useMutation({
    mutationFn: (enabled: boolean) => tenantsApi.update(tenantId!, {
      session_distillation_enabled: enabled,
    }),
    onSuccess: (updated) => {
      qc.setQueryData(["tenants", tenantId], updated);
      toast.success(updated.session_distillation_enabled
        ? "Session distillation enabled"
        : "Session distillation disabled");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const toggleSessionDistillation = async () => {
    const enabling = !tenant?.session_distillation_enabled;
    if (enabling) {
      const ok = await confirm({
        title: "Enable session distillation?",
        description: "Captured session transcripts for this tenant will be sent to the configured distiller LLM to extract shared project lessons.",
        confirmLabel: "Enable distillation",
      });
      if (!ok) return;
    }
    updateDistillationMut.mutate(enabling);
  };

  const { data: usersData } = useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => usersApi.list(tenantId!),
    enabled: !!tenantId,
  });
  const userList = usersData?.users ?? [];

  // Analytics tab — lazy-load when the tab is first activated.
  const analyticsEnabled = tab === "analytics" && !!tenantId;

  const usageQ = useQuery({
    queryKey: ["usage-reports", { tenant_id: tenantId, granularity: "daily" }],
    queryFn: () => usageApi.reports({ tenant_id: tenantId, granularity: "daily" }),
    enabled: analyticsEnabled,
    staleTime: 60_000,
  });

  const projectionQ = useQuery({
    queryKey: ["usage-reports", {
      tenant_id: tenantId,
      granularity: "daily",
      from: currentMonth.startISO,
      scope: "projection",
    }],
    queryFn: () => usageApi.reports({
      tenant_id: tenantId,
      granularity: "daily",
      from: currentMonth.startISO,
    }),
    enabled: analyticsEnabled,
    staleTime: 60_000,
  });

  const analyticsPayloadsQ = useQuery({
    queryKey: ["conversation-payloads-latency", tenantId],
    queryFn: () => payloadsApi.list({ tenant_id: tenantId, limit: 500 }),
    enabled: analyticsEnabled,
    staleTime: 60_000,
  });

  const analyticsPoliciesQ = useQuery({
    queryKey: ["quota-policies", tenantId],
    queryFn: () => qpApi.list(tenantId!),
    enabled: analyticsEnabled,
  });

  const tenantMonthlyBudget = useMemo(() => {
    const policy = (analyticsPoliciesQ.data?.quota_policies ?? []).find(
      (p) => p.scope_type === "tenant" && (p.scope_id === tenantId || p.scope_id === "*"),
    );
    return policy?.limits?.monthly_spend_cents as number | undefined;
  }, [analyticsPoliciesQ.data, tenantId]);

  const modelsEnabled = tab === "models" && !!tenantId;
  const modelSettingsQ = useQuery({
    queryKey: ["tenant-model-settings", tenantId],
    queryFn: () => modelSettingsApi.get(tenantId!),
    enabled: modelsEnabled,
  });
  const modelCatalogQ = useQuery({
    queryKey: ["model-catalog"],
    queryFn: () => modelCatalogApi.get(),
    enabled: modelsEnabled,
    staleTime: 60_000,
  });
  const [modelMode, setModelMode] = useState<TenantModelMode>("blacklist");
  const [selectedModels, setSelectedModels] = useState<string[]>([]);

  useEffect(() => {
    if (!modelSettingsQ.data) return;
    setModelMode(modelSettingsQ.data.mode);
    setSelectedModels(modelSettingsQ.data.model_ids ?? []);
  }, [modelSettingsQ.data]);

  const availableModels = useMemo(() => {
    return (modelCatalogQ.data?.providers ?? []).flatMap((provider) =>
      provider.models.map((model) => ({
        id: model.id,
        name: model.name,
        provider: provider.name,
        apiType: provider.api_type,
      })),
    );
  }, [modelCatalogQ.data]);

  const toggleTenantModel = (modelID: string, checked: boolean) => {
    setSelectedModels((current) => {
      if (checked) {
        return current.includes(modelID) ? current : [...current, modelID].sort();
      }
      return current.filter((id) => id !== modelID);
    });
  };

  const updateModelsMut = useMutation({
    mutationFn: () => modelSettingsApi.update(tenantId!, {
      mode: modelMode,
      model_ids: selectedModels,
    }),
    onSuccess: (data) => {
      qc.setQueryData(["tenant-model-settings", tenantId], data);
      toast.success("Model settings saved");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  // Create user
  const [showCreateUser, setShowCreateUser] = useState(false);
  const [newUserEmail, setNewUserEmail] = useState("");
  const [newUserDisplay, setNewUserDisplay] = useState("");

  const createUserMut = useMutation({
    mutationFn: () => usersApi.create(tenantId!, { email: newUserEmail, display_name: newUserDisplay }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["users", tenantId] });
      setShowCreateUser(false);
      setNewUserEmail("");
      setNewUserDisplay("");
      toast.success("User created");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  const deleteUserMut = useMutation({
    mutationFn: (userId: string) => usersApi.delete(tenantId!, userId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["users", tenantId] });
      toast.success("User deleted");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  // Create key
  const [showCreateKey, setShowCreateKey] = useState(false);
  const [keyUserId, setKeyUserId] = useState("");
  const [newKeyResult, setNewKeyResult] = useState<{
    key: string;
    prefix: string;
    onboarding?: OnboardingContent;
  } | null>(null);
  const [keyCopied, setKeyCopied] = useState(false);
  const [onboardingTab, setOnboardingTab] = useState("install");
  const [onboardingCopied, setOnboardingCopied] = useState(false);
  const [sessionCapabilities, setSessionCapabilities] = useState({
    logs: false,
    context: false,
    observe: false,
  });

  const openCreateKey = (userId: string) => {
    setKeyUserId(userId);
    setNewKeyResult(null);
    setSessionCapabilities({ logs: false, context: false, observe: false });
    setShowCreateKey(true);
  };

  const confirmDeleteUser = async (user: User) => {
    const ok = await confirm({
      title: "Delete user?",
      description: `Archive ${user.email} and revoke their API keys. Historical usage remains available.`,
      confirmLabel: "Delete user",
      destructive: true,
    });
    if (ok) {
      deleteUserMut.mutate(user.id);
    }
  };

  const createKeyMut = useMutation({
    mutationFn: ({ tenantId, userId, scopes }: { tenantId: string; userId: string; scopes: string[] }) =>
      keysApi.create(tenantId, userId, scopes),
    onSuccess: (data, vars) => {
      setNewKeyResult({
        key: data.key,
        prefix: data.prefix,
        onboarding: data.onboarding,
      });
      setOnboardingTab("install");
      // Refresh the key list so the newly created key shows up immediately.
      qc.invalidateQueries({ queryKey: ["keys", vars.tenantId, vars.userId] });
      toast.success("Key created — copy it now, it won't be shown again");
    },
    onError: (err) => toast.error("Failed", { description: err.message }),
  });

  // Helper to copy onboarding for a specific tab
  const getOnboardingText = (tab: string): string => {
    if (!newKeyResult?.onboarding) return "";
    switch (tab) {
      case "install": return newKeyResult.onboarding.install;
      case "extension": return newKeyResult.onboarding.extension;
      case "models_json": return newKeyResult.onboarding.models_json;
      case "curl_test": return newKeyResult.onboarding.curl_test;
      default: return "";
    }
  };

  return (
    <PageContainer>
      <div className="flex items-center gap-3">
        <Link to="/tenants" className="text-muted-foreground hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          <span className="sr-only">Back to tenants</span>
        </Link>
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{tenant?.name || "Tenant"}</h1>
          {tenant?.id && (
            <p className="text-xs font-mono text-muted-foreground">{tenant.id}</p>
          )}
        </div>
        <div className="ml-auto flex items-center gap-2">
          {tenant?.status && (
            <Badge variant={tenant.status === "active" ? "success" : "default"}>
              {tenant.status}
            </Badge>
          )}
          {tenant && (
            <Button
              variant="outline"
              size="sm"
              disabled={updateDistillationMut.isPending}
              onClick={toggleSessionDistillation}
            >
              Session distillation: {tenant.session_distillation_enabled ? "on" : "off"}
            </Button>
          )}
        </div>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="users"><Users className="h-4 w-4 mr-1" /> Users</TabsTrigger>
          <TabsTrigger value="keys"><KeyRound className="h-4 w-4 mr-1" /> API Keys</TabsTrigger>
          <TabsTrigger value="quotas"><Gauge className="h-4 w-4 mr-1" /> Quota Policies</TabsTrigger>
          <TabsTrigger value="models"><SlidersHorizontal className="h-4 w-4 mr-1" /> Models</TabsTrigger>
          <TabsTrigger value="analytics"><BarChart3 className="h-4 w-4 mr-1" /> Analytics</TabsTrigger>
        </TabsList>

        {/* Users Tab */}
        <TabsContent value="users" className="space-y-4 mt-4">
          <div className="flex justify-end">
            <Button size="sm" onClick={() => setShowCreateUser(true)}>
              <Plus className="h-4 w-4" /> Add User
            </Button>
          </div>
          <Card>
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Email</TableHead>
                    <TableHead>Display Name</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Created</TableHead>
                    <TableHead className="w-20"></TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {userList.length === 0 ? (
                    <TableRow><TableCell colSpan={5} className="text-center py-8 text-muted-foreground">No users</TableCell></TableRow>
                  ) : (
                    userList.map((u) => (
                      <TableRow key={u.id} className="data-row">
                        <TableCell className="font-medium">{u.email}</TableCell>
                        <TableCell className="text-muted-foreground">{u.display_name || "—"}</TableCell>
                        <TableCell><Badge variant={u.status === "active" ? "success" : "destructive"}>{u.status}</Badge></TableCell>
                        <TableCell className="text-muted-foreground text-sm">{formatDate(u.created_at)}</TableCell>
                        <TableCell>
                          <div className="flex gap-1">
                            <Button variant="ghost" size="icon" title="Create key" aria-label="Create key" onClick={() => openCreateKey(u.id)}>
                              <KeyRound className="h-3.5 w-3.5" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              title="Delete user"
                              aria-label="Delete user"
                              onClick={() => void confirmDeleteUser(u)}
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
        </TabsContent>

        {/* API Keys Tab — one card per user, each listing real keys. */}
        <TabsContent value="keys" className="space-y-4 mt-4">
          {userList.length === 0 ? (
            <Card>
              <CardContent className="py-8 text-center text-muted-foreground">
                Add a user first, then create API keys for them.
              </CardContent>
            </Card>
          ) : (
            userList.map((u) => (
              <UserKeysCard
                key={u.id}
                tenantId={tenantId!}
                user={u}
                onCreate={() => openCreateKey(u.id)}
              />
            ))
          )}
        </TabsContent>

        {/* Quota Policies Tab */}
        <TabsContent value="quotas" className="mt-4">
          <QuotaPoliciesPanel tenantId={tenantId!} />
        </TabsContent>

        <TabsContent value="models" className="space-y-4 mt-4">
          <Card>
            <CardHeader className="flex flex-row items-start justify-between gap-4 space-y-0">
              <div>
                <CardTitle className="text-base">Tenant Models</CardTitle>
                <p className="text-sm text-muted-foreground mt-1">
                  {modelMode === "whitelist"
                    ? "Only selected models are advertised and accepted."
                    : "Selected models are hidden and rejected."}
                </p>
              </div>
              <Button
                size="sm"
                onClick={() => updateModelsMut.mutate()}
                disabled={updateModelsMut.isPending || modelSettingsQ.isLoading}
              >
                {updateModelsMut.isPending ? "Saving..." : "Save"}
              </Button>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="max-w-xs space-y-2">
                <label className="text-sm font-medium">Mode</label>
                <Select value={modelMode} onValueChange={(value) => setModelMode(value as TenantModelMode)}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="blacklist">Blacklist selected models</SelectItem>
                    <SelectItem value="whitelist">Whitelist selected models</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              <div className="rounded-md border">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-12"></TableHead>
                      <TableHead>Model</TableHead>
                      <TableHead>Provider</TableHead>
                      <TableHead>Protocol</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {modelCatalogQ.isLoading || modelSettingsQ.isLoading ? (
                      <TableRow><TableCell colSpan={4} className="text-center py-6 text-muted-foreground text-sm">Loading…</TableCell></TableRow>
                    ) : availableModels.length === 0 ? (
                      <TableRow><TableCell colSpan={4} className="text-center py-6 text-muted-foreground text-sm">No active provider models</TableCell></TableRow>
                    ) : (
                      availableModels.map((model) => (
                        <TableRow key={`${model.provider}:${model.id}`} className="data-row">
                          <TableCell>
                            <Checkbox
                              checked={selectedModels.includes(model.id)}
                              onCheckedChange={(checked) => toggleTenantModel(model.id, checked === true)}
                              aria-label={`${modelMode === "whitelist" ? "Allow" : "Block"} ${model.id}`}
                            />
                          </TableCell>
                          <TableCell>
                            <div className="font-medium">{model.name}</div>
                            <div className="font-mono text-xs text-muted-foreground">{model.id}</div>
                          </TableCell>
                          <TableCell>{model.provider}</TableCell>
                          <TableCell className="font-mono text-xs text-muted-foreground">{model.apiType}</TableCell>
                        </TableRow>
                      ))
                    )}
                  </TableBody>
                </Table>
              </div>
            </CardContent>
          </Card>
        </TabsContent>

        {/* Analytics Tab */}
        <TabsContent value="analytics" className="mt-4">
          {usageQ.isLoading ? (
            <p className="text-sm text-muted-foreground">Loading analytics…</p>
          ) : (usageQ.data?.usage_reports ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">No usage data yet for this tenant.</p>
          ) : (
            <UsagePanel
              reports={usageQ.data?.usage_reports ?? []}
              projectionReports={projectionQ.data?.usage_reports ?? usageQ.data?.usage_reports ?? []}
              payloads={analyticsPayloadsQ.data?.conversation_payloads ?? []}
              monthlyBudgetCents={tenantMonthlyBudget}
            />
          )}
        </TabsContent>
      </Tabs>

      {/* Create User Dialog */}
      <Dialog open={showCreateUser} onOpenChange={setShowCreateUser}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create User</DialogTitle>
            <DialogDescription>Add a new user to this tenant.</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Email *</label>
              <Input placeholder="user@example.com" value={newUserEmail} onChange={(e) => setNewUserEmail(e.target.value)} />
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium">Display Name</label>
              <Input placeholder="Jane Doe" value={newUserDisplay} onChange={(e) => setNewUserDisplay(e.target.value)} />
            </div>
          </div>
          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button onClick={() => createUserMut.mutate()} disabled={!newUserEmail.trim() || createUserMut.isPending}>
              {createUserMut.isPending ? "Creating..." : "Create"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Create Key Dialog */}
      <Dialog open={showCreateKey} onOpenChange={(o) => { setShowCreateKey(o); if (!o) setNewKeyResult(null); }}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>Create API Key</DialogTitle>
            <DialogDescription>
              {newKeyResult ? "Copy the key and onboarding instructions now. They will not be shown again." : "Generate a new API key for this user."}
            </DialogDescription>
          </DialogHeader>
          {newKeyResult ? (
            <div className="space-y-4 py-2 max-h-[60vh] overflow-y-auto">
              {/* API Key */}
              <div className="space-y-2">
                <label className="text-sm font-medium">API Key</label>
                <div className="flex gap-2">
                  <Input value={newKeyResult.key} readOnly className="font-mono text-xs" />
                  <Button size="icon" variant="outline" aria-label={keyCopied ? "Copied" : "Copy API key"} onClick={async () => {
                    try {
                      await copyToClipboard(newKeyResult.key);
                      setKeyCopied(true);
                      setTimeout(() => setKeyCopied(false), 2000);
                      toast.success("Copied to clipboard");
                    } catch (err) {
                      toast.error("Copy failed", { description: err instanceof Error ? err.message : "Select and copy the text manually." });
                    }
                  }}>
                    {keyCopied ? <Check className="h-4 w-4 text-success" /> : <Copy className="h-4 w-4" />}
                  </Button>
                </div>
              </div>

              {newKeyResult.onboarding && (
                <>
                  <Separator />

                  <div className="space-y-2">
                    <div className="flex items-center justify-between">
                      <label className="text-sm font-medium">Pi Onboarding Instructions</label>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={async () => {
                          const text = getOnboardingText(onboardingTab);
                          try {
                            await copyToClipboard(text);
                            setOnboardingCopied(true);
                            setTimeout(() => setOnboardingCopied(false), 2000);
                            toast.success("Copied to clipboard");
                          } catch (err) {
                            toast.error("Copy failed", { description: err instanceof Error ? err.message : "Select and copy the text manually." });
                          }
                        }}
                      >
                        {onboardingCopied ? (
                          <><Check className="h-3.5 w-3.5 mr-1 text-success" /> Copied</>
                        ) : (
                          <><Copy className="h-3.5 w-3.5 mr-1" /> Copy</>
                        )}
                      </Button>
                    </div>

                    <Tabs value={onboardingTab} onValueChange={setOnboardingTab}>
                      <TabsList className="grid grid-cols-4">
                        <TabsTrigger value="install">
                          <Terminal className="h-3.5 w-3.5 mr-1" /> Install
                        </TabsTrigger>
                        <TabsTrigger value="extension">
                          <FileCode className="h-3.5 w-3.5 mr-1" /> Extension
                        </TabsTrigger>
                        <TabsTrigger value="models_json">
                          <FileJson className="h-3.5 w-3.5 mr-1" /> models.json
                        </TabsTrigger>
                        <TabsTrigger value="curl_test">
                          <Terminal className="h-3.5 w-3.5 mr-1" /> Test
                        </TabsTrigger>
                      </TabsList>
                      <TabsContent value="install" className="mt-2">
                        <div className="relative">
                          <pre className="text-xs bg-muted p-3 rounded-md overflow-x-auto whitespace-pre-wrap font-mono max-h-64 overflow-y-auto">
                            {newKeyResult.onboarding?.install}
                          </pre>
                        </div>
                      </TabsContent>
                      <TabsContent value="extension" className="mt-2">
                        <div className="relative">
                          <pre className="text-xs bg-muted p-3 rounded-md overflow-x-auto whitespace-pre-wrap font-mono max-h-64 overflow-y-auto">
                            {newKeyResult.onboarding?.extension}
                          </pre>
                        </div>
                      </TabsContent>
                      <TabsContent value="models_json" className="mt-2">
                        <div className="relative">
                          <pre className="text-xs bg-muted p-3 rounded-md overflow-x-auto whitespace-pre-wrap font-mono max-h-64 overflow-y-auto">
                            {newKeyResult.onboarding?.models_json}
                          </pre>
                        </div>
                      </TabsContent>
                      <TabsContent value="curl_test" className="mt-2">
                        <div className="relative">
                          <pre className="text-xs bg-muted p-3 rounded-md overflow-x-auto whitespace-pre-wrap font-mono max-h-64 overflow-y-auto">
                            {newKeyResult.onboarding?.curl_test}
                          </pre>
                        </div>
                      </TabsContent>
                    </Tabs>
                  </div>
                </>
              )}

              <DialogFooter>
                <DialogClose asChild><Button>Done</Button></DialogClose>
              </DialogFooter>
            </div>
          ) : (
            <div className="space-y-4 py-4">
              <p className="text-sm text-muted-foreground">Inference and lightweight session metadata are enabled by default. Raw logs and memory access remain explicit capabilities.</p>
			  <div className="rounded-md border bg-muted/30 p-3">
				<span className="block text-sm font-medium text-foreground">Session metadata included</span>
				<span className="block text-xs text-muted-foreground">Reports session identifiers and usage summaries for project activity; it does not upload raw transcript JSONL.</span>
			  </div>
              <div className="space-y-3 rounded-md border p-3">
                {([
                  ["logs", "Upload raw session JSONL", "Allows full local transcripts and tool results; the client must also opt in locally."],
                  ["context", "Request project context", "Allows the extension to call the public context-policy endpoint."],
                  ["observe", "Write project observations", "Allows agents to append tenant-scoped shared lessons."],
                ] as const).map(([key, label, description]) => (
                  <label key={key} className="flex items-start gap-3 text-left">
                    <Checkbox
                      checked={sessionCapabilities[key]}
                      onCheckedChange={(checked) => setSessionCapabilities((current) => ({ ...current, [key]: checked === true }))}
                    />
                    <span>
                      <span className="block text-sm font-medium text-foreground">{label}</span>
                      <span className="block text-xs text-muted-foreground">{description}</span>
                    </span>
                  </label>
                ))}
              </div>
              <Button
                onClick={() => createKeyMut.mutate({
                  tenantId: tenantId!,
                  userId: keyUserId,
                  scopes: [
                    "llm:invoke",
					"sessions:submit",
                    ...(sessionCapabilities.logs ? ["sessions:logs:write"] : []),
                    ...(sessionCapabilities.context ? ["memory:context:read"] : []),
                    ...(sessionCapabilities.observe ? ["memory:observe"] : []),
                  ],
                })}
                disabled={createKeyMut.isPending}
              >
                {createKeyMut.isPending ? "Generating..." : "Generate Key"}
              </Button>
            </div>
          )}
        </DialogContent>
      </Dialog>

    </PageContainer>
  );
}
