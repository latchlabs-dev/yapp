import type { ProviderBalanceHistory } from "@/lib/types";
import type {
  Tenant,
  User,
  ApiKey,
  ApiKeyCreateResponse,
  QuotaPolicy,
  ProviderAccount,
  ProviderCatalog,
  SubscriptionRateCard,
  ProviderCredential,
  ProviderCredentialCreate,
  TenantModelSettings,
  ModelAlias,
  ModelCatalogResponse,
  UsageReportsResponse,
  TenantSpendResponse,
  OnboardingContent,
  ConversationPayloadsListResponse,
  ConversationSessionsListResponse,
  ConversationPayload,
  SessionSubmissionsListResponse,
  SessionArchiveResponse,
  SessionTranscriptPage,
  ExtensionErrorsListResponse,
  MemoryBundlePreview,
  MemoryProjectDetail,
  MemoryProjectSummary,
  MemoryRule,
  MemorySearchResult,
  WebIdentityDomain,
  WebIdentity,
  HookSubscription,
  HookDelivery,
  RepositoryProjectDetail,
  RepositoryProjectSummary,
  ProjectAttributionCoverage,
  InsightProcessor,
  InsightConfiguration,
  InsightTag,
  InsightRun,
} from "./types";
import { getStoredAdminKey, notifyAuthExpired } from "./auth-session";

const BASE = "/api";

function getAdminKey(): string {
  return getStoredAdminKey() ?? "";
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  const key = getAdminKey();
  if (key) {
    headers["X-Admin-Key"] = key;
  }

  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });

  if (!res.ok) {
    const errBody = await res.json().catch(() => ({ error: { type: "unknown", message: res.statusText } }));
    if (res.status === 401 || res.status === 403) {
      notifyAuthExpired();
    }
    throw new ApiRequestError(res.status, errBody.error?.type || "unknown", errBody.error?.message || res.statusText);
  }

  return res.json();
}

export class ApiRequestError extends Error {
  constructor(
    public status: number,
    public type: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiRequestError";
  }
}

// --- Tenants ---
export const tenants = {
  list: () => request<{ tenants: Tenant[] | null }>("GET", "/tenants"),
  get: (id: string) => request<Tenant>("GET", `/tenants/${id}`),
  create: (data: { name: string }) => request<Tenant>("POST", "/tenants", data),
  update: (id: string, data: Partial<Tenant>) =>
    request<Tenant>("PUT", `/tenants/${id}`, data),
  delete: (id: string) => request<{ status: string }>("DELETE", `/tenants/${id}`),
};

// --- Users ---
export const users = {
  list: (tenantId: string) =>
    request<{ users: User[] | null }>("GET", `/tenants/${tenantId}/users`),
  get: (tenantId: string, userId: string) =>
    request<User>("GET", `/tenants/${tenantId}/users/${userId}`),
  create: (tenantId: string, data: { email: string; display_name?: string }) =>
    request<User>("POST", `/tenants/${tenantId}/users`, data),
  update: (tenantId: string, userId: string, data: Partial<User>) =>
    request<User>("PUT", `/tenants/${tenantId}/users/${userId}`, data),
  delete: (tenantId: string, userId: string) =>
    request<{ status: string }>("DELETE", `/tenants/${tenantId}/users/${userId}`),
};

// --- Keys ---
export const keys = {
  list: (tenantId: string, userId: string) =>
    request<{ keys: ApiKey[] | null }>("GET", `/tenants/${tenantId}/users/${userId}/keys`),
  create: (tenantId: string, userId: string, scopes?: string[]) =>
    request<ApiKeyCreateResponse>("POST", `/tenants/${tenantId}/users/${userId}/keys`, { scopes }),
  revoke: (tenantId: string, userId: string, keyId: string) =>
    request<{ status: string; key_id: string }>(
      "PUT",
      `/tenants/${tenantId}/users/${userId}/keys/${keyId}/revoke`,
    ),
};

