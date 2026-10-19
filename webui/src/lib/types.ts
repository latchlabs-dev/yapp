// Shapes returned by /web/v1. These mirror the Go structs in
// internal/httpapi/webui; keep them in step when the API changes.

export interface AuthConfig {
  enabled: boolean;
  provider_name: string;
  api_key_enabled: boolean;
  oidc_enabled: boolean;
}

export interface Me {
  user: { id: string; email: string; display_name: string };
  tenant: { id: string; name?: string };
  scopes: string[];
  csrf_token: string;
  expires_at: string;
  attachments: { enabled: boolean; max_size_bytes: number };
}

export interface Project {
  id: string;
  slug: string;
  display_name: string;
  repo: string;
}

export interface Conversation {
  id: string;
  project_id?: string;
  title: string;
  model: string;
  provider_id: string;
  created_at: string;
  updated_at: string;
}

/** One block of a stored message. Attachments are references, not bytes. */
export interface MessageBlock {
  type: "text" | "attachment";
  text?: string;
  attachment_id?: string;
  filename?: string;
  mime_type?: string;
  kind?: "image" | "text" | "pdf";
}

export interface Message {
  id: string;
  conversation_id: string;
  seq: number;
  role: "user" | "assistant";
  content: MessageBlock[];
  model?: string;
  input_tokens: number;
  output_tokens: number;
  cost_total_cents: number;
  annotations?: Record<string, string>;
  created_at: string;
}

/** A model as offered by the tenant-aware catalog. */
export interface Model {
  id: string;
  name?: string;
  provider_id: string;
  provider?: string;
  /** Modalities the model accepts; "image" gates image attachments. */
  input?: string[];
  reasoning?: boolean;
  context_window?: number;
}

/**
 * One model entry in the Pi-extension catalog response. The console consumes
 * the same catalog the CLI/extension does, so models arrive grouped under
 * providers and are flattened client-side by `flattenModels`.
 */
export interface CatalogModel {
  id: string;
  name?: string;
  reasoning?: boolean;
  input?: string[];
  context_window?: number;
  max_tokens?: number;
}

/** One provider entry in the Pi-extension catalog response. */
export interface CatalogProvider {
  name: string;
  base_url?: string;
  api_type?: string;
  /** Present when the provider has a stable route id; absent for built-ins. */
  route_id?: string;
  models?: CatalogModel[];
}

/** Response from GET /web/v1/models. */
export interface ProvidersResponse {
  providers?: CatalogProvider[];
}

export interface Attachment {
  id: string;
  filename: string;
  mime_type: string;
  kind: "image" | "text" | "pdf";
  size_bytes: number;
  status: "pending" | "ready";
  created_at: string;
}

export interface UploadTicket {
  id: string;
  upload_url: string;
  method: string;
  headers: Record<string, string>;
  expires_in: number;
}

export interface ProjectMemory {
  memory: string;
  memory_bytes: number;
  memory_tokens: number;
  lessons: { Timestamp: string; Content: string }[];
  degraded: boolean;
  degraded_reason?: string;
}

export interface BundlePreview {
  bundle: string;
  token_estimate: number;
  degraded: boolean;
  degraded_reason?: string;
  cached: boolean;
}

/**
 * One UTC quota period for one scope, as returned by the shared quota handler
 * (internal/httpapi/quotausage). Nullable fields are null exactly when the
 * period is unlimited: there is no remaining amount to report when nothing
 * constrains it.
 */
export interface QuotaPeriod {
  used_millicents: number;
  used_cents: number;
  used_percent: number | null;
  limit_cents: number;
  remaining_millicents: number | null;
  remaining_cents: number | null;
  remaining_percent: number | null;
  resets_at: string;
  unlimited: boolean;
  exhausted: boolean;
}

/** Balances for one of the scopes that constrains the caller: key, user, tenant. */
export interface QuotaScope {
  scope_type: string;
  scope_id: string;
  daily: QuotaPeriod;
  monthly: QuotaPeriod;
}

/** The tightest remaining balance across every applicable scope. */
export interface QuotaEffective extends QuotaPeriod {
  binding_scope_type?: string;
  binding_scope_id?: string;
}

/** Response from GET /web/v1/quota. */
export interface QuotaResponse {
  currency: string;
  as_of: string;
  daily: QuotaEffective;
  monthly: QuotaEffective;
  scopes?: QuotaScope[] | null;
}
