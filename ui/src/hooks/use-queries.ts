import { useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { tenants, users, keys, quotaPolicies, providers, credentials, modelAliases, usage } from "@/lib/api";

// --- Tenants ---
export function useTenants() {
  return useQuery({ queryKey: ["tenants"], queryFn: () => tenants.list() });
}

// Resolves raw tenant IDs (e.g. "tn__01KT...") to their friendly names for
// display. Returns the tenant list plus a `tenantName` helper that falls back
// to the raw ID when a name isn't available yet.
export function useTenantNames() {
  const { data } = useTenants();
  const list = data?.tenants ?? [];
  const byId = useMemo(() => {
    const m = new Map<string, string>();
    for (const t of list) m.set(t.id, t.name);
    return m;
  }, [list]);
  const tenantName = (id?: string) => (id ? byId.get(id) ?? id : "—");
  return { tenants: list, tenantName };
}

export function useTenant(id: string | undefined) {
  return useQuery({
    queryKey: ["tenants", id],
    queryFn: () => tenants.get(id!),
    enabled: !!id,
  });
}

export function useCreateTenant() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: { name: string }) => tenants.create(data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tenants"] }),
  });
}

export function useUpdateTenant() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: Partial<{ name: string; status: string }> }) =>
      tenants.update(id, data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tenants"] }),
  });
}

export function useDeleteTenant() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => tenants.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tenants"] }),
  });
}

// --- Users ---
export function useUsers(tenantId: string | undefined) {
  return useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => users.list(tenantId!),
    enabled: !!tenantId,
  });
}

export function useCreateUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, data }: { tenantId: string; data: { email: string; display_name?: string } }) =>
      users.create(tenantId, data),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["users", vars.tenantId] }),
  });
}

export function useDeleteUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, userId }: { tenantId: string; userId: string }) =>
      users.delete(tenantId, userId),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["users", vars.tenantId] }),
  });
}

// --- Keys ---
export function useKeys(tenantId: string | undefined, userId: string | undefined) {
  return useQuery({
    queryKey: ["keys", tenantId, userId],
    queryFn: () => keys.list(tenantId!, userId!),
    enabled: !!tenantId && !!userId,
  });
}

export function useCreateKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, userId, scopes }: { tenantId: string; userId: string; scopes?: string[] }) =>
      keys.create(tenantId, userId, scopes),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["keys", vars.tenantId, vars.userId] }),
  });
}

export function useRevokeKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, userId, keyId }: { tenantId: string; userId: string; keyId: string }) =>
      keys.revoke(tenantId, userId, keyId),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["keys", vars.tenantId, vars.userId] }),
  });
}

// --- Quota Policies ---
export function useQuotaPolicies(tenantId: string | undefined) {
  return useQuery({
    queryKey: ["quota-policies", tenantId],
    queryFn: () => quotaPolicies.list(tenantId!),
    enabled: !!tenantId,
  });
}

export function useCreateQuotaPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, data }: { tenantId: string; data: Partial<{
      scope_type: string; scope_id: string; priority: number; limits: Record<string, number>; enabled: boolean;
    }> }) => quotaPolicies.create(tenantId, data),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["quota-policies", vars.tenantId] }),
  });
}

export function useDeleteQuotaPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ tenantId, policyId }: { tenantId: string; policyId: string }) =>
      quotaPolicies.delete(tenantId, policyId),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["quota-policies", vars.tenantId] }),
  });
}

// --- Provider Accounts ---
export function useProviderAccounts(tenantId?: string) {
  return useQuery({
    queryKey: ["provider-accounts", tenantId],
    queryFn: () => providers.list(tenantId),
  });
}

export function useProviderAccount(accountId: string | undefined) {
  return useQuery({
    queryKey: ["provider-accounts", accountId],
    queryFn: () => providers.get(accountId!),
    enabled: !!accountId,
  });
}

export function useCreateProviderAccount() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Partial<{ provider: string; name: string; config: Record<string, unknown> }>) =>
      providers.create(data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provider-accounts"] }),
  });
}

export function useDeleteProviderAccount() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (accountId: string) => providers.delete(accountId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provider-accounts"] }),
  });
}

// --- Credentials ---
export function useCredentials(accountId: string | undefined) {
  return useQuery({
    queryKey: ["credentials", accountId],
    queryFn: () => credentials.list(accountId!),
    enabled: !!accountId,
  });
}

export function useCreateCredential() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ accountId, apiKey }: { accountId: string; apiKey: string }) =>
      credentials.create(accountId, { api_key: apiKey }),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["credentials", vars.accountId] }),
  });
}

export function useRevokeCredential() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ accountId, credentialId }: { accountId: string; credentialId: string }) =>
      credentials.revoke(accountId, credentialId),
    onSuccess: (_, vars) => qc.invalidateQueries({ queryKey: ["credentials", vars.accountId] }),
  });
}

// --- Model Aliases ---
export function useModelAliases(tenantId?: string) {
  const qk = tenantId ? ["model-aliases", tenantId] : ["model-aliases", "global"];
  return useQuery({
    queryKey: qk,
    queryFn: () => tenantId ? modelAliases.listTenant(tenantId) : modelAliases.listGlobal(),
  });
}

// --- Usage Reports ---
export function useUsageReports(params: Parameters<typeof usage.reports>[0]) {
  return useQuery({
    queryKey: ["usage-reports", params],
    queryFn: () => usage.reports(params),
  });
}