// --- Quota Policies ---
export const quotaPolicies = {
  list: (tenantId: string) =>
    request<{ quota_policies: QuotaPolicy[] | null }>("GET", `/tenants/${tenantId}/quota-policies`),
  get: (tenantId: string, policyId: string) =>
    request<QuotaPolicy>("GET", `/tenants/${tenantId}/quota-policies/${policyId}`),
  create: (tenantId: string, data: Partial<QuotaPolicy>) =>
    request<QuotaPolicy>("POST", `/tenants/${tenantId}/quota-policies`, data),
  update: (tenantId: string, policyId: string, data: Partial<QuotaPolicy>) =>
    request<QuotaPolicy>("PUT", `/tenants/${tenantId}/quota-policies/${policyId}`, data),
  delete: (tenantId: string, policyId: string) =>
    request<{ status: string }>("DELETE", `/tenants/${tenantId}/quota-policies/${policyId}`),
};

// --- Live spend (enforcement counters) ---
export const spend = {
  tenant: (tenantId: string) =>
    request<TenantSpendResponse>("GET", `/tenants/${tenantId}/spend`),
};

// --- Tenant model settings ---
export const tenantModelSettings = {
  get: (tenantId: string) =>
    request<TenantModelSettings>("GET", `/tenants/${tenantId}/model-settings`),
  update: (tenantId: string, data: Pick<TenantModelSettings, "mode" | "model_ids">) =>
    request<TenantModelSettings>("PUT", `/tenants/${tenantId}/model-settings`, data),
};

export const modelCatalog = {
  get: () => request<ModelCatalogResponse>("GET", "/model-catalog"),
};

// --- Providers ---
export const providers = {
  listCatalogs: () =>
    request<{ provider_catalogs: ProviderCatalog[] }>("GET", "/provider-catalogs"),
  list: (tenantId?: string) =>
    request<{ provider_accounts: ProviderAccount[] | null }>(
      "GET",
      `/provider-accounts${tenantId ? `?tenant_id=${tenantId}` : ""}`,
    ),
  get: (accountId: string) =>
    request<ProviderAccount>("GET", `/provider-accounts/${accountId}`),
  create: (data: Partial<ProviderAccount>) =>
    request<ProviderAccount>("POST", `/provider-accounts`, data),
  update: (accountId: string, data: Partial<ProviderAccount>) =>
    request<ProviderAccount>("PUT", `/provider-accounts/${accountId}`, data),
  refreshBalance: (accountId: string) =>
    request<ProviderAccount["balance"]>("POST", `/provider-accounts/${accountId}/balance/refresh`),
  balanceHistory: (accountId: string, limit = 96, range?: { from: string; to: string }) =>
    request<ProviderBalanceHistory>(
      "GET",
      `/provider-accounts/${accountId}/balance/history?${new URLSearchParams({ limit: String(limit), ...range })}`,
    ),
  balanceSnapshot: (accountId: string, snapshotId: number) =>
    request<NonNullable<ProviderAccount["balance"]>>(
      "GET",
      `/provider-accounts/${accountId}/balance/history/${snapshotId}`,
    ),
  subscriptionRateCard: (accountId: string, date?: string) =>
    request<{ rate_cards: SubscriptionRateCard[] }>(
      "GET",
      `/provider-accounts/${accountId}/subscription-rate-card${date ? `?date=${date}` : ""}`,
    ),
  billingPeriods: (accountId: string) => request<{ billing_periods: Array<{
    id: number; starts_at: string; ends_at: string; amount_millicents: number;
    allocated_millicents: number; unattributed_millicents: number; consumed_points: number;
    observed_through?: string; revision: number;
  }> }>("GET", `/provider-accounts/${accountId}/billing-periods`),
  createBillingPeriod: (accountId: string, data: { starts_at: string; ends_at: string; amount_millicents: number }) =>
    request<{ status: string }>("POST", `/provider-accounts/${accountId}/billing-periods`, data),
  delete: (accountId: string) =>
    request<{ status: string }>("DELETE", `/provider-accounts/${accountId}`),
};

