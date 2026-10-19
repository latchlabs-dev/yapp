/**
 * Pi Corporate Proxy Extension (dynamic model discovery + session submission
 * + auto-update + project memory)
 *
 * Fetches available providers and models from the proxy at startup and
 * registers them with Pi. Submits session metadata to the proxy at shutdown
 * for correlation with server-side usage logs. Checks for extension updates
 * on startup and self-updates when a new version is available.
 *
 * Project memory: at session start the extension asks the proxy for a
 * duffel-backed context bundle for the current project (curated memory,
 * recent lessons, related notes) and injects it into the session. The proxy
 * decides server-side whether injection happens (hook chain + admin rules),
 * so behavior can change centrally without updating clients. Agents record
 * lessons/mistakes via the record_memory tool and can retrieve relevant shared
 * context on demand with retrieve_memories (when Pi supports tool registration).
 *
 * Usage:
 *   1. Save this file as ~/.pi/agent/extensions/corp-proxy.ts
 *      (create the directory if it doesn't exist: mkdir -p ~/.pi/agent/extensions)
 *   2. Restart pi — copies downloaded from the proxy carry your API key
 *      embedded, so no environment variables are needed.
 *   3. All provider models configured on the proxy are registered
 *      automatically (marked "(corp)" in the model selector).
 *
 * Optional environment overrides:
 *   CORP_PROXY_KEY      — overrides the embedded API key
 *   CORP_PROXY_BASE_URL — overrides the proxy base URL
 *   CORP_PROXY_ENABLE_SESSION_UPLOAD=1 — explicitly enables current-session
 *     JSONL upload (off by default)
 *   CORP_PROXY_DISABLE_SESSION_UPLOAD=1 — emergency override disabling upload
 *   CORP_PROXY_ENABLE_HISTORICAL_SESSION_UPLOAD=1 — explicitly uploads older
 *     sessions from the global Pi session tree (off by default)
 *
 * Requirements on the proxy:
 *   - PROXY_SECRET_PROVIDERS_DEEPSEEK_API_KEY (if DeepSeek models are enabled)
 *   - /proxy/v1/models endpoint returns available providers and models
 *   - /proxy/v1/quota returns the caller's live spend balances
 *   - /proxy/v1/session-submit accepts session metadata
 *   - /proxy/v1/projects/resolve returns a tenant-scoped canonical project ID
 *   - /proxy/v1/extension-errors accepts silent extension health reports
 *   - /proxy/v1/session-logs accepts canonical gzip Pi session uploads
 *   - /proxy/v1/extension-version returns version info for auto-update
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import * as piAi from "@earendil-works/pi-ai";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { readFile, readdir, rename, stat, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { basename, join } from "node:path";
import { promisify } from "node:util";
import { gzip } from "node:zlib";

// --- Embedded version (bumped alongside proxy's extensionversion.CurrentExtensionVersion) ---
const EXTENSION_VERSION = "1.3.26";
const execFileAsync = promisify(execFile);
const reportedExtensionErrors = new Set<string>();

// Personalized by the proxy at download time: /proxy/v1/extension-download
// substitutes the requesting client's own API key and the deployment's base
// URL, so every downloaded copy (onboarding and auto-update alike) carries
// working credentials. The raw repo copy keeps the placeholders and falls
// back to env vars / the built-in default URL.
const EMBEDDED_KEY = "__CORP_PROXY_EMBEDDED_KEY__";
const EMBEDDED_BASE_URL = "__CORP_PROXY_EMBEDDED_BASE_URL__";

// Suffix appended to model display names so corp-proxy models are easy to
// spot in Pi's model selector.
const MODEL_BADGE = " (corp)";
const DEEPSEEK_API_TYPE = "deepseek-completions";
const OPENAI_COMPAT_API_TYPE = "openai-completions";
const CODEX_API_TYPE = "openai-codex-responses";
const CODEX_PI_PROVIDER_KEY = "openai-codex";
const ZAI_THINKING_FORMAT = "zai";
const CODEX_PROXY_SENTINEL_TOKEN = makeUnsignedJWT({
  "https://api.openai.com/auth": { chatgpt_account_id: "corp-proxy" },
});
const openAICompletionsStreamSimple = piAi.openAICompletionsApi().streamSimple;
function streamSimpleDeepSeekCompletions(model: any, context: any, options?: any) {
  return openAICompletionsStreamSimple(asOpenAICompatibleModel(model), context, options);
}

function streamSimpleZAICompletions(model: any, context: any, options?: any) {
  if (model?.compat?.thinkingFormat !== ZAI_THINKING_FORMAT) {
    return openAICompletionsStreamSimple(asOpenAICompatibleModel(model), context, options);
  }
  return withZAIThinkingPreserved(() =>
    openAICompletionsStreamSimple(asOpenAICompatibleModel(model), context, options),
  );
}

function asOpenAICompatibleModel(model: any) {
  // Pi may pass provider-level fields back on the selected model. If the
  // provider's custom streamSimple survives this delegation, Pi redispatches
  // into this adapter and overflows the stack before making a network request.
  const { streamSimple: _streamSimple, ...safeModel } = model ?? {};
  return { ...safeModel, api: OPENAI_COMPAT_API_TYPE };
}

function streamSimpleProxyCodexResponses(model: any, context: any, options?: any) {
  const piCodexStreamSimple = getPiCodexStreamSimple();
  const proxyKey = process.env.CORP_PROXY_KEY || (EMBEDDED_KEY.startsWith("__CORP_PROXY") ? "" : EMBEDDED_KEY);
  if (!proxyKey) {
    throw new Error("CORP_PROXY_KEY is required for proxy-backed Codex models");
  }
  installCodexProxyFetchPatch(proxyKey, model?.baseUrl);
  return piCodexStreamSimple(model, context, {
    ...options,
    apiKey: CODEX_PROXY_SENTINEL_TOKEN,
    transport: "sse",
    onPayload: async (payload: any, requestModel: any) => {
      const nextPayload =
        payload && typeof payload === "object" && !Array.isArray(payload)
          ? { ...payload, model: stripCorpModelPrefix(payload.model) }
          : payload;
      return options?.onPayload ? options.onPayload(nextPayload, requestModel) : nextPayload;
    },
  });
}

function hasPiCodexResponsesHelper(): boolean {
  return typeof piAi.openAICodexResponsesApi === "function";
}

function getPiCodexStreamSimple(): (model: any, context: any, options?: any) => any {
  const factory = piAi.openAICodexResponsesApi;
  if (typeof factory !== "function") {
    throw new Error("This Pi build does not include openAICodexResponsesApi; update Pi/Codex to use proxy-backed Codex models");
  }
  const api = factory();
  if (!api || typeof api.streamSimple !== "function") {
    throw new Error("This Pi build returned an invalid Codex Responses API helper");
  }
  return api.streamSimple;
}

function stripCorpModelPrefix(model: unknown): unknown {
  return typeof model === "string" && model.startsWith("corp-") ? model.slice("corp-".length) : model;
}

let codexProxyFetchPatchInstalled = false;
let codexProxyKey = "";
const codexProxyBaseURLs = new Set<string>();

function installCodexProxyFetchPatch(proxyKey: string, baseUrl: string | undefined) {
  codexProxyKey = proxyKey;
  const normalized = normalizeURLPrefix(baseUrl);
  if (normalized) codexProxyBaseURLs.add(normalized);
  if (codexProxyFetchPatchInstalled) return;

  const originalFetch = globalThis.fetch;
  globalThis.fetch = (input, init) => {
    const url = requestURL(input);
    if (codexProxyKey && isProxyCodexURL(url)) {
      const headers = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined));
      headers.set("Authorization", `Bearer ${codexProxyKey}`);
      return originalFetch(input, { ...init, headers });
    }
    return originalFetch(input, init);
  };
  codexProxyFetchPatchInstalled = true;
}

function isProxyCodexURL(url: string) {
  const normalized = normalizeURLPrefix(url);
  if (!normalized) return false;
  for (const baseURL of codexProxyBaseURLs) {
    if (normalized === baseURL || normalized.startsWith(`${baseURL}/`)) return true;
  }
  return false;
}

function requestURL(input: string | URL | Request): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.toString();
  return input.url;
}

function normalizeURLPrefix(url: string | undefined): string {
  if (!url) return "";
  try {
    return new URL(url).toString().replace(/\/+$/, "");
  } catch {
    return "";
  }
}

function makeUnsignedJWT(payload: Record<string, unknown>): string {
  return [
    base64URL(JSON.stringify({ alg: "none", typ: "JWT" })),
    base64URL(JSON.stringify(payload)),
    "corp-proxy",
  ].join(".");
}

function base64URL(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function withZAIThinkingPreserved<T>(fn: () => T): T {
  const originalFetch = globalThis.fetch;
  const patchedFetch: typeof fetch = (input, init) => originalFetch(input, patchZAIFetchInit(init));
  globalThis.fetch = patchedFetch;

  let result: T;
  try {
    result = fn();
  } catch (err) {
    globalThis.fetch = originalFetch;
    throw err;
  }

  if (isAsyncIterable(result)) {
    globalThis.fetch = originalFetch;
    return wrapAsyncIterableWithPatchedFetch(result, originalFetch, patchedFetch) as T;
  }

  if (isPromiseLike(result)) {
    return result.finally(() => {
      globalThis.fetch = originalFetch;
    }) as T;
  }

  globalThis.fetch = originalFetch;
  return result;
}

function patchZAIFetchInit(init?: RequestInit): RequestInit | undefined {
  if (!init?.body) return init;

  const bodyText = requestBodyToString(init.body);
  if (!bodyText) return init;

  let payload: unknown;
  try {
    payload = JSON.parse(bodyText);
  } catch {
    return init;
  }
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return init;

  const record = payload as Record<string, unknown>;
  const currentThinking = record.thinking;
  record.thinking =
    currentThinking && typeof currentThinking === "object" && !Array.isArray(currentThinking)
      ? { ...(currentThinking as Record<string, unknown>), clear_thinking: false }
      : { clear_thinking: false };

  return { ...init, body: JSON.stringify(record) };
}

function requestBodyToString(body: BodyInit): string | null {
  if (typeof body === "string") return body;
  if (body instanceof Uint8Array) return new TextDecoder().decode(body);
  return null;
}

function isAsyncIterable(value: unknown): value is AsyncIterable<unknown> {
  return !!value && typeof (value as { [Symbol.asyncIterator]?: unknown })[Symbol.asyncIterator] === "function";
}

function isPromiseLike(value: unknown): value is Promise<unknown> {
  return !!value && typeof (value as { finally?: unknown }).finally === "function";
}

async function* wrapAsyncIterableWithPatchedFetch(
  iterable: AsyncIterable<unknown>,
  originalFetch: typeof fetch,
  patchedFetch: typeof fetch,
) {
  globalThis.fetch = patchedFetch;
  try {
    for await (const item of iterable) {
      yield item;
    }
  } finally {
    globalThis.fetch = originalFetch;
  }
}

// --- Session state accumulated across agent runs ---
interface SessionStats {
  messageCount: number;
  model: string;
  totalTokens: number;
  totalCostCents: number;
  startedAt: string;
  endedAt: string;
  thinkingLevel: string;
  firstPrompt: string;
  runMode: string;
}

// --- Context bundle response from POST /proxy/v1/context ---
interface ContextResponse {
  project: string;
  project_id?: string;
  inject: boolean;
  reason?: string;
  bundle: string;
  token_estimate: number;
  degraded: boolean;
}

interface QuotaPeriod {
  used_cents: number;
  used_percent: number | null;
  limit_cents: number;
  remaining_cents: number | null;
  remaining_percent: number | null;
  resets_at: string;
  unlimited: boolean;
  exhausted: boolean;
  binding_scope_type?: string;
}

interface QuotaResponse {
  currency: string;
  as_of: string;
  daily: QuotaPeriod;
  monthly: QuotaPeriod;
  cache_trend: CacheTrend;
}

interface CacheTrendStats {
  request_count: number;
  cached_requests: number;
  input_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cache_read_share_percent: number;
  request_cache_hit_percent: number;
}

interface CacheTrendDay extends CacheTrendStats {
  date: string;
}

interface CacheTrend {
  available: boolean;
  scope_type?: string;
  days: number;
  current: CacheTrendStats;
  previous: CacheTrendStats;
  cache_read_share_change_points: number;
  direction: "up" | "down" | "flat";
  daily: CacheTrendDay[];
}

export default async function (pi: ExtensionAPI) {
  const embeddedBaseUrl = EMBEDDED_BASE_URL.startsWith("__CORP_PROXY")
    ? "http://localhost:18080"
    : EMBEDDED_BASE_URL;
  const baseUrl = process.env.CORP_PROXY_BASE_URL ?? embeddedBaseUrl;

  // Credential resolution: env var override first, then the key embedded at
  // download time. The placeholder check keeps the raw repo copy harmless.
  const embeddedKey = EMBEDDED_KEY.startsWith("__CORP_PROXY") ? "" : EMBEDDED_KEY;
  const resolvedKey = process.env.CORP_PROXY_KEY || embeddedKey;

  // Provider registration uses the env-var reference when the override is
  // active (Pi expands $VARS at request time), otherwise the embedded key.
  const apiKey = process.env.CORP_PROXY_KEY ? "$CORP_PROXY_KEY" : resolvedKey;

  // --- User command: /quota ---
  const registerCommand = (pi as any).registerCommand;
  if (typeof registerCommand === "function") {
    registerCommand.call(pi, "quota", {
      description: "Show your used and remaining daily/monthly proxy quota",
      handler: async (_args: string, ctx: ExtensionContext) => {
        if (!resolvedKey) {
          ctx.ui.notify("Proxy quota unavailable: CORP_PROXY_KEY is not configured.", "error");
          return;
        }
        const setStatus = (ctx.ui as any).setStatus;
        if (typeof setStatus === "function") {
          setStatus.call(ctx.ui, "corp-proxy-quota", "Checking quota...");
        }
        try {
          const usage = await fetchQuota(baseUrl, resolvedKey);
          const message = [
            "Proxy quota",
            formatQuotaPeriod("Today", usage.daily, usage.currency),
            formatQuotaPeriod("This month", usage.monthly, usage.currency),
            ...formatCacheTrend(usage.cache_trend),
          ].join("\n");
          ctx.ui.notify(
            message,
            usage.daily.exhausted || usage.monthly.exhausted ? "warning" : "info",
          );
        } catch (err) {
          ctx.ui.notify(`Proxy quota check failed: ${errorMessage(err)}`, "error");
        } finally {
          if (typeof setStatus === "function") {
            setStatus.call(ctx.ui, "corp-proxy-quota", undefined);
          }
        }
      },
    });
  }

  // Collect model names for the notification later.
  let registeredModels: string[] = [];
  let fetchError: string | null = null;

  // Provider configs captured at registration so they can be re-registered
  // per session with an opaque X-Session-ID header (see session_start). Sending the
  // session id as a transport header — not a body field — is what reaches the
  // proxy: Pi's provider SDKs strip non-standard body keys, but headers pass
  // through untouched. The proxy stores it as conversation_payloads.conversation_id,
  // linking captured chat payloads to the uploaded session.
  type RegisteredProviderConfig = Parameters<ExtensionAPI["registerProvider"]>[1] & {
    streamSimple?: typeof streamSimpleDeepSeekCompletions;
  };
  type RegisteredProvider = { name: string; config: RegisteredProviderConfig };
  const registeredProviders: RegisteredProvider[] = [];

  const registerProvider = (name: string, config: RegisteredProviderConfig) => {
    const hasZAIModels = config.models?.some(
      (model) => model.compat?.thinkingFormat === ZAI_THINKING_FORMAT,
    );
    const providerConfig = {
      ...config,
      headers: {
        ...(config.headers ?? {}),
        "X-Extension-Version": EXTENSION_VERSION,
      },
      ...(config.api === DEEPSEEK_API_TYPE ? { streamSimple: streamSimpleDeepSeekCompletions } : {}),
      ...(hasZAIModels ? { streamSimple: streamSimpleZAICompletions } : {}),
      ...(config.api === CODEX_API_TYPE
        ? {
            streamSimple: (model: any, context: any, options?: any) =>
              streamSimpleProxyCodexResponses({ ...model, baseUrl: model?.baseUrl ?? config.baseUrl }, context, options),
          }
        : {}),
    };
    registeredProviders.push({ name, config: providerConfig });
    pi.registerProvider(name, providerConfig);
  };

  const reregisterWithAttributionHeaders = (sessionId: string, resolvedProjectId = "") => {
    if (!sessionId) return;
    for (const { name, config } of registeredProviders) {
      pi.registerProvider(name, {
        ...config,
        headers: {
          ...(config.headers ?? {}),
          "X-Session-ID": sessionId,
          ...(resolvedProjectId ? { "X-Project-ID": resolvedProjectId } : {}),
        },
      });
    }
  };

  // --- Auto-update check (runs before model fetch) ---
  if (resolvedKey && process.env.CORP_PROXY_DISABLE_AUTO_UPDATE !== "1") {
    try {
      await checkAndUpdate(baseUrl, resolvedKey);
    } catch (_err) {
      // Silent — update failure should not block model registration.
    }
  }

  // --- Model discovery ---
  if (resolvedKey) {
    try {
      const response = await fetch(`${baseUrl}/proxy/v1/models`, {
        headers: {
          Authorization: `Bearer ${resolvedKey}`,
        },
      });

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      const data = (await response.json()) as {
        providers: Array<{
          name: string;
          base_url: string;
          api_type: string;
          route_id?: string;
          compat?: {
            supports_developer_role?: boolean;
            supports_reasoning_effort?: boolean;
            max_tokens_field?: string;
          };
          models: Array<{
            id: string;
            name: string;
            reasoning: boolean;
            input: string[];
            // Top-level Pi model fields: null level value = unsupported.
            thinking_level_map?: Record<string, string | null>;
            cost?: { input: number; output: number; cache_read: number; cache_write: number };
            context_window: number;
            max_tokens: number;
            compat?: {
              thinking_format?: string;
              requires_reasoning_content_on_assistant_messages?: boolean;
            };
          }>;
        }>;
      };

      for (const provider of data.providers) {
        if (provider.api_type === CODEX_API_TYPE && !hasPiCodexResponsesHelper()) {
          continue;
        }
        registerProvider(provider.api_type === CODEX_API_TYPE ? CODEX_PI_PROVIDER_KEY : provider.name, {
          ...(provider.api_type === CODEX_API_TYPE ? { name: provider.name } : {}),
          baseUrl: `${baseUrl}${provider.base_url}`,
          apiKey: apiKey,
          api: provider.api_type,
          headers: provider.route_id
            ? { "X-Corp-Provider-ID": provider.route_id }
            : undefined,
          models: provider.models.map((model) => {
            // Merge provider-level compat into each model's compat.
            const modelCompat: Record<string, unknown> = {};
            if (provider.compat?.supports_developer_role !== undefined) {
              modelCompat.supportsDeveloperRole = provider.compat.supports_developer_role;
            }
            if (provider.compat?.supports_reasoning_effort !== undefined) {
              modelCompat.supportsReasoningEffort = provider.compat.supports_reasoning_effort;
            }
            if (provider.compat?.max_tokens_field) {
              modelCompat.maxTokensField = provider.compat.max_tokens_field;
            }
            if (model.compat?.thinking_format) {
              modelCompat.thinkingFormat = model.compat.thinking_format;
            }
            if (model.compat?.requires_reasoning_content_on_assistant_messages !== undefined) {
              modelCompat.requiresReasoningContentOnAssistantMessages =
                model.compat.requires_reasoning_content_on_assistant_messages;
            }
            return {
              id: model.id,
              name: model.name + MODEL_BADGE,
              reasoning: model.reasoning,
              input: model.input as ("text" | "image")[],
              // thinkingLevelMap is top-level (not compat); null values mark
              // unsupported levels and must be preserved.
              thinkingLevelMap: model.thinking_level_map,
              // Pi REQUIRES cost: its cost tracking reads model.cost.input
              // unconditionally and crashes without it.
              cost: model.cost
                ? {
                    input: model.cost.input,
                    output: model.cost.output,
                    cacheRead: model.cost.cache_read,
                    cacheWrite: model.cost.cache_write,
                  }
                : { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
              contextWindow: model.context_window,
              maxTokens: model.max_tokens,
              compat: Object.keys(modelCompat).length > 0 ? modelCompat : undefined,
            };
          }),
        });

        for (const model of provider.models) {
          registeredModels.push(`${provider.name}/${model.id}`);
        }
      }
    } catch (err) {
      fetchError = String(err);
    }
  } else {
    fetchError = "CORP_PROXY_KEY is not set";
  }

  // --- Session stats accumulator ---
  let sessionStats: SessionStats = {
    messageCount: 0,
    model: "",
    totalTokens: 0,
    totalCostCents: 0,
    startedAt: new Date().toISOString(),
    endedAt: "",
    thinkingLevel: "",
    firstPrompt: "",
    runMode: "",
  };

  // Track the conversation_id for correlation. Keep this URL-safe and opaque;
  // the full Pi session file path is submitted separately as session_file.
  let conversationId: string | undefined;

  // Project identity for memory correlation (detected once at startup).
  // Projects map 1:1 to git repositories; the repo URL is sent along so the
  // proxy can bootstrap the project's duffel folder with provenance.
	const { project, repoUrl, remotes } = await detectProject();
	let projectId = resolvedKey && project
	  ? await resolveProjectIdentity(baseUrl, resolvedKey, project, repoUrl, remotes)
	  : "";
	const refreshProviderAttribution = () => {
	  if (!conversationId) return;
	  reregisterWithAttributionHeaders(conversationId, projectId);
	  if (fetchError) {
		registerFallback(pi, baseUrl, apiKey, conversationId, projectId);
	  }
	};
	const sessionUploader = resolvedKey &&
	  process.env.CORP_PROXY_ENABLE_SESSION_UPLOAD === "1" &&
	  process.env.CORP_PROXY_DISABLE_SESSION_UPLOAD !== "1"
    ? new SessionLogUploader(baseUrl, resolvedKey, () => ({ project, projectId, repoUrl }))
    : null;
  let currentSessionFile = "";

  // --- Project memory state ---
  // The bundle is fetched async at session_start (no hint: instant, served
  // from the proxy's cache) and appended to the system prompt on every agent
  // turn via the before_agent_start event (the officially supported injection
  // point — extensions may return a replacement systemPrompt).
  //
  // Eval-derived refinement: the first user prompt is the best retrieval hint
  // (recall 1.0 vs 0.0 hintless for related notes), so the first
  // before_agent_start kicks off a second fetch with the prompt as hint and
  // swaps the bundle in for subsequent turns. One-time swap only — it trades
  // a single upstream prompt-cache break for hint-matched context.
  let memoryBundle = "";
  let hintRefreshStarted = false;
  let memoryToolGuidance = "";

  pi.on("before_agent_start", (event) => {
    // Capture the first user prompt once as a human-readable session title
    // fallback (the proxy prefers the explicit /name session name).
    if (!sessionStats.firstPrompt) {
      const firstPrompt = (event.prompt ?? "").trim();
      if (firstPrompt) sessionStats.firstPrompt = firstPrompt.slice(0, 300);
    }
    if (!hintRefreshStarted && resolvedKey && project) {
      hintRefreshStarted = true;
      const hint = (event.prompt ?? "").slice(0, 200).trim();
      if (hint) {
		fetchContextBundle(baseUrl, resolvedKey, project, repoUrl, remotes, observationToolRegistered, hint)
          .then((bundle) => {
			if (!bundle) return;
			projectId = bundle.projectId || projectId;
			refreshProviderAttribution();
			if (bundle.text) memoryBundle = bundle.text;
          })
          .catch(() => {});
      }
    }
    // Keep the tool reminder last so it is the final line of Pi's master prompt.
    const additions = [memoryBundle, memoryToolGuidance].filter(Boolean);
    if (additions.length === 0) return undefined;
    return { systemPrompt: `${event.systemPrompt}\n\n${additions.join("\n\n")}` };
  });

  // --- Project-memory tools (capability-detected for older Pi versions) ---
  let observationToolRegistered = false;
  if (resolvedKey && project) {
    const tools = await tryRegisterMemoryTools(
      pi, baseUrl, resolvedKey, project, repoUrl, remotes,
      () => projectId,
      (resolved) => {
		projectId = resolved || projectId;
		refreshProviderAttribution();
	  },
    );
    observationToolRegistered = tools.record;
    if (tools.record || tools.retrieve) {
      memoryToolGuidance =
        `Project memory tools: proactively use retrieve_memories to read relevant shared project context, and use record_memory to write durable project-specific lessons, mistakes, lookup pointers, and user-confirmed decisions; never store secrets, personal data, session trivia, or agent-inferred decisions.`;
    }
  }

  // --- Agent lifecycle hooks ---

  // Track message count and model on each agent run.
  pi.on("agent_start", (_event) => {
    sessionStats.messageCount++;
  });

  pi.on("agent_end", (event, ctx) => {
    sessionStats.endedAt = new Date().toISOString();
    // Track the effective thinking level of the last completed run.
    if (ctx?.thinkingLevel) sessionStats.thinkingLevel = ctx.thinkingLevel;
    // Accumulate usage across turns.
    if (event.messages && Array.isArray(event.messages)) {
      for (const msg of event.messages) {
        if (msg.role === "assistant" && msg.usage) {
          sessionStats.totalTokens += msg.usage.inputTokens ?? 0;
          sessionStats.totalTokens += msg.usage.outputTokens ?? 0;
          if (msg.usage.cost?.total) {
            sessionStats.totalCostCents += msg.usage.cost.total * 100; // dollars → cents
          }
        }
      }
    }
    // Track the last model used.
    if (event.messages && event.messages.length > 0) {
      const lastMsg = event.messages[event.messages.length - 1];
      if (lastMsg.role === "assistant") {
        sessionStats.model = (lastMsg as any).model ?? sessionStats.model;
      }
    }
  });

  // --- Periodic heartbeat: flush stats every 2m during long sessions ---
  // Protects against crashes mid-session without the noise of per-turn submits.
  let lastSubmittedMessageCount = 0;
  const heartbeat = setInterval(() => {
    if (!resolvedKey || !conversationId) return;
    if (sessionStats.messageCount === 0) return;
    if (sessionStats.messageCount === lastSubmittedMessageCount) return;

    lastSubmittedMessageCount = sessionStats.messageCount;
    submitSession(baseUrl, resolvedKey, sessionPayload()).catch(() => {});
    if (currentSessionFile) {
      sessionUploader?.syncFile(currentSessionFile).catch(() => {});
    }
  }, 120_000);

  // Clean up heartbeat on shutdown.
  pi.on("session_shutdown", () => clearInterval(heartbeat));

  // --- Session shutdown: final flush ---
  pi.on("session_shutdown", async (_event, ctx) => {
    if (!resolvedKey) return;

    sessionStats.endedAt = sessionStats.endedAt || new Date().toISOString();
    conversationId = conversationId ?? getConversationId(ctx);

    currentSessionFile = getSessionFile(ctx);
    const payload = {
      ...sessionPayload(),
      session_file: currentSessionFile, // available at shutdown
    };

    // Await the bounded final upload so the canonical JSONL is not lost when
    // Pi exits immediately after this hook returns. Metadata remains
    // best-effort and is submitted alongside it.
    await Promise.allSettled([
      submitSession(baseUrl, resolvedKey, payload),
      ...(sessionUploader && currentSessionFile
        ? [sessionUploader.syncFile(currentSessionFile)]
        : []),
    ]);
  });

  // --- Helper: build the submission payload from current stats ---
  function sessionPayload(): Record<string, unknown> {
    return {
      conversation_id: conversationId,
      session_file: "", // only populated at shutdown when ctx is available
      message_count: sessionStats.messageCount,
      model: sessionStats.model || undefined,
      total_tokens: sessionStats.totalTokens > 0 ? sessionStats.totalTokens : undefined,
      total_cost_cents: sessionStats.totalCostCents > 0 ? sessionStats.totalCostCents : undefined,
      started_at: sessionStats.startedAt,
      ended_at: sessionStats.endedAt,
      extension_version: EXTENSION_VERSION,
      pi_version: getPiVersion(),
      thinking_level: sessionStats.thinkingLevel || undefined,
      session_name: getSessionName(pi) || undefined,
      first_prompt: sessionStats.firstPrompt || undefined,
      run_mode: sessionStats.runMode || undefined,
      project: project || undefined,
		project_id: projectId || undefined,
		repo: repoUrl || undefined,
		remotes: remotes.length > 0 ? remotes : undefined,
    };
  }

  // --- Session start: capture conversation ID + kick off async backfill ---
  pi.on("session_start", async (_event: { reason: string }, ctx: ExtensionContext) => {
    conversationId = getConversationId(ctx);
    currentSessionFile = getSessionFile(ctx);

    // Run at most one background historical pass at a time. Directory walking
    // is metadata-only; unchanged files reuse the locally cached hash and are
    // not opened. The current session is handled again by heartbeat/shutdown.
    const sessionDir = getSessionDir(ctx);
    if (sessionDir && process.env.CORP_PROXY_ENABLE_HISTORICAL_SESSION_UPLOAD === "1") {
      sessionUploader?.syncHistorical(sessionRootFromDir(sessionDir)).catch(() => {});
    }

    // Attach the session id as a header on all proxy providers so captured
    // payloads correlate to this session (body fields get stripped by SDKs).
    reregisterWithAttributionHeaders(conversationId, projectId);

    // Reset for new session, then kick off async backfill from disk.
    // The backfill runs in the background so startup is never blocked.
    sessionStats = {
      messageCount: 0,
      model: "",
      totalTokens: 0,
      totalCostCents: 0,
      startedAt: new Date().toISOString(),
      endedAt: "",
      thinkingLevel: "",
      firstPrompt: "",
      runMode: ctx?.mode || sessionStats.runMode,
    };
    lastSubmittedMessageCount = 0;
    hintRefreshStarted = false;

    // Async backfill: reads entries from the session file and merges
    // recovered stats into sessionStats. If an agent_end fires before
    // this completes, the recovered floor is merged additively.
    rebuildAndSubmit(ctx).catch(() => {});

    // Project memory: fetch the context bundle. Fire-and-forget — the
    // session must never wait on (or fail because of) memory. Once loaded,
    // the before_agent_start hook appends it to the system prompt.
    if (resolvedKey && project) {
		fetchContextBundle(baseUrl, resolvedKey, project, repoUrl, remotes, observationToolRegistered)
        .then((bundle) => {
          if (!bundle) return;
		  projectId = bundle.projectId || projectId;
		  refreshProviderAttribution();
		  if (!bundle.text) return;
          memoryBundle = bundle.text;
          try {
            ctx.ui.notify(`Project memory loaded for "${project}" (~${bundle.tokens} tokens)`, "info");
          } catch {
            // Notification failure is cosmetic.
          }
        })
        .catch(() => {});
    }

    if (fetchError) {
      ctx.ui.notify(
        `Proxy: failed to fetch models (${fetchError}). Using fallback.`,
        "warning",
      );
      registerFallback(pi, baseUrl, apiKey, conversationId ?? "", projectId);
      return;
    }

    if (registeredModels.length > 0) {
      ctx.ui.notify(
        `Proxy: ${registeredModels.join(", ")}`,
        "info",
      );
    }
  });

  // --- Async backfill: rebuild stats from session file and submit ---
  // Runs in background after session_start; never blocks startup.
  // Merges recovered stats into sessionStats using max (agent_end only adds)
  // so concurrent agent_end events are not overwritten.
  async function rebuildAndSubmit(ctx: ExtensionContext): Promise<void> {
    const recovered = await recoverStatsFromEntries(ctx);
    if (!recovered || recovered.messageCount === 0) return;

    sessionStats.messageCount = Math.max(sessionStats.messageCount, recovered.messageCount);
    sessionStats.totalTokens = Math.max(sessionStats.totalTokens, recovered.totalTokens);
    sessionStats.totalCostCents = Math.max(sessionStats.totalCostCents, recovered.totalCostCents);
    if (recovered.model && !sessionStats.model) {
      sessionStats.model = recovered.model;
    }
    if (recovered.startedAt && (!sessionStats.startedAt || recovered.startedAt < sessionStats.startedAt)) {
      sessionStats.startedAt = recovered.startedAt;
    }
    if (recovered.endedAt && (!sessionStats.endedAt || recovered.endedAt > sessionStats.endedAt)) {
      sessionStats.endedAt = recovered.endedAt;
    }

    if (resolvedKey) {
      const payload = {
        ...sessionPayload(),
        session_file: getSessionFile(ctx),
      };
      submitSession(baseUrl, resolvedKey, payload).catch(() => {});
    }
  }

  // --- Helper: read cumulative stats from session entries on disk ---
  async function recoverStatsFromEntries(ctx: ExtensionContext): Promise<SessionStats | null> {
    try {
      const entries = ctx.sessionManager?.getEntries?.();
      if (!entries || !Array.isArray(entries)) return null;

      const stats: SessionStats = {
        messageCount: 0,
        model: "",
        totalTokens: 0,
        totalCostCents: 0,
        startedAt: "",
        endedAt: "",
        thinkingLevel: "",
        firstPrompt: "",
        runMode: "",
      };

      for (const entry of entries) {
        if (entry.type === "user" || entry.role === "user") {
          stats.messageCount++;
        }
        if (entry.type === "assistant" || entry.role === "assistant") {
          const msg = entry.message ?? entry;
          if (msg?.usage) {
            stats.totalTokens += msg.usage.inputTokens ?? 0;
            stats.totalTokens += msg.usage.outputTokens ?? 0;
            if (msg.usage.cost?.total) {
              stats.totalCostCents += msg.usage.cost.total * 100;
            }
          }
          if (msg?.model) stats.model = msg.model;
        }
        const ts = entry.timestamp;
        if (ts) {
          if (!stats.startedAt || ts < stats.startedAt) stats.startedAt = ts;
          if (!stats.endedAt || ts > stats.endedAt) stats.endedAt = ts;
        }
      }

      return stats;
    } catch {
      return null;
    }
  }
}

// --- Project detection ---
// Priority: CORP_PROXY_PROJECT env → git origin repo basename → git-root /
// cwd basename. The proxy re-sanitizes server-side; this client-side pass
// just keeps the value readable and predictable.
async function detectProject(): Promise<{ project: string; repoUrl: string; remotes: string[] }> {
  const override = process.env.CORP_PROXY_PROJECT;
	const remoteOverride = process.env.CORP_PROXY_REPOSITORY_REMOTE;
	if (override && remoteOverride) {
		return { project: sanitizeProjectName(override), repoUrl: remoteOverride, remotes: [remoteOverride] };
	}

  try {
	const cwd = process.cwd();
	const git = async (...args: string[]) => {
	  const result = await execFileAsync("git", ["-C", cwd, ...args], { timeout: 3000, maxBuffer: 64 * 1024 });
	  return String(result.stdout).trim();
	};
	const root = await git("rev-parse", "--show-toplevel");
	const foundRemotes: string[] = [];
	for (const name of ["upstream", "origin"]) {
	  try {
		const remote = await git("remote", "get-url", name);
		if (remote && !foundRemotes.includes(remote)) foundRemotes.push(remote);
	  } catch {
		// The remote is optional.
	  }
	}
	const identityRemotes: string[] = [];
	for (const remote of foundRemotes) {
	  const resolvedAlias = await resolveSSHRemoteAlias(remote);
	  for (const candidate of [resolvedAlias, remote]) {
		if (candidate && !identityRemotes.includes(candidate)) identityRemotes.push(candidate);
	  }
	}
	const preferred = foundRemotes[0] ?? "";
	const remoteName = preferred.split(/[/:]/).pop()?.replace(/\.git$/, "") ?? "";
	return {
	  project: sanitizeProjectName(override || remoteName || basename(root)),
	  repoUrl: preferred,
	  remotes: identityRemotes,
	};
	} catch {
	// Fall through to filesystem detection for installations without git.
  }

  try {
    const fs = await import("node:fs");
    const path = await import("node:path");

    // Walk up from cwd looking for a .git directory.
    let dir = process.cwd();
    for (let i = 0; i < 30; i++) {
      const gitDir = path.join(dir, ".git");
      if (fs.existsSync(gitDir)) {
        // Prefer the origin remote's repo name.
        const configPath = path.join(gitDir, "config");
        if (fs.existsSync(configPath)) {
          const config = fs.readFileSync(configPath, "utf-8");
          const originUrl = parseOriginUrl(config);
          if (originUrl) {
            const repo = originUrl.split("/").pop()?.replace(/\.git$/, "");
			if (repo) return { project: sanitizeProjectName(override || repo), repoUrl: originUrl, remotes: [originUrl] };
          }
        }
		return { project: sanitizeProjectName(override || path.basename(dir)), repoUrl: "", remotes: [] };
      }
      const parent = path.dirname(dir);
      if (parent === dir) break;
      dir = parent;
    }
	return { project: sanitizeProjectName(override || path.basename(process.cwd())), repoUrl: "", remotes: [] };
  } catch {
	return { project: override ? sanitizeProjectName(override) : "", repoUrl: "", remotes: [] };
  }
}

// ssh -G reads local SSH configuration without connecting. Including the
// resolved hostname lets users with aliases such as github.com-work converge
// with coworkers whose remotes use github.com directly.
async function resolveSSHRemoteAlias(remote: string): Promise<string> {
  const match = remote.match(/^([^@/:]+@)?([^/:]+):(.+)$/);
  if (!match || remote.includes("://")) return remote;
  try {
	const result = await execFileAsync("ssh", ["-G", match[2]], { timeout: 1000, maxBuffer: 64 * 1024 });
	const hostname = String(result.stdout).match(/^hostname\s+(\S+)$/mi)?.[1];
	if (!hostname || hostname === match[2]) return remote;
	return `${match[1] ?? ""}${hostname}:${match[3]}`;
  } catch {
	return remote;
  }
}

// parseOriginUrl extracts the url from the [remote "origin"] section.
function parseOriginUrl(gitConfig: string): string | null {
  const match = gitConfig.match(/\[remote "origin"\][^[]*?url\s*=\s*(\S+)/);
  return match ? match[1] : null;
}

// sanitizeProjectName mirrors the proxy's memory.SanitizeProject rules.
function sanitizeProjectName(raw: string): string {
  const name = raw
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/-{2,}/g, "-")
    .replace(/^[-.]+|[-.]+$/g, "");
  if (!name || name.length > 64 || name.includes("..")) return "";
  return name;
}

// --- Observation tool registration ---
// Uses Pi's ToolDefinition API (registerTool). TypeBox is loaded dynamically
// and registration is wrapped in try/catch so older Pi versions without
// registerTool (or without typebox) gracefully fall back to the curl
// instructions embedded in the context bundle.
async function tryRegisterMemoryTools(
  pi: ExtensionAPI,
  baseUrl: string,
  apiKey: string,
  project: string,
	repoUrl: string,
	remotes: string[],
	getProjectID: () => string,
	setProjectID: (projectID: string) => void,
): Promise<{ record: boolean; retrieve: boolean }> {
  const registerTool = (pi as any).registerTool;
  if (typeof registerTool !== "function") return { record: false, retrieve: false };

  try {
    const { Type } = await import("typebox");
    registerTool.call(pi, {
      name: "record_memory",
      label: "Record project memory",
      description:
        "Record a durable project observation (mistake, lesson, decision, or context) " +
        "into the team's shared project memory. Use for project-specific knowledge that " +
        "will help future sessions — not session-specific trivia.",
      parameters: Type.Object({
        kind: Type.Union(
          [
            Type.Literal("mistake"),
            Type.Literal("lesson"),
            Type.Literal("decision"),
            Type.Literal("context"),
          ],
          { description: "The memory kind. Use decision only for a choice explicitly made or confirmed by the user." },
        ),
        content: Type.String({
          description: "1-3 self-contained sentences. Name files, commands, and APIs.",
        }),
      }),
      async execute(
        _toolCallId: string,
        params: { kind: string; content: string },
        _signal: unknown,
        _onUpdate: unknown,
        _ctx: unknown,
      ) {
        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), 5000);
        try {
          const resp = await fetch(`${baseUrl}/proxy/v1/observations`, {
            method: "POST",
            headers: {
              "Content-Type": "application/json",
              Authorization: `Bearer ${apiKey}`,
            },
			body: JSON.stringify({ project, project_id: getProjectID() || undefined, kind: params.kind, content: params.content }),
            signal: controller.signal,
          });
          let text: string;
          if (resp.ok) {
            text = `Observation recorded for project "${project}".`;
          } else {
            let detail = "";
            try {
              const payload = (await resp.json()) as { error?: { message?: string } };
              detail = payload.error?.message?.trim() ?? "";
            } catch {
              // Keep the status-only fallback for non-JSON proxy responses.
            }
            if (resp.status === 403) {
              text =
                "Failed to record observation (HTTP 403): this proxy key does not have " +
                "the memory:observe permission. Ask an administrator to grant memory access " +
                "or issue a key with “Write project observations” enabled.";
            } else {
              text = `Failed to record observation (HTTP ${resp.status})${detail ? `: ${detail}` : "."}`;
            }
          }
          return { content: [{ type: "text", text }], details: undefined };
        } catch (err) {
          return {
            content: [{ type: "text", text: `Failed to record observation: ${String(err)}` }],
            details: undefined,
          };
        } finally {
          clearTimeout(timeout);
        }
      },
    });
    registerTool.call(pi, {
      name: "retrieve_memories",
      label: "Retrieve project memories",
      description:
        "Retrieve shared memory for the current project, optionally focused on a task or question. " +
        "Use before repeating investigation or when prior decisions, mistakes, commands, or code locations may help.",
      parameters: Type.Object({
        query: Type.Optional(Type.String({
          description: "Optional task, question, symbol, error, or topic used to focus memory retrieval.",
        })),
      }),
      async execute(
        _toolCallId: string,
        params: { query?: string },
        _signal: unknown,
        _onUpdate: unknown,
        _ctx: unknown,
      ) {
        const bundle = await fetchContextBundle(
          baseUrl, apiKey, project, repoUrl, remotes, true, params.query?.trim() ?? "", true,
        );
        if (bundle?.projectId) setProjectID(bundle.projectId);
        const text = bundle?.text || `No shared memories were found for project "${project}".`;
        return { content: [{ type: "text", text }], details: undefined };
      },
    });
    return { record: true, retrieve: true };
  } catch {
    return { record: false, retrieve: false };
  }
}

// --- Context bundle fetch ---
// Asks the proxy for the project's context bundle. The proxy's hook chain
// (config toggle + admin-managed rules) decides server-side whether to
// inject, so this can be turned off centrally without touching clients.
async function fetchContextBundle(
  baseUrl: string,
  apiKey: string,
  project: string,
  repoUrl: string,
	remotes: string[],
  toolRegistered: boolean,
  hint = "",
  retrieve = false,
): Promise<{ text: string; tokens: number; projectId: string } | null> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), hint ? 8000 : 4000);

  try {
	const resp = await fetch(`${baseUrl}/proxy/v1/context`, {
	  method: "POST",
	  headers: {
	    "Content-Type": "application/json",
	    Authorization: `Bearer ${apiKey}`,
	  },
	  body: JSON.stringify({ project, repo: repoUrl || undefined, remotes: remotes.length > 0 ? remotes : undefined, hint: hint || undefined, tools: toolRegistered, retrieve: retrieve || undefined }),
      signal: controller.signal,
    });
    if (!resp.ok) return null;
    const data = (await resp.json()) as ContextResponse;
    // inject=false (hook chain said no) or empty bundle (no memory yet).
	if ((!data.inject && !retrieve) || !data.bundle) return { text: "", tokens: 0, projectId: data.project_id ?? "" };
    return {
      text: `<project-memory>\n${data.bundle}\n</project-memory>`,
      tokens: data.token_estimate,
	  projectId: data.project_id ?? "",
    };
  } catch {
    return null; // Proxy or network problem — session proceeds without memory.
  } finally {
    clearTimeout(timeout);
  }
}

async function resolveProjectIdentity(
  baseUrl: string,
  apiKey: string,
  project: string,
  repoUrl: string,
  remotes: string[],
): Promise<string> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 4000);
  try {
	const response = await fetch(`${baseUrl}/proxy/v1/projects/resolve`, {
	  method: "POST",
	  headers: { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` },
	  body: JSON.stringify({ project, repo: repoUrl || undefined, remotes: remotes.length > 0 ? remotes : undefined }),
	  signal: controller.signal,
	});
	if (!response.ok) return "";
	const body = (await response.json()) as { project_id?: string };
	return body.project_id?.trim() ?? "";
  } catch {
	return "";
  } finally {
	clearTimeout(timeout);
  }
}

// getSessionName is capability-detected: the Pi API exposes the session
// display name (set via /name) but older Pi versions do not.
function getSessionName(pi: ExtensionAPI): string {
  try {
    const fn = (pi as { getSessionName?: () => string | undefined }).getSessionName;
    if (typeof fn === "function") return (fn.call(pi) ?? "").trim();
  } catch {
    // Best-effort only; unnamed sessions fall back to first_prompt server-side.
  }
  return "";
}

function getConversationId(ctx: ExtensionContext): string {
  try {
    const sessionFile = ctx.sessionManager?.getSessionFile?.();
    if (sessionFile) return sessionIdFromSessionFile(sessionFile);
  } catch {
    // sessionManager may not have getSessionFile.
  }
  // Fallback: use the current timestamp + random suffix.
  return `pi-session-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

function sessionIdFromSessionFile(sessionFile: string): string {
  const trimmed = sessionFile.trim();
  const uuidMatch = trimmed.match(
    /([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:\.jsonl)?$/i,
  );
  if (uuidMatch) return uuidMatch[1].toLowerCase();

  const normalized = trimmed.replace(/\\/g, "/");
  const basename = normalized.split("/").pop()?.replace(/\.jsonl$/i, "") ?? "";
  const safe = basename
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 96);
  return safe ? `pi-session-${safe}` : `pi-session-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// --- Helper: get session file path ---
function getSessionFile(ctx: ExtensionContext): string {
  try {
    const sessionFile = ctx.sessionManager?.getSessionFile?.();
    if (sessionFile) return sessionFile;
  } catch {
    // ignore
  }
  return "";
}

function getSessionDir(ctx: ExtensionContext): string {
  try {
    return ctx.sessionManager?.getSessionDir?.() ?? "";
  } catch {
    return "";
  }
}

function sessionRootFromDir(sessionDir: string): string {
  const normalized = sessionDir.replace(/\\/g, "/").replace(/\/+$/, "");
  const marker = "/sessions/";
  const markerIndex = normalized.lastIndexOf(marker);
  if (markerIndex >= 0) {
    return normalized.slice(0, markerIndex + "/sessions".length);
  }
  return sessionDir;
}

interface SessionUploadIndexEntry {
  sessionId: string;
  size: number;
  mtimeMs: number;
  sha256: string;
  confirmedAt: number;
}

interface SessionUploadIndex {
  version: 1;
  files: Record<string, SessionUploadIndexEntry>;
}

interface SessionCandidate {
  path: string;
  sessionId: string;
  size: number;
  mtimeMs: number;
  sha256?: string;
  raw?: Uint8Array;
}

const SESSION_UPLOAD_RECHECK_MS = 24 * 60 * 60 * 1000;
const SESSION_UPLOAD_BATCH_SIZE = 5;
const SESSION_UPLOAD_CONCURRENCY = 2;

export class SessionLogUploader {
  private readonly indexPath: string;
  private indexPromise?: Promise<SessionUploadIndex>;
  private indexWrite: Promise<void> = Promise.resolve();
  private historicalStarted = false;
  private historicalComplete = false;
  private readonly inFlight = new Map<string, Promise<void>>();

  constructor(
    private readonly baseUrl: string,
    private readonly apiKey: string,
	private readonly getProjectIdentity: () => { project: string; projectId: string; repoUrl: string } = () => ({ project: "", projectId: "", repoUrl: "" }),
  ) {
    this.indexPath = process.env.CORP_PROXY_SESSION_UPLOAD_INDEX ??
      join(homedir(), ".pi", "agent", "corp-proxy-session-upload-index.json");
  }

  async syncHistorical(root: string): Promise<void> {
    if (this.historicalStarted || this.historicalComplete || !root) return;
    this.historicalStarted = true;
    let completed = true;

    try {
      const paths = await discoverSessionFiles(root);
      for (let offset = 0; offset < paths.length; offset += SESSION_UPLOAD_BATCH_SIZE) {
        const batchPaths = paths.slice(offset, offset + SESSION_UPLOAD_BATCH_SIZE);
        const candidates = (await mapLimit(batchPaths, SESSION_UPLOAD_CONCURRENCY, (path) =>
          this.inspect(path))).filter((item): item is SessionCandidate => item !== null);
        try {
          await this.syncCandidates(candidates);
        } catch {
          completed = false;
          break; // Proxy offline: stop the backfill and retry on a later session start.
        }
      }
    } finally {
      this.historicalStarted = false;
      this.historicalComplete = completed;
    }
  }

  syncFile(path: string): Promise<void> {
    if (!path) return Promise.resolve();
    const existing = this.inFlight.get(path);
    if (existing) return existing;

    const work = this.inspect(path)
      .then((candidate) => candidate ? this.syncCandidates([candidate]) : undefined)
      .finally(() => this.inFlight.delete(path));
    this.inFlight.set(path, work);
    return work;
  }

  private async inspect(path: string): Promise<SessionCandidate | null> {
    try {
      if (!path.toLowerCase().endsWith(".jsonl")) return null;
      const fileStat = await stat(path);
      if (!fileStat.isFile() || fileStat.size <= 0) return null;

      const index = await this.loadIndex();
      const cached = index.files[path];
      const metadataMatches = cached && cached.sessionId &&
        cached.size === fileStat.size && cached.mtimeMs === fileStat.mtimeMs;
      if (metadataMatches && Date.now() - cached.confirmedAt < SESSION_UPLOAD_RECHECK_MS) {
        return null;
      }

      const candidate: SessionCandidate = {
        path,
        sessionId: metadataMatches ? cached.sessionId : sessionIdFromSessionFile(path),
        size: fileStat.size,
        mtimeMs: fileStat.mtimeMs,
      };
      if (metadataMatches) {
        candidate.sha256 = cached.sha256;
      } else {
        await prepareCandidate(candidate);
      }
      return candidate;
    } catch {
      return null;
    }
  }

  private async syncCandidates(candidates: SessionCandidate[]): Promise<void> {
    if (candidates.length === 0) return;

    const required = await this.requiredSessionIDs(candidates);
    await mapLimit(candidates, SESSION_UPLOAD_CONCURRENCY, async (candidate) => {
      if (required.has(candidate.sessionId)) {
        await this.upload(candidate);
      }

      const after = await stat(candidate.path);
      if (after.size !== candidate.size || after.mtimeMs !== candidate.mtimeMs || !candidate.sha256) {
        return; // File changed during sync; the next heartbeat will retry it.
      }
      const index = await this.loadIndex();
      index.files[candidate.path] = {
        sessionId: candidate.sessionId,
        size: candidate.size,
        mtimeMs: candidate.mtimeMs,
        sha256: candidate.sha256,
        confirmedAt: Date.now(),
      };
    });
    await this.saveIndex();
  }

  private async requiredSessionIDs(candidates: SessionCandidate[]): Promise<Set<string>> {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10_000);
    try {
      const response = await fetch(`${this.baseUrl}/proxy/v1/session-logs/status`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.apiKey}`,
        },
        body: JSON.stringify({
          sessions: candidates.map((candidate) => ({
            session_id: candidate.sessionId,
            sha256: candidate.sha256,
          })),
        }),
        signal: controller.signal,
      });
      if (!response.ok) throw new Error(`session status HTTP ${response.status}`);
      const result = await response.json() as { required_session_ids?: string[] };
      return new Set(result.required_session_ids ?? []);
    } finally {
      clearTimeout(timeout);
    }
  }

  private async upload(candidate: SessionCandidate): Promise<void> {
    if (!candidate.sha256) return;
    const raw = candidate.raw ?? await readFile(candidate.path);
    const compressed = await gzipAsync(raw);
    const body = compressed.buffer.slice(
      compressed.byteOffset,
      compressed.byteOffset + compressed.byteLength,
    ) as ArrayBuffer;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 30_000);
    try {
	  const identity = this.getProjectIdentity();
      const response = await fetch(
        `${this.baseUrl}/proxy/v1/session-logs/${encodeURIComponent(candidate.sessionId)}`,
        {
          method: "PUT",
          headers: {
            "Content-Type": "application/x-ndjson",
            "Content-Encoding": "gzip",
            "X-Session-SHA256": candidate.sha256,
            "X-Session-Modified-Ms": String(Math.trunc(candidate.mtimeMs)),
            "X-Session-File-Name": basename(candidate.path),
			...(identity.project ? { "X-Project": identity.project } : {}),
			...(identity.projectId ? { "X-Project-ID": identity.projectId } : {}),
			...(identity.repoUrl ? { "X-Repository-Remote": identity.repoUrl } : {}),
            Authorization: `Bearer ${this.apiKey}`,
          },
          body,
          signal: controller.signal,
        },
      );
      if (!response.ok) throw new Error(`session upload HTTP ${response.status}`);
    } finally {
      clearTimeout(timeout);
    }
  }

  private loadIndex(): Promise<SessionUploadIndex> {
    if (!this.indexPromise) {
      this.indexPromise = readFile(this.indexPath, "utf8")
        .then((raw) => JSON.parse(String(raw)) as SessionUploadIndex)
        .then<SessionUploadIndex>((parsed) => parsed?.version === 1 && parsed.files
          ? parsed
          : ({ version: 1, files: {} }))
        .catch<SessionUploadIndex>(() => ({ version: 1, files: {} }));
    }
    return this.indexPromise!;
  }

  private async saveIndex(): Promise<void> {
    const index = await this.loadIndex();
    const payload = JSON.stringify(index);
    const tempPath = `${this.indexPath}.tmp`;
    this.indexWrite = this.indexWrite.then(async () => {
      await writeFile(tempPath, payload, "utf8");
      await rename(tempPath, this.indexPath);
    }).catch(() => {});
    await this.indexWrite;
  }
}

async function prepareCandidate(candidate: SessionCandidate): Promise<void> {
  const raw = await readFile(candidate.path);
  const firstLineEnd = raw.indexOf(10);
  const firstLine = raw.slice(0, firstLineEnd >= 0 ? firstLineEnd : raw.length);
  try {
    const header = JSON.parse(new TextDecoder().decode(firstLine)) as { type?: string; id?: string };
    if (header.type === "session" && header.id && /^[A-Za-z0-9._-]{1,200}$/.test(header.id)) {
      candidate.sessionId = header.id;
    }
  } catch {
    // The server performs authoritative JSONL validation and will reject an
    // invalid header. Keep the filename-derived id for the request path.
  }
  candidate.sha256 = createHash("sha256").update(raw).digest("hex");
  candidate.raw = raw;
}

function gzipAsync(raw: Uint8Array): Promise<Uint8Array> {
  return new Promise((resolve, reject) => {
    gzip(raw, (err, compressed) => {
      if (err) {
        reject(err);
        return;
      }
      resolve(compressed);
    });
  });
}

async function discoverSessionFiles(root: string): Promise<string[]> {
  const files: string[] = [];
  const walk = async (dir: string, depth: number): Promise<void> => {
    if (depth > 2) return;
    let entries;
    try {
      entries = await readdir(dir, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) {
        await walk(path, depth + 1);
      } else if (entry.isFile() && entry.name.toLowerCase().endsWith(".jsonl")) {
        files.push(path);
      }
    }
  };
  await walk(root, 0);
  return files.sort();
}

async function mapLimit<T, R>(
  items: T[],
  concurrency: number,
  worker: (item: T) => Promise<R>,
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const runners = Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    while (true) {
      const index = next++;
      if (index >= items.length) return;
      results[index] = await worker(items[index]);
    }
  });
  await Promise.all(runners);
  return results;
}

// --- Helper: get Pi version string ---
function getPiVersion(): string {
  try {
    // Pi might expose version through process or env.
    return process.env.PI_VERSION ?? "";
  } catch {
    return "";
  }
}

// --- Submit session metadata to proxy (async, fire-and-forget) ---
async function submitSession(
  baseUrl: string,
  apiKey: string,
  payload: Record<string, unknown>,
): Promise<void> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 5000); // 5s timeout

  try {
    const response = await fetch(`${baseUrl}/proxy/v1/session-submit`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${apiKey}`,
      },
      body: JSON.stringify(payload),
      signal: controller.signal,
    });
	if (!response.ok) {
	  let message = `HTTP ${response.status}`;
	  try {
		const body = (await response.json()) as { error?: { message?: string } };
		if (body.error?.message) message = body.error.message;
	  } catch {
		// Status is sufficient when the response is not JSON.
	  }
	  await reportExtensionError(baseUrl, apiKey, {
		operation: "session_submit",
		error_code: `http_${response.status}`,
		http_status: response.status,
		message,
		project_id: typeof payload.project_id === "string" ? payload.project_id : "",
		conversation_id: typeof payload.conversation_id === "string" ? payload.conversation_id : "",
	  });
	}
  } finally {
    clearTimeout(timeout);
  }
}

interface ExtensionErrorReport {
  operation: string;
  error_code: string;
  http_status?: number;
  message?: string;
  project_id?: string;
  conversation_id?: string;
}

// Monitoring is deliberately silent in the client UI. The authenticated
// server endpoint aggregates repeated failures for operator review.
async function reportExtensionError(baseUrl: string, apiKey: string, report: ExtensionErrorReport): Promise<void> {
  const signature = [report.operation, report.error_code, report.project_id, report.conversation_id].join(":");
  if (reportedExtensionErrors.has(signature)) return;

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 3000);
  try {
	const response = await fetch(`${baseUrl}/proxy/v1/extension-errors`, {
	  method: "POST",
	  headers: { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` },
	  body: JSON.stringify({ ...report, extension_version: EXTENSION_VERSION }),
	  signal: controller.signal,
	});
	if (response.ok) reportedExtensionErrors.add(signature);
  } catch {
	// The monitoring endpoint must never affect inference or session shutdown.
  } finally {
	clearTimeout(timeout);
  }
}

async function fetchQuota(baseUrl: string, apiKey: string): Promise<QuotaResponse> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 5000);
  try {
    const response = await fetch(`${baseUrl}/proxy/v1/quota`, {
      headers: { Authorization: `Bearer ${apiKey}` },
      signal: controller.signal,
    });
    if (!response.ok) {
      let detail = `HTTP ${response.status}`;
      try {
        const body = (await response.json()) as { error?: { message?: string } };
        if (body.error?.message) detail = body.error.message;
      } catch {
        // Keep the status-only error for non-JSON proxy responses.
      }
      throw new Error(detail);
    }
    return (await response.json()) as QuotaResponse;
  } finally {
    clearTimeout(timeout);
  }
}

function formatQuotaPeriod(label: string, period: QuotaPeriod, currency: string): string {
  const reset = new Date(period.resets_at).toLocaleString();
  const sharedScope =
    period.binding_scope_type === "user" || period.binding_scope_type === "tenant"
      ? formatScope(period.binding_scope_type)
      : "";
  if (period.unlimited || period.remaining_cents === null) {
    if (sharedScope) {
      return `${label}: unlimited (${sharedScope} limit · resets ${reset})`;
    }
    const used = formatCurrency(period.used_cents, currency);
    return `${label}: ${used} used · unlimited (resets ${reset})`;
  }
  if (
    sharedScope &&
    typeof period.used_percent === "number" &&
    typeof period.remaining_percent === "number"
  ) {
    return `${label}: ${period.used_percent.toFixed(1)}% used · ` +
      `${period.remaining_percent.toFixed(1)}% remaining · binding ${sharedScope} limit · resets ${reset}`;
  }
  const used = formatCurrency(period.used_cents, currency);
  const scope = period.binding_scope_type
    ? ` · binding ${formatScope(period.binding_scope_type)} limit`
    : "";
  return `${label}: ${used} used of ${formatCurrency(period.limit_cents, currency)} · ` +
    `${formatCurrency(period.remaining_cents, currency)} remaining${scope} · resets ${reset}`;
}

function formatCacheTrend(trend: CacheTrend | undefined): string[] {
  if (!trend?.available) {
    return ["Cache trend: usage history unavailable"];
  }
  const current = trend.current;
  const arrow = trend.direction === "up" ? "↑" : trend.direction === "down" ? "↓" : "→";
  const change = Math.abs(trend.cache_read_share_change_points).toFixed(1);
  const scope = trend.scope_type ? `, ${formatScope(trend.scope_type)}` : "";
  const daily = trend.daily
    .map((day) => `${day.date.slice(5)} ${day.cache_read_share_percent.toFixed(1)}%`)
    .join(" · ");
  return [
    `Cache (${trend.days}d${scope}): ${current.cache_read_share_percent.toFixed(1)}% of input read from cache ` +
      `(${arrow} ${change} pp vs prior period)`,
    `Cache hits: ${current.request_cache_hit_percent.toFixed(1)}% of requests ` +
      `(${formatInteger(current.cached_requests)}/${formatInteger(current.request_count)}) · ` +
      `${formatInteger(current.cache_read_tokens)} read tokens · ` +
      `${formatInteger(current.cache_write_tokens)} write tokens · ` +
      `${formatInteger(current.input_tokens)} uncached input tokens`,
    `Daily cache share: ${daily}`,
  ];
}

function formatCurrency(cents: number, currency: string): string {
  return new Intl.NumberFormat(undefined, {
    style: "currency",
    currency,
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(cents / 100);
}

function formatInteger(value: number): string {
  return new Intl.NumberFormat().format(value);
}

function formatScope(scope: string): string {
  if (scope === "api_key") return "API key";
  return scope.replaceAll("_", " ");
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// --- Auto-update: check for new version and self-update ---
async function checkAndUpdate(baseUrl: string, apiKey: string): Promise<void> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10000); // 10s timeout

  try {
    // Check version.
    const versionResp = await fetch(`${baseUrl}/proxy/v1/extension-version`, {
      headers: { Authorization: `Bearer ${apiKey}` },
      signal: controller.signal,
    });

    if (!versionResp.ok) return; // Server doesn't support auto-update.

    const versionInfo = (await versionResp.json()) as {
      version: string;
      changelog?: string;
      download_url?: string;
    };

    if (!versionInfo.version) return;
    if (versionInfo.version === EXTENSION_VERSION) return; // Already current.

    // The proxy is the source of truth for the extension version.
    // Any non-matching version string means an update is available.

    // Download the new extension source.
    const downloadUrl = resolveProxyURL(baseUrl, versionInfo.download_url, "/proxy/v1/extension-download");
    const downloadResp = await fetch(downloadUrl, {
      headers: { Authorization: `Bearer ${apiKey}` },
      signal: controller.signal,
    });

    if (!downloadResp.ok) return;

    const newSource = await downloadResp.text();
    if (!newSource || newSource.length < 100) return; // Sanity check.

    // Determine our own file path.
    const selfPath = await getExtensionPath();
    if (!selfPath) return;

    // Write the new version to a temp file first, then swap.
    const fs = await import("node:fs/promises");
    const path = await import("node:path");

    const dir = path.dirname(selfPath);
    const tmpPath = path.join(dir, ".corp-proxy.ts.tmp");

    await fs.writeFile(tmpPath, newSource, "utf-8");

    // Atomic rename on the same filesystem.
    await fs.rename(tmpPath, selfPath);

    console.log(
      `[corp-proxy] Auto-updated from ${EXTENSION_VERSION} to ${versionInfo.version}. ` +
        `Restart pi or run /reload to apply.`,
    );
  } finally {
    clearTimeout(timeout);
  }
}

function resolveProxyURL(baseUrl: string, maybeURL: string | undefined, fallbackPath: string): string {
  const raw = maybeURL || fallbackPath;
  try {
    return new URL(raw, baseUrl.endsWith("/") ? baseUrl : `${baseUrl}/`).toString();
  } catch {
    return `${baseUrl.replace(/\/+$/, "")}${fallbackPath}`;
  }
}

// --- Helper: find our own file path ---
async function getExtensionPath(): Promise<string | null> {
  const path = await import("node:path");
  const fs = await import("node:fs");

  // Pi loads extensions from ~/.pi/agent/extensions (and project .pi dirs);
  // the bare ~/.pi/extensions path is checked only as a legacy fallback.
  const home = process.env.HOME ?? process.env.USERPROFILE ?? "";
  const candidates = [
    path.join(home, ".pi", "agent", "extensions", "corp-proxy.ts"),
    path.join(home, ".pi", "extensions", "corp-proxy.ts"),
  ];

  for (const c of candidates) {
    if (fs.existsSync(c)) return c;
  }

  // Try to find our own file via Error stack trace (fragile but works in Node).
  try {
    const e = new Error();
    const stack = e.stack ?? "";
    const match = stack.match(/\((.*corp-proxy\.ts):\d+:\d+\)/);
    if (match) return match[1];
  } catch {
    // ignore
  }

  return null;
}

// --- Fallback model registration ---
/**
 * Fallback registration when the proxy API is unreachable. Registers a
 * sensible set of hardcoded models so Pi remains usable even without the
 * proxy running.
 */
