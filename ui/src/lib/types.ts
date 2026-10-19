export interface Tenant {
  id: string;
  name: string;
  status: string;
  session_distillation_enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface InsightProcessor {
  id: string;
  name: string;
  grpc_target: string;
  plaintext: boolean;
  tls_server_name?: string;
  timeout_ms: number;
  status: "active" | "disabled";
  created_at: string;
  updated_at: string;
}

export interface InsightConfiguration {
  id: string;
  processor_id: string;
  name: string;
  tenant_id?: string;
  project_id?: string;
  instructions: string;
  allow_new_tags: boolean;
  include_session_summary: boolean;
  include_insight_summaries: boolean;
  max_input_items: number;
  max_input_chars: number;
  revision: number;
  status: "active" | "disabled";
  processor?: InsightProcessor;
  created_at: string;
  updated_at: string;
}

export interface InsightTag {
  name: string;
  description: string;
  usage_count: number;
  status: "active" | "disabled";
  created_at: string;
  updated_at: string;
}

export interface InsightRun {
  id: string;
  configuration_id: string;
  configuration_name?: string;
  configuration_revision: number;
  session_submission_id: string;
  tenant_id: string;
  conversation_id: string;
  project_id: string;
  input_sha256: string;
  status: "running" | "completed" | "failed";
  session_summary: string;
  usage: Record<string, unknown>;
  warnings: string[];
  error?: string;
  insight_count: number;
  started_at: string;
  completed_at?: string;
}

export interface User {
  id: string;
  tenant_id: string;
  email: string;
  display_name?: string;
  status: string;
  created_at: string;
  updated_at: string;
}

export interface ApiKey {
  id: string;
  tenant_id: string;
  user_id: string;
  prefix: string;
  scopes: string[];
  status: string;
  expires_at?: string;
  revoked_at?: string | null;
  created_at: string;
}

// Extend the create response to include optional onboarding content.
export interface ApiKeyCreateResponse {
  key: string;
  prefix: string;
  key_id: string;
  tenant_id: string;
  user_id: string;
  scopes: string[];
  onboarding?: OnboardingContent;
}

export interface QuotaPolicy {
  id: string;
  tenant_id: string;
  scope_type: string;
  scope_id: string;
  priority: number;
  // Go marshals a nil map as JSON null, so wire collections are nullable.
  limits: Record<string, number> | null;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface ProviderAccount {
  id: string;
  tenant_id?: string;
  provider: string;
  name: string;
  status: string;
  config: Record<string, unknown>;
  balance?: ProviderBalanceSnapshot;
  created_at: string;
  updated_at: string;
}

export interface ProviderBalanceWindow {
  name: string;
  limit?: number;
  used?: number;
  remaining?: number;
  used_percent: number;
  remaining_percent: number;
  window_seconds?: number;
  resets_at?: string;
}

export interface ProviderBalanceSnapshot {
  id?: number;
  provider_account_id: string;
  billing_mode: "subscription" | "pay_as_you_go";
  status: "success" | "error";
  balance_kind?: "monetary" | "usage";
  currency?: string;
  unit?: string;
  amount_limit?: number;
  amount_used?: number;
  amount_spent?: number;
  amount_remaining?: number;
  usage_windows?: ProviderBalanceWindow[];
  normalization_version: number;
  normalization_error?: string;
  raw_payload?: unknown;
  raw_headers?: Record<string, string[]>;
  source?: string;
  error_code?: string;
  error_message?: string;
  observed_at: string;
}

export interface ProviderResetEvent {
  window_name: string;
  window_seconds: number;
  kind: "scheduled" | "early";
  occurred_at: string;
  observed_at: string;
  previous_observed_at: string;
  previous_used_percent: number;
  used_percent: number;
  previous_resets_at?: string;
  resets_at?: string;
}

export interface ProviderBalanceHistory {
  provider_balances: ProviderBalanceSnapshot[];
  reset_events: ProviderResetEvent[];
  truncated: boolean;
}

export interface SubscriptionRateCard {
  id: number;
  provider_account_id: string;
  rate_date: string;
  model: string;
  currency: string;
  monthly_cost_cents: number;
  accrued_cost_cents: number;
  source_period_start: string;
  source_period_end: string;
  source_tokens: number;
  source_usage_percent: number;
  source_usage_window?: string;
  source_usage_window_seconds?: number;
  source_usage_window_resets_at?: string;
  usd_per_million_tokens: number;
  calculation_method: "cycle_to_date" | "utilization_delta" | "previous_rate" | "configured_fallback";
  calculated_at: string;
}

export interface ProviderCatalog {
  namespace: string;
  provider_key: string;
  display_name: string;
  api_type: string;
  base_url: string;
  public_base_path: string;
  public_api_base_path: string;
  metadata: Record<string, unknown>;
  source_version: string;
}

export interface ProviderCredential {
  id: string;
  provider_account_id: string;
  version: number;
  status: string;
  secret_ref?: string;
  fingerprint: string;
  valid_from?: string;
  valid_until?: string;
  last_used_at?: string;
  created_by: string;
  created_at: string;
}

export interface ProviderCredentialCreate {
  api_key?: string;
  access_token?: string;
  refresh_token?: string;
  account_id?: string;
  oauth_token_url?: string;
  oauth_client_id?: string;
  oauth_scope?: string;
  expires_at?: string;
  created_by?: string;
}

export type TenantModelMode = "blacklist" | "whitelist";

export interface TenantModelSettings {
  tenant_id: string;
  mode: TenantModelMode;
  model_ids: string[];
  created_at?: string;
  updated_at?: string;
}

export interface PiModelConfig {
  id: string;
  name: string;
  reasoning: boolean;
  input: string[];
  context_window: number;
  max_tokens: number;
  compat?: Record<string, unknown>;
  cost?: Record<string, number>;
}

export interface PiProviderConfig {
  name: string;
  base_url: string;
  api_type: string;
  route_id?: string;
  models: PiModelConfig[];
}

export interface ModelCatalogResponse {
  providers: PiProviderConfig[];
}

export interface ModelAlias {
  id: string;
  tenant_id?: string;
  public_provider: string;
  public_model: string;
  upstream_provider: string;
  upstream_model: string;
  provider_account_id?: string;
  api_type: string;
  capabilities?: Record<string, unknown>;
  cost?: Record<string, unknown>;
  limits?: Record<string, unknown>;
  status: string;
  created_at: string;
  updated_at: string;
}

export interface UsageReport {
  period_start: string;
  period_end: string;
  granularity: string;
  tenant_id?: string;
  user_id?: string;
  user_display_name?: string;
  user_email?: string;
  project_id?: string;
  project_name?: string;
  model?: string;
  request_count: number;
  input_tokens: number;
  output_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  cost_total_cents: number;
}

export interface UsageReportsResponse {
  usage_reports: UsageReport[] | null;
  from: string;
  to: string;
  granularity: string;
}

export interface ScopeSpend {
  scope_type: string;
  scope_id: string;
  monthly_spend_cents: number;
  daily_spend_cents: number;
  monthly_limit_cents: number;
  daily_limit_cents: number;
}

export interface TenantSpendResponse {
  month: string;
  redis_available: boolean;
  scopes: ScopeSpend[] | null;
}

export interface ApiError {
  error: {
    type: string;
    message: string;
  };
}

export interface OnboardingContent {
  install: string;
  extension: string;
  models_json: string;
  curl_test: string;
}

export interface ConversationPayloadSummary {
  id: string;
  request_id: string;
  conversation_id?: string;
  tenant_id?: string;
  tenant_name?: string;
  user_id?: string;
  user_display_name?: string;
  user_email?: string;
  key_id?: string;
  provider: string;
  model: string;
  status: string;
  error_class?: string;
  streaming: boolean;
  response_format: string;
  created_at: string;
  completed_at?: string;
  expires_at?: string;
  last_prompt_role?: string;
  last_prompt_text?: string;
  cost_total_cents?: number;
}

export interface ConversationPayload extends ConversationPayloadSummary {
  request_body: string;
  response_body: string;
  reasoning_content?: string;
  request_truncated: boolean;
  response_truncated: boolean;
}

export interface ConversationPayloadsListResponse {
  conversation_payloads: ConversationPayloadSummary[] | null;
  total_count: number;
}

export interface ConversationSessionSummary {
  owner_id?: string;
  success_count?: number;
  failure_count?: number;
  interrupted_count?: number;
  upload_state?: "uploaded" | "pending" | "not_uploaded";
  transcript_state?: "available" | "rebuilding" | "updating" | "not_available";

  conversation_id?: string;
  tenant_id?: string;
  user_id?: string;
  tenant_name?: string;
  user_display_name?: string;
  user_email?: string;
  provider: string;
  model: string;
  status: string;
  message_count: number;
  last_prompt_role?: string;
  last_prompt_text?: string;
  cost_total_cents: number;
  started_at: string;
  ended_at: string;
}

export interface ConversationSessionsListResponse {
  sessions: ConversationSessionSummary[] | null;
  total_count: number;
}

// --- Session submissions (uploaded Pi sessions + distillation state) ---

export interface SessionSubmission {
  id: string;
  conversation_id: string;
  tenant_id?: string;
  user_id?: string;
  user_display_name?: string;
  user_email?: string;
  key_id?: string;
  session_file?: string;
  message_count: number;
  model?: string;
  total_tokens?: number;
  total_cost_cents?: number;
  started_at?: string;
  ended_at?: string;
  extension_version?: string;
  pi_version?: string;
  thinking_level?: string;
  session_name?: string;
  first_prompt?: string;
  run_mode?: string;
  project?: string;
	project_id?: string;
  distill_attempts: number;
  distilled_at?: string;
  distill_error?: string;
  last_activity_at?: string;
  created_at: string;
}

export interface SessionSubmissionsListResponse {
  session_submissions: SessionSubmission[] | null;
  total_count: number;
}

// --- Session archive (metadata in Postgres, transcripts in object storage) ---

// How a captured request related to the stored conversation history.
export type SessionTurnKind = "extend" | "prefix" | "reset" | "append" | "unparsed";

export interface SessionArchiveSession {
  conversation_id: string;
  tenant_id?: string;
  tenant_name?: string;
  user_id?: string;
  user_display_name?: string;
  user_email?: string;
  key_id?: string;
  provider: string;
  model: string;
  api: string;
  opening_prompt?: string;
  last_prompt?: string;
  request_count: number;
  user_turn_count: number;
  tool_call_count: number;
  error_count: number;
  reset_count: number;
  started_at: string;
  last_activity_at: string;
  cost_total_cents: number;
  transcript_bytes: number;
  objects_location: string;
  generation: number;
  last_seq: number;
  needs_rebuild: boolean;
  last_error?: string;
}

export interface SessionArchiveTurn {
  generation: number;
  seq: number;
  request_id: string;
  created_at: string;
  provider: string;
  model: string;
  status: string;
  kind: SessionTurnKind;
  base: number;
  input_count: number;
  delta_count: number;
  output_count: number;
  system_changed: boolean;
  prompt_preview?: string;
  tool_names: string[] | null;
  segment_bytes: number;
  cost_total_cents: number;
}

export interface SessionArchiveMatch {
  tenant_id: string;
  tenant_name?: string;
  last_activity_at: string;
  request_count: number;
  has_transcript: boolean;
}

export interface SessionClientIP {
  ip: string;
  requests: number;
  first_seen: string;
  last_seen: string;
}

export interface SessionArchiveResponse {
  tenant_id: string;
  matches: SessionArchiveMatch[];
  rebuilding: boolean;
  // False when the transcript's objects are in a different store (written
  // before a backend switch): it must be rebuilt before pages can be read.
  transcript_readable?: boolean;
  session: SessionArchiveSession | null;
  turns: SessionArchiveTurn[];
  // Distinct client IPs observed for this session's requests, most-used
  // first. Empty until a transcript exists (IPs come from usage events
  // joined by request id).
  client_ips?: SessionClientIP[];
}

export interface SessionDisplayItem {
  kind: "user" | "assistant" | "reasoning" | "tool_call" | "tool_result" | "system" | "other";
  text?: string;
  name?: string;
  call_id?: string;
  arguments?: string;
  index?: number;
  echo?: boolean;
  encrypted?: boolean;
  truncated?: boolean;
}

export interface SessionTranscriptTurn {
  seq: number;
  request_id: string;
  created_at: string;
  provider: string;
  model: string;
  status: string;
  api: string;
  kind: SessionTurnKind;
  base: number;
  input_count: number;
  request_truncated?: boolean;
  response_truncated?: boolean;
  system?: string;
  items: SessionDisplayItem[];
  output: SessionDisplayItem[];
  error?: string;
}

export interface SessionTranscriptPage {
  tenant_id: string;
  generation: number;
  turns: SessionTranscriptTurn[];
  next_seq: number | null;
  last_seq: number;
}

// --- Extension error monitoring ---

export interface ExtensionError {
  id: string;
  tenant_id: string;
  tenant_name?: string;
  user_id?: string;
  user_name?: string;
  key_id: string;
  operation: string;
  error_code: string;
  http_status: number;
  message?: string;
  project_id?: string;
  conversation_id?: string;
  extension_version?: string;
  occurrence_count: number;
  first_seen_at: string;
  last_seen_at: string;
}

export interface ExtensionErrorsListResponse {
  extension_errors: ExtensionError[] | null;
  total_count: number;
}

// --- Repository-backed projects ---

export interface ProjectMetrics {
  session_count: number;
  unattributed_sessions: number;
  distilled_sessions: number;
  skipped_sessions: number;
  pending_sessions: number;
  failed_sessions: number;
  message_count: number;
  total_tokens: number;
  total_cost_cents: number;
	request_count: number;
  last_activity_at?: string;
	last_activity_source?: "proxy_request" | "session_submission";
}

export interface ProjectAttributionCoverage {
	total_sessions: number;
	canonical_project_sessions: number;
	legacy_project_sessions: number;
	unattributed_sessions: number;
	total_model_requests: number;
	project_attributed_requests: number;
}

export interface ProjectParticipant {
  user_id: string;
  display_name: string;
  email: string;
  session_count: number;
  message_count: number;
  total_cost_cents: number;
  last_activity_at?: string;
}

export interface ProjectUser {
  id: string;
  display_name: string;
  email: string;
}

export interface ProjectParticipantSummary {
  name: string;
}

export interface ProjectKnowledgeSummary {
  available: boolean;
  has_memory: boolean;
  memory_tokens: number;
  memory_bytes: number;
  insight_count: number;
  prompt_reflection_count: number;
  last_knowledge_activity?: string;
}

export interface RepositoryProjectSummary {
  id: string;
  tenant_id: string;
  tenant_name: string;
  slug: string;
  display_name: string;
  canonical_repo_key: string;
  remotes: string[];
  participants: ProjectParticipantSummary[];
  metrics: ProjectMetrics;
  knowledge: ProjectKnowledgeSummary;
  created_at: string;
  updated_at: string;
}

export interface ProjectKnowledgeEntry {
  timestamp: string;
  content: string;
  attribution?: {
    source: string;
    conversation_id?: string;
    user?: ProjectUser;
  };
}

export type RepositoryProjectDetail = Omit<RepositoryProjectSummary, "participants"> & {
  participants: ProjectParticipant[];
  memory: string;
  insights: ProjectKnowledgeEntry[];
  prompt_reflections: ProjectKnowledgeEntry[];
};

// --- Project memory (duffel-backed knowledge) ---

export interface MemoryLessonEntry {
  timestamp: string;
  content: string;
}

export interface MemoryProjectSummary {
  name: string;
  lessons: number;
  memory_tokens: number;
  memory_bytes: number;
  has_memory: boolean;
  last_activity: string;
}

export interface MemoryProjectDetail {
  project: string;
  memory: string;
  lessons: MemoryLessonEntry[] | null;
}

export interface MemoryBundlePreview {
  project: string;
  markdown: string;
  token_estimate: number;
  degraded: boolean;
  degraded_reason: string;
  from_cache: boolean;
  hint: string;
}

export interface MemorySearchResult {
  path: string;
  title: string;
  snippet: string;
  score: number;
  modified_at: string;
}

export interface MemoryRule {
  id: string;
  project?: string;
  tenant_id?: string;
  user_id?: string;
  enabled: boolean;
  budget_tokens?: number;
  note?: string;
  created_at: string;
  updated_at: string;
}

// --- Web console access ---

export interface WebIdentityDomain {
  domain: string;
  tenant_id: string;
  default_scopes: string[];
  status: string;
  created_at: string;
  updated_at: string;
}

export interface WebIdentity {
  id: string;
  issuer: string;
  subject: string;
  tenant_id: string;
  user_id: string;
  email: string;
  display_name?: string;
  last_login_at?: string;
  created_at: string;
}

// --- Hook subscriptions ---

export interface HookSubscription {
  id: string;
  name: string;
  tenant_id?: string;
  project_id?: string;
  event_type: string;
  url: string;
  has_secret: boolean;
  timeout_ms: number;
  mode: "blocking" | "async";
  /** Ships prompts (and replies) to the endpoint. A content-egress grant. */
  include_content: boolean;
  /** Lets the endpoint rewrite the prompt or the reply. */
  allow_mutation: boolean;
  fail_mode: "open" | "closed";
  priority: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface HookDelivery {
  id: number;
  subscription_id: string;
  event_id: string;
  event_type: string;
  tenant_id?: string;
  status: string;
  http_status?: number;
  latency_ms?: number;
  error?: string;
  created_at: string;
}