// --- Credentials ---
export const credentials = {
  list: (accountId: string) =>
    request<{ credentials: ProviderCredential[] | null }>(
      "GET",
      `/provider-accounts/${accountId}/credentials`,
    ),
  create: (accountId: string, data: ProviderCredentialCreate) =>
    request<ProviderCredential>(
      "POST",
      `/provider-accounts/${accountId}/credentials`,
      data,
    ),
  revoke: (accountId: string, credentialId: string) =>
    request<{ status: string }>(
      "PUT",
      `/provider-accounts/${accountId}/credentials/${credentialId}/revoke`,
    ),
};

// --- Model Aliases ---
export const modelAliases = {
  listGlobal: () =>
    request<{ model_aliases: ModelAlias[] | null }>("GET", "/model-aliases"),
  listAll: () =>
    request<{ model_aliases: ModelAlias[] | null }>("GET", "/model-aliases?scope=all"),
  listTenant: (tenantId: string) =>
    request<{ model_aliases: ModelAlias[] | null }>(
      "GET",
      `/tenants/${tenantId}/model-aliases`,
    ),
  get: (aliasId: string) => request<ModelAlias>("GET", `/tenants/_/model-aliases/${aliasId}`),
  createGlobal: (data: Partial<ModelAlias>) =>
    request<ModelAlias>("POST", "/model-aliases", data),
  createTenant: (tenantId: string, data: Partial<ModelAlias>) =>
    request<ModelAlias>("POST", `/tenants/${tenantId}/model-aliases`, data),
  delete: (aliasId: string) =>
    request<{ status: string }>("DELETE", `/model-aliases/${aliasId}`),
};

// --- Usage ---
export const usage = {
  reports: (params: {
    tenant_id?: string;
    user_id?: string;
    model?: string;
    granularity?: string;
    from?: string;
    to?: string;
    limit?: number;
  }) => {
    const qs = new URLSearchParams();
    Object.entries(params).forEach(([k, v]) => {
      if (v !== undefined && v !== "") qs.set(k, String(v));
    });
    return request<UsageReportsResponse>("GET", `/usage-reports?${qs.toString()}`);
  },
};

// --- Onboarding ---
export const onboarding = {
  // Regenerates the Pi onboarding snippet for an existing key.
  // Route is tenant-scoped (the server's onboardingHandler is mounted at
  // /tenants/{tenantID}/onboarding), and requires PROXY_PUBLIC_BASE_URL.
  generate: (tenantId: string, key: string) =>
    request<OnboardingContent>("POST", `/tenants/${tenantId}/onboarding`, { key }),
};

// --- Conversation Payloads ---
export const conversationPayloads = {
  list: (params: { session_scope?: string; request_id?: string; conversation_id?: string; tenant_id?: string; user_id?: string; provider?: string; limit?: number; offset?: number; include_prompts?: boolean }) => {
    const qs = new URLSearchParams();
    Object.entries(params).forEach(([k, v]) => {
      if (v !== undefined && v !== "") qs.set(k, String(v));
    });
    return request<ConversationPayloadsListResponse>("GET", `/conversation-payloads?${qs.toString()}`);
  },
  get: (requestId: string) =>
    request<ConversationPayload>("GET", `/conversation-payloads/${encodeURIComponent(requestId)}`),

  // Session-level listing — groups payloads by conversation_id, paginates at session level.
  sessions: {
    list: (params: { tenant_id?: string; user_id?: string; provider?: string; limit?: number; offset?: number }) => {
      const qs = new URLSearchParams({ group_by: "session" });
      Object.entries(params).forEach(([k, v]) => {
        if (v !== undefined && v !== "") qs.set(k, String(v));
      });
      return request<ConversationSessionsListResponse>("GET", `/conversation-payloads?${qs.toString()}`);
    },
  },
};

// --- Session Submissions (uploaded Pi sessions + distillation state) ---
export const sessionSubmissions = {
  list: (params: { tenant_id?: string; limit?: number; offset?: number }) => {
    const qs = new URLSearchParams();
    Object.entries(params).forEach(([k, v]) => {
      if (v !== undefined && v !== "") qs.set(k, String(v));
    });
    return request<SessionSubmissionsListResponse>("GET", `/session-submissions?${qs.toString()}`);
  },
};