function registerFallback(
  pi: ExtensionAPI,
  baseUrl: string,
  apiKey: string,
  sessionId: string,
	projectId = "",
) {
  // "corp", not "deepseek": Pi overrides credentials for providers named
  // after its built-ins (auth.json / DEEPSEEK_API_KEY env beat the
  // registered apiKey), which silently breaks proxy auth.
  // Model definitions mirror Pi's native DeepSeek catalog (thinking levels,
  // cost, context window) so the proxy is at full parity with direct access.
  const deepseekLevels = { minimal: null, low: "low", medium: null, high: "high", xhigh: "max", max: "max" };
  const deepseekCompat = {
    supportsDeveloperRole: false,
    supportsReasoningEffort: true,
    maxTokensField: "max_tokens",
    thinkingFormat: "deepseek",
    requiresReasoningContentOnAssistantMessages: true,
  };

  pi.registerProvider("corp", {
    // X-Session-ID lets captured payloads correlate to the session even on the
    // fallback path (see the main registration for why it's a header).
    headers: {
      "X-Extension-Version": EXTENSION_VERSION,
      ...(sessionId ? { "X-Session-ID": sessionId } : {}),
      ...(projectId ? { "X-Project-ID": projectId } : {}),
    },
    baseUrl: `${baseUrl}/deepseek/v1`,
    apiKey: apiKey,
    api: DEEPSEEK_API_TYPE,
    streamSimple: streamSimpleDeepSeekCompletions,
    models: [
      {
        id: "corp-deepseek-v4.1-flash",
        name: "DeepSeek V4.1 Flash" + MODEL_BADGE,
        reasoning: true,
        input: ["text", "image"],
        thinkingLevelMap: deepseekLevels,
        cost: { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0 },
        contextWindow: 1048576,
        maxTokens: 393216,
        compat: deepseekCompat,
      },
      {
        id: "corp-deepseek-v4-pro",
        name: "DeepSeek V4 Pro" + MODEL_BADGE,
        reasoning: true,
        input: ["text"],
        thinkingLevelMap: { ...deepseekLevels, low: null },
        cost: { input: 0.435, output: 0.87, cacheRead: 0.003625, cacheWrite: 0 },
        contextWindow: 1000000,
        maxTokens: 384000,
        compat: deepseekCompat,
      },
    ],
  });
}