// --- Session archive (header from Postgres, transcript pages from object storage) ---
function sessionArchiveQuery(params: Record<string, string | number | undefined>): string {
  const qs = new URLSearchParams();
  Object.entries(params).forEach(([k, v]) => {
    // An empty tenant_id is meaningful: it selects unauthenticated captures.
    if (v !== undefined) qs.set(k, String(v));
  });
  const encoded = qs.toString();
  return encoded ? `?${encoded}` : "";
}

export const sessionArchive = {
  get: (conversationId: string, params: { tenant_id?: string } = {}) =>
    request<SessionArchiveResponse>("GET", `/sessions/${encodeURIComponent(conversationId)}${sessionArchiveQuery(params)}`),
  transcript: (conversationId: string, params: { tenant_id?: string; from_seq?: number; limit?: number }) =>
    request<SessionTranscriptPage>(
      "GET",
      `/sessions/${encodeURIComponent(conversationId)}/transcript${sessionArchiveQuery(params)}`,
    ),
  rebuild: (conversationId: string, params: { tenant_id?: string } = {}) =>
    request<{ tenant_id: string; rebuilding: boolean; already_running: boolean }>(
      "POST",
      `/sessions/${encodeURIComponent(conversationId)}/rebuild${sessionArchiveQuery(params)}`,
    ),
};

// --- External insight generation ---
export const insightProcessors = {
  list: () => request<{ insight_processors: InsightProcessor[] | null }>("GET", "/insight-processors"),
  create: (data: Partial<InsightProcessor>) => request<InsightProcessor>("POST", "/insight-processors", data),
  update: (id: string, data: Partial<InsightProcessor>) => request<InsightProcessor>("PUT", `/insight-processors/${encodeURIComponent(id)}`, data),
  disable: (id: string) => request<{ status: string }>("DELETE", `/insight-processors/${encodeURIComponent(id)}`),
  test: (id: string) => request<{ status: string; service: string; version: string }>("POST", `/insight-processors/${encodeURIComponent(id)}/test`),
};

export const insightConfigurations = {
  list: () => request<{ insight_configurations: InsightConfiguration[] | null }>("GET", "/insight-configurations"),
  create: (data: Partial<InsightConfiguration>) => request<InsightConfiguration>("POST", "/insight-configurations", data),
  update: (id: string, data: Partial<InsightConfiguration>) => request<InsightConfiguration>("PUT", `/insight-configurations/${encodeURIComponent(id)}`, data),
  disable: (id: string) => request<{ status: string }>("DELETE", `/insight-configurations/${encodeURIComponent(id)}`),
};

export const insightTags = {
  list: () => request<{ insight_tags: InsightTag[] | null }>("GET", "/insight-tags"),
  upsert: (data: Pick<InsightTag, "name" | "description"> & Partial<Pick<InsightTag, "status">>) => request<InsightTag>("POST", "/insight-tags", data),
  disable: (name: string) => request<{ status: string }>("DELETE", `/insight-tags/${encodeURIComponent(name)}`),
};

export const insightRuns = {
  list: (limit = 50) => request<{ insight_runs: InsightRun[] | null }>("GET", `/insight-runs?limit=${limit}`),
};

// --- Extension error monitoring ---
export const extensionErrors = {
  list: (params: { tenant_id?: string; operation?: string; limit?: number; offset?: number }) => {
    const qs = new URLSearchParams();
    Object.entries(params).forEach(([key, value]) => {
      if (value !== undefined && value !== "") qs.set(key, String(value));
    });
    return request<ExtensionErrorsListResponse>("GET", `/extension-errors?${qs.toString()}`);
  },
};

// --- Repository-backed projects ---
export const projects = {
  list: () =>
    request<{
	  projects: RepositoryProjectSummary[] | null;
	  attribution_coverage: ProjectAttributionCoverage;
	}>("GET", "/projects"),
  get: (projectId: string) =>
    request<RepositoryProjectDetail>("GET", `/projects/${encodeURIComponent(projectId)}`),
};

// --- Project Memory (duffel-backed knowledge browser) ---
export const memoryProjects = {
  list: () => request<{ projects: MemoryProjectSummary[] | null }>("GET", "/memory/projects"),
  get: (project: string) =>
    request<MemoryProjectDetail>("GET", `/memory/projects/${encodeURIComponent(project)}`),
  bundle: (project: string, hint?: string) => {
    const qs = hint && hint.trim() ? `?hint=${encodeURIComponent(hint.trim())}` : "";
    return request<MemoryBundlePreview>(
      "GET",
      `/memory/projects/${encodeURIComponent(project)}/bundle${qs}`,
    );
  },
  saveMemory: (project: string, content: string) =>
    request<{ status: string }>("PUT", `/memory/projects/${encodeURIComponent(project)}/memory`, { content }),
  appendLesson: (project: string, content: string) =>
    request<{ status: string }>("POST", `/memory/projects/${encodeURIComponent(project)}/lessons`, { content }),
  archiveLessons: (project: string) =>
    request<{ status: string }>("POST", `/memory/projects/${encodeURIComponent(project)}/lessons/archive`),
  archive: (project: string) =>
    request<{ status: string; project: string; archived_path: string }>(
      "POST",
      `/memory/projects/${encodeURIComponent(project)}/archive`,
    ),
};

// --- Memory semantic search (embedded duffel index, across all projects) ---
export const memorySearch = {
  query: (q: string, limit = 10) =>
    request<{ query: string; results: MemorySearchResult[] | null }>(
      "GET",
      `/memory/search?q=${encodeURIComponent(q)}&limit=${limit}`,
    ),
};

// --- Memory Injection Rules (when context is injected) ---
export const memoryRules = {
  list: () => request<{ memory_rules: MemoryRule[] | null }>("GET", "/memory-rules"),
  upsert: (data: Partial<MemoryRule> & { enabled: boolean }) =>
    request<{ memory_rule: MemoryRule }>("POST", "/memory-rules", data),
  delete: (ruleId: string) =>
    request<{ status: string }>("DELETE", `/memory-rules/${encodeURIComponent(ruleId)}`),
};

// --- Web console access control ---
// Nobody can sign into the public console until a domain is mapped to a tenant
// here: an email address alone cannot identify one, and login refuses rather
// than guessing.
export const webAccess = {
  domains: () =>
    request<{ domains: WebIdentityDomain[] | null; default_scopes: string[] }>("GET", "/web-access/domains"),
  upsertDomain: (data: Partial<WebIdentityDomain>) =>
    request<WebIdentityDomain>("POST", "/web-access/domains", data),
  deleteDomain: (domain: string) =>
    request<{ status: string; note: string }>("DELETE", `/web-access/domains/${encodeURIComponent(domain)}`),
  identities: (tenantId?: string) =>
    request<{ identities: WebIdentity[] | null }>(
      "GET",
      tenantId ? `/web-access/identities?tenant_id=${encodeURIComponent(tenantId)}` : "/web-access/identities",
    ),
  revokeSessions: (userId: string) =>
    request<{ status: string }>("POST", `/web-access/users/${encodeURIComponent(userId)}/revoke-sessions`),
};

// --- Hook subscriptions ---
export const hooks = {
  eventTypes: () => request<{ event_types: string[] }>("GET", "/hook-event-types"),
  list: () => request<{ subscriptions: HookSubscription[] | null }>("GET", "/hook-subscriptions"),
  upsert: (data: Partial<HookSubscription> & { secret?: string }) =>
    request<HookSubscription>("POST", "/hook-subscriptions", data),
  update: (id: string, data: Partial<HookSubscription> & { secret?: string }) =>
    request<HookSubscription>("PUT", `/hook-subscriptions/${encodeURIComponent(id)}`, data),
  delete: (id: string) =>
    request<{ status: string }>("DELETE", `/hook-subscriptions/${encodeURIComponent(id)}`),
  test: (id: string) =>
    request<{ delivered: boolean; latency_ms: number; decision?: unknown; error?: string }>(
      "POST",
      `/hook-subscriptions/${encodeURIComponent(id)}/test`,
    ),
  deliveries: (subscriptionId?: string) =>
    request<{ deliveries: HookDelivery[] | null }>(
      "GET",
      subscriptionId
        ? `/hook-deliveries?subscription_id=${encodeURIComponent(subscriptionId)}`
        : "/hook-deliveries",
    ),
};
