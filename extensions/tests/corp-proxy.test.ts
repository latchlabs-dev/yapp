import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, test } from "node:test";
import { gunzipSync } from "node:zlib";

import extension, { SessionLogUploader } from "../corp-proxy.js";

type Handler = (...args: any[]) => any;

interface RegisteredProvider {
  name: string;
  config: any;
}

class MockPi {
  providers: RegisteredProvider[] = [];
  tools: any[] = [];
  commands: Array<{ name: string; options: any }> = [];
  handlers = new Map<string, Handler[]>();

  registerProvider(name: string, config: any) {
    this.providers.push({ name, config });
  }

  registerTool(tool: any) {
    this.tools.push(tool);
  }

  registerCommand(name: string, options: any) {
    this.commands.push({ name, options });
  }

  on(event: string, handler: Handler) {
    const handlers = this.handlers.get(event) ?? [];
    handlers.push(handler);
    this.handlers.set(event, handlers);
  }

  async emit(event: string, ...args: any[]) {
    const results = [];
    for (const handler of this.handlers.get(event) ?? []) {
      results.push(await handler(...args));
    }
    return results;
  }
}

const originalFetch = globalThis.fetch;
const originalEnv = { ...process.env };

beforeEach(() => {
  process.env = { ...originalEnv };
  process.env.CORP_PROXY_KEY = "test-proxy-key";
  process.env.CORP_PROXY_BASE_URL = "https://proxy.test";
  process.env.CORP_PROXY_PROJECT = "Piproxy Tests";
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  delete (globalThis as any).__piAiStreamSimpleImpl;
  delete (globalThis as any).__piAiStreamSimpleCalls;
  delete (globalThis as any).__piAiCodexStreamSimpleImpl;
  delete (globalThis as any).__piAiCodexStreamSimpleCalls;
  process.env = { ...originalEnv };
});

test("unconfigured source extension contacts only the local quickstart", async () => {
  delete process.env.CORP_PROXY_BASE_URL;
  const requested: string[] = [];
  globalThis.fetch = async (input: string | URL | Request) => {
    requested.push(String(input));
    return jsonResponse({ providers: [] });
  };
  const pi = new MockPi();
  await extension(pi as any);
  try {
    assert.ok(requested.some((url) => url.endsWith("/proxy/v1/models")));
    assert.ok(requested.every((url) => new URL(url).origin === "http://localhost:18080"));
  } finally {
    await shutdown(pi);
  }
});

test("registers discovered models with per-model Pi compat fields", async () => {
  globalThis.fetch = async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({
        providers: [
          {
            name: "corp",
            base_url: "/deepseek/v1",
            api_type: "deepseek-completions",
            compat: {
              supports_developer_role: false,
              supports_reasoning_effort: true,
              max_tokens_field: "max_tokens",
            },
            models: [
              {
                id: "corp-deepseek-v4-flash",
                name: "DeepSeek V4 Flash",
                reasoning: true,
                input: ["text"],
                thinking_level_map: { minimal: null, high: "high", xhigh: "max" },
                cost: { input: 0.14, output: 0.28, cache_read: 0.0028, cache_write: 0 },
                context_window: 1000000,
                max_tokens: 384000,
                compat: {
                  thinking_format: "deepseek",
                  requires_reasoning_content_on_assistant_messages: true,
                },
              },
            ],
          },
        ],
      });
    }
    if (url.startsWith("https://proxy.test/proxy/v1/context?")) {
      return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
    }
    return jsonResponse({}, 404);
  };

  const pi = new MockPi();
  await extension(pi as any);

  assert.equal(pi.providers.length, 1);
  assert.equal(pi.providers[0].name, "corp");
  assert.equal(pi.providers[0].config.baseUrl, "https://proxy.test/deepseek/v1");
  assert.equal(pi.providers[0].config.apiKey, "$CORP_PROXY_KEY");
  assert.equal(pi.providers[0].config.api, "deepseek-completions");
  assert.equal(typeof pi.providers[0].config.streamSimple, "function");

  const model = pi.providers[0].config.models[0];
  assert.equal(model.name, "DeepSeek V4 Flash (corp)");
  assert.deepEqual(model.thinkingLevelMap, { minimal: null, high: "high", xhigh: "max" });
  assert.deepEqual(model.cost, { input: 0.14, output: 0.28, cacheRead: 0.0028, cacheWrite: 0 });
  assert.deepEqual(model.compat, {
    supportsDeveloperRole: false,
    supportsReasoningEffort: true,
    maxTokensField: "max_tokens",
    thinkingFormat: "deepseek",
    requiresReasoningContentOnAssistantMessages: true,
  });

  await shutdown(pi);
});

test("/quota shows live balances and cache usage trends", async () => {
  let quotaAuthorization = "";
  globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.20" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({ providers: [] });
    }
    if (url.endsWith("/proxy/v1/quota")) {
      quotaAuthorization = new Headers(init?.headers).get("Authorization") ?? "";
      return jsonResponse({
        currency: "USD",
        as_of: "2026-07-24T12:00:00Z",
        daily: {
          used_cents: 200,
          used_percent: 80,
          limit_cents: 250,
          remaining_cents: 50,
          remaining_percent: 20,
          resets_at: "2026-07-25T00:00:00Z",
          unlimited: false,
          exhausted: false,
          binding_scope_type: "user",
        },
        monthly: {
          used_cents: 7000,
          used_percent: 87.5,
          limit_cents: 8000,
          remaining_cents: 1000,
          remaining_percent: 12.5,
          resets_at: "2026-08-01T00:00:00Z",
          unlimited: false,
          exhausted: false,
          binding_scope_type: "tenant",
        },
        cache_trend: {
          available: true,
          scope_type: "user",
          days: 7,
          current: {
            request_count: 4,
            cached_requests: 3,
            input_tokens: 200,
            cache_read_tokens: 400,
            cache_write_tokens: 20,
            cache_read_share_percent: 66.7,
            request_cache_hit_percent: 75,
          },
          previous: {
            request_count: 2,
            cached_requests: 1,
            input_tokens: 300,
            cache_read_tokens: 100,
            cache_write_tokens: 0,
            cache_read_share_percent: 25,
            request_cache_hit_percent: 50,
          },
          cache_read_share_change_points: 41.7,
          direction: "up",
          daily: [
            {
              date: "2026-07-24",
              request_count: 2,
              cached_requests: 2,
              input_tokens: 100,
              cache_read_tokens: 100,
              cache_write_tokens: 0,
              cache_read_share_percent: 50,
              request_cache_hit_percent: 100,
            },
          ],
        },
      });
    }
    return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
  };

  const pi = new MockPi();
  await extension(pi as any);
  const command = pi.commands.find((item) => item.name === "quota");
  assert.ok(command);

  const notifications: Array<{ message: string; type: string }> = [];
  const statuses: Array<string | undefined> = [];
  await command!.options.handler("", {
    ui: {
      notify(message: string, type: string) {
        notifications.push({ message, type });
      },
      setStatus(_key: string, value: string | undefined) {
        statuses.push(value);
      },
    },
  });

  assert.equal(quotaAuthorization, "Bearer test-proxy-key");
  assert.equal(notifications.length, 1);
  assert.equal(notifications[0].type, "info");
  assert.match(notifications[0].message, /Today: 80\.0% used · 20\.0% remaining · binding user limit/);
  assert.match(notifications[0].message, /This month: 87\.5% used · 12\.5% remaining · binding tenant limit/);
  assert.equal(notifications[0].message.includes("$"), false);
  assert.match(notifications[0].message, /66\.7% of input read from cache/);
  assert.match(notifications[0].message, /41\.7 pp vs prior period/);
  assert.match(notifications[0].message, /Daily cache share: 07-24 50\.0%/);
  assert.deepEqual(statuses, ["Checking quota...", undefined]);

  await shutdown(pi);
});

test("adds session and canonical project IDs to registered providers", async () => {
  globalThis.fetch = async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({
        providers: [
          {
            name: "corp",
            base_url: "/deepseek/v1",
            api_type: "deepseek-completions",
            route_id: "provider-account-corp",
            models: [
              {
                id: "m",
                name: "Model",
                reasoning: false,
                input: ["text"],
                context_window: 100,
                max_tokens: 50,
              },
            ],
          },
        ],
      });
    }
	if (url.endsWith("/proxy/v1/projects/resolve")) {
	  return jsonResponse({ project_id: "mprj_01K2PROJECTATTRIBUTION000000" });
	}
    return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
  };

  const pi = new MockPi();
  await extension(pi as any);

  await pi.emit(
    "session_start",
    { reason: "new" },
    {
      ui: { notify() {} },
      sessionManager: {
        getSessionFile: () => "/tmp/019c6e27-e55b-73d1-87d8-4e01f1f75043.jsonl",
        getEntries: () => [],
      },
    },
  );

  assert.equal(pi.providers.length, 2);
  assert.equal(typeof pi.providers[1].config.streamSimple, "function");
  assert.equal(
    pi.providers[1].config.headers["X-Session-ID"],
    "019c6e27-e55b-73d1-87d8-4e01f1f75043",
  );
  assert.equal(
    pi.providers[1].config.headers["X-Corp-Provider-ID"],
    "provider-account-corp",
  );
	assert.equal(
	  pi.providers[1].config.headers["X-Project-ID"],
	  "mprj_01K2PROJECTATTRIBUTION000000",
	);
	assert.equal(pi.providers[1].config.headers["X-Extension-Version"], "1.3.26");

  await shutdown(pi);
});

test("silently reports failed session submissions to extension monitoring", async () => {
	const reports: any[] = [];
	const notifications: string[] = [];
	globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
	  const url = String(input);
	  if (url.endsWith("/proxy/v1/extension-version")) return jsonResponse({ version: "1.3.26" });
	  if (url.endsWith("/proxy/v1/models")) return jsonResponse({ providers: [] });
	  if (url.endsWith("/proxy/v1/projects/resolve")) return jsonResponse({ project_id: "mprj_monitoring" });
	  if (url.endsWith("/proxy/v1/context")) return jsonResponse({ inject: false, bundle: "", token_estimate: 0, project_id: "mprj_monitoring" });
	  if (url.endsWith("/proxy/v1/session-submit")) {
		return jsonResponse({ error: { type: "forbidden", message: "missing required scope: sessions:submit" } }, 403);
	  }
	  if (url.endsWith("/proxy/v1/extension-errors")) {
		reports.push(JSON.parse(String(init?.body)));
		return new Response(null, { status: 202 });
	  }
	  return jsonResponse({}, 404);
	};

	const pi = new MockPi();
	await extension(pi as any);
	const ctx = {
	  ui: { notify(message: string) { notifications.push(message); } },
	  sessionManager: {
		getSessionFile: () => "/tmp/019c6e27-e55b-73d1-87d8-4e01f1f79999.jsonl",
		getEntries: () => [],
	  },
	};
	await pi.emit("session_start", { reason: "new" }, ctx);
	await pi.emit("session_shutdown", {}, ctx);

	assert.equal(reports.length, 1);
	assert.equal(reports[0].operation, "session_submit");
	assert.equal(reports[0].error_code, "http_403");
	assert.equal(reports[0].http_status, 403);
	assert.equal(reports[0].project_id, "mprj_monitoring");
	assert.equal(reports[0].conversation_id, "019c6e27-e55b-73d1-87d8-4e01f1f79999");
	assert.equal(reports[0].extension_version, "1.3.26");
	assert.deepEqual(notifications, []);
});

test("ZAI streamSimple preserves GLM thinking state without touching global completions", async () => {
  let sentBody: any = null;

  globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({
        providers: [
          {
            name: "ZAI",
            base_url: "/openai/v1",
            api_type: "openai-completions",
            compat: {
              supports_developer_role: false,
              supports_reasoning_effort: true,
              max_tokens_field: "max_tokens",
            },
            models: [
              {
                id: "corp-glm-5.2",
                name: "GLM-5.2",
                reasoning: true,
                input: ["text"],
                thinking_level_map: { minimal: null, high: "enabled" },
                cost: { input: 1.4, output: 4.4, cache_read: 0.26, cache_write: 0 },
                context_window: 1000000,
                max_tokens: 131072,
                compat: {
                  thinking_format: "zai",
                },
              },
            ],
          },
        ],
      });
    }
    if (url.startsWith("https://proxy.test/proxy/v1/context?")) {
      return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
    }
    if (url === "https://proxy.test/openai/v1/chat/completions") {
      sentBody = JSON.parse(String(init?.body));
      return jsonResponse({});
    }
    return jsonResponse({}, 404);
  };

  (globalThis as any).__piAiStreamSimpleImpl = async function* (model: any) {
    await fetch("https://proxy.test/openai/v1/chat/completions", {
      method: "POST",
      body: JSON.stringify({
        model: model.id,
        messages: [{ role: "user", content: "hello" }],
        thinking: { type: "enabled" },
      }),
    });
  };

  const pi = new MockPi();
  await extension(pi as any);

  const provider = pi.providers.find((registered) => registered.name === "ZAI");
  assert.ok(provider);
  assert.equal(typeof provider!.config.streamSimple, "function");
  assert.equal(provider!.config.models[0].compat?.thinkingFormat, "zai");

  for await (const _ of provider!.config.streamSimple!(provider!.config.models[0], {}, {})) {
    // Drain the stream so the fake Pi helper issues its request.
  }

  assert.deepEqual(sentBody.thinking, { type: "enabled", clear_thinking: false });

  await shutdown(pi);
});

test("DeepSeek adapter strips provider streamSimple before delegating to Pi", async () => {
  let delegatedModel: any = null;

  globalThis.fetch = async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({
        providers: [
          {
            name: "corp",
            base_url: "/deepseek/v1",
            api_type: "deepseek-completions",
            models: [
              {
                id: "corp-deepseek-v4-flash",
                name: "DeepSeek V4 Flash",
                reasoning: true,
                input: ["text"],
                context_window: 1000000,
                max_tokens: 384000,
              },
            ],
          },
        ],
      });
    }
    if (url.startsWith("https://proxy.test/proxy/v1/context?")) {
      return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
    }
    return jsonResponse({}, 404);
  };

  (globalThis as any).__piAiStreamSimpleImpl = (model: any) => {
    delegatedModel = model;
    if (typeof model.streamSimple === "function") {
      throw new Error("recursive streamSimple redispatch");
    }
    return { [Symbol.asyncIterator]: async function* () {} };
  };

  const pi = new MockPi();
  await extension(pi as any);

  const provider = pi.providers.find((registered) => registered.name === "corp");
  assert.ok(provider);
  assert.equal(typeof provider!.config.streamSimple, "function");

  const selectedModel = {
    ...provider!.config.models[0],
    streamSimple: provider!.config.streamSimple,
  };
  for await (const _ of provider!.config.streamSimple!(selectedModel, {}, {})) {
    // Drain the stream.
  }

  assert.equal(delegatedModel.api, "openai-completions");
  assert.equal(delegatedModel.streamSimple, undefined);

  await shutdown(pi);
});

test("Codex proxy streamSimple rewrites local sentinel auth to the proxy key", async () => {
  let sentAuth = "";
  let sentModel = "";
  let codexOptions: any = null;

  globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({
        providers: [
          {
            name: "OpenAI Codex",
            base_url: "/codex",
            api_type: "openai-codex-responses",
            models: [
              {
                id: "corp-gpt-5.4-mini",
                name: "GPT-5.4 mini",
                reasoning: true,
                input: ["text"],
                cost: { input: 0.75, output: 4.5, cache_read: 0.075, cache_write: 0 },
                context_window: 272000,
                max_tokens: 128000,
              },
            ],
          },
        ],
      });
    }
    if (url.startsWith("https://proxy.test/proxy/v1/context?")) {
      return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
    }
    if (url === "https://proxy.test/codex/responses") {
      sentAuth = new Headers(init?.headers).get("Authorization") ?? "";
      return jsonResponse({});
    }
    return jsonResponse({}, 404);
  };

  (globalThis as any).__piAiCodexStreamSimpleImpl = async function* (model: any, _context: any, options: any) {
    codexOptions = options;
    const payload = await options.onPayload({ model: model.id, input: [] }, model);
    sentModel = payload.model;
    await fetch(`${model.baseUrl}/responses`, {
      method: "POST",
      headers: { Authorization: `Bearer ${options.apiKey}` },
      body: "{}",
    });
  };

  const pi = new MockPi();
  await extension(pi as any);

  const provider = pi.providers.find((registered) => registered.name === "openai-codex");
  assert.ok(provider);
  assert.equal(provider!.config.name, "OpenAI Codex");
  assert.equal(provider!.config.baseUrl, "https://proxy.test/codex");
  assert.equal(typeof provider!.config.streamSimple, "function");
  assert.equal(provider!.config.models[0].id, "corp-gpt-5.4-mini");

  for await (const _ of provider!.config.streamSimple!(provider!.config.models[0], {}, {})) {
    // Drain the stream so the fake Codex helper issues its request.
  }

  assert.equal(sentAuth, "Bearer test-proxy-key");
  assert.equal(sentModel, "gpt-5.4-mini");
  assert.equal(codexOptions.transport, "sse");
  assert.ok(codexOptions.apiKey.includes("."));

  await shutdown(pi);
});

test("falls back to hardcoded corp models when model discovery fails", async () => {
  globalThis.fetch = async (input: string | URL | Request) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "1.3.13" });
    }
    return jsonResponse({ error: "nope" }, 503);
  };

  const pi = new MockPi();
  await extension(pi as any);
  await pi.emit(
    "session_start",
    { reason: "new" },
    {
      ui: { notify() {} },
      sessionManager: {
        getSessionFile: () => "/tmp/session.jsonl",
        getEntries: () => [],
      },
    },
  );

  const fallback = pi.providers.find((provider) => provider.name === "corp");
  assert.ok(fallback);
  assert.equal(fallback!.config.baseUrl, "https://proxy.test/deepseek/v1");
  assert.equal(typeof fallback!.config.streamSimple, "function");
  assert.equal(fallback!.config.headers["X-Session-ID"], "pi-session-session");
  assert.equal(fallback!.config.models[0].id, "corp-deepseek-v4.1-flash");
  assert.deepEqual(fallback!.config.models[0].input, ["text", "image"]);
  assert.deepEqual(fallback!.config.models[0].cost, { input: 0.30, output: 1.20, cacheRead: 0.006, cacheWrite: 0 });
  assert.equal(fallback!.config.models[0].compat.supportsDeveloperRole, false);

  await shutdown(pi);
});

test("auto-update resolves relative download URLs against proxy base URL", async () => {
  const seen: string[] = [];
  globalThis.fetch = async (input: string | URL | Request) => {
    const url = String(input);
    seen.push(url);
    if (url.endsWith("/proxy/v1/extension-version")) {
      return jsonResponse({ version: "9.9.9", download_url: "/proxy/v1/extension-download" });
    }
    if (url === "https://proxy.test/proxy/v1/extension-download") {
      return jsonResponse({ error: "skip write" }, 503);
    }
    if (url.endsWith("/proxy/v1/models")) {
      return jsonResponse({ providers: [] });
    }
    return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
  };

  const pi = new MockPi();
  await extension(pi as any);

  assert.ok(seen.includes("https://proxy.test/proxy/v1/extension-download"));
  await shutdown(pi);
});

test("session uploader hashes and uploads only new or changed JSONL files", async () => {
  const root = await mkdtemp(join(tmpdir(), "piproxy-session-upload-"));
  const projectDir = join(root, "--demo--");
  const sessionID = "019f471a-7fb8-7720-b913-c3bbd06610d1";
  const sessionPath = join(projectDir, `2026-07-10T10-00-00-000Z_${sessionID}.jsonl`);
  const indexPath = join(root, "upload-index.json");
  process.env.CORP_PROXY_SESSION_UPLOAD_INDEX = indexPath;
  await (await import("node:fs/promises")).mkdir(projectDir, { recursive: true });
  await writeFile(sessionPath, sessionJSONL(sessionID, "first"), "utf8");

  let statusCalls = 0;
  const uploads: string[] = [];
	let uploadHeaders = new Headers();
  const requestOrder: string[] = [];
  globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/proxy/v1/session-logs/status")) {
      requestOrder.push("status");
      statusCalls++;
      const body = JSON.parse(String(init?.body)) as { sessions: Array<{ session_id: string }> };
      return jsonResponse({ required_session_ids: body.sessions.map((item) => item.session_id) });
    }
    if (url.includes("/proxy/v1/session-logs/")) {
      requestOrder.push("upload");
      uploads.push(new TextDecoder().decode(gunzipSync(new Uint8Array(init?.body as ArrayBuffer))));
	  uploadHeaders = new Headers(init?.headers);
      return jsonResponse({ status: "stored" }, 201);
    }
    return jsonResponse({}, 404);
  };

  try {
	const uploader = new SessionLogUploader("https://proxy.test", "test-key", () => ({
	  project: "example-app",
	  projectId: "mprj_shared",
	  repoUrl: "git@github.com:example-org/example-app.git",
	}));
    await uploader.syncHistorical(root);
    assert.equal(statusCalls, 1);
    assert.equal(uploads.length, 1);
    assert.deepEqual(requestOrder, ["status", "upload"]);
    assert.ok(uploads[0].includes("first"));
	assert.equal(uploadHeaders.get("X-Project-ID"), "mprj_shared");
	assert.equal(uploadHeaders.get("X-Project"), "example-app");
	assert.equal(uploadHeaders.get("X-Repository-Remote"), "git@github.com:example-org/example-app.git");

    // The confirmed local metadata cache avoids even a hash/status check.
    await uploader.syncFile(sessionPath);
    assert.equal(statusCalls, 1);
    assert.equal(uploads.length, 1);

    await writeFile(sessionPath, sessionJSONL(sessionID, "changed content"), "utf8");
    await uploader.syncFile(sessionPath);
    assert.equal(statusCalls, 2);
    assert.equal(uploads.length, 2);
    assert.ok(uploads[1].includes("changed content"));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("session uploader defers asynchronous gzip until upload is required", async () => {
  const source = await readFile(join(process.cwd(), "extensions", "corp-proxy.ts"), "utf8");
  assert.equal(/\bgzipSync\b/.test(source), false);

  const prepareStart = source.indexOf("async function prepareCandidate");
  const gzipHelperStart = source.indexOf("function gzipAsync", prepareStart);
  assert.ok(prepareStart >= 0 && gzipHelperStart > prepareStart);
  assert.equal(/\bgzip(?:Async)?\s*\(/.test(source.slice(prepareStart, gzipHelperStart)), false);

  const uploadStart = source.indexOf("private async upload");
  const loadIndexStart = source.indexOf("private loadIndex", uploadStart);
  assert.ok(uploadStart >= 0 && loadIndexStart > uploadStart);
  assert.equal(/await gzipAsync\(raw\)/.test(source.slice(uploadStart, loadIndexStart)), true);
});

test("session upload and historical backfill are explicit opt-ins", async () => {
	const source = await readFile(join(process.cwd(), "extensions", "corp-proxy.ts"), "utf8");
	assert.ok(/CORP_PROXY_ENABLE_SESSION_UPLOAD === "1"/.test(source));
	assert.ok(/CORP_PROXY_ENABLE_HISTORICAL_SESSION_UPLOAD === "1"/.test(source));
});

test("context hints and repository remotes use a POST body", async () => {
	const source = await readFile(join(process.cwd(), "extensions", "corp-proxy.ts"), "utf8");
	const start = source.indexOf("async function fetchContextBundle");
	const end = source.indexOf("function getConversationId", start);
	const implementation = source.slice(start, end);
	assert.ok(/fetch\(`\$\{baseUrl\}\/proxy\/v1\/context`,/.test(implementation));
	assert.ok(/method: "POST"/.test(implementation));
	assert.ok(/body: JSON\.stringify\(\{ project, repo:/.test(implementation));
	assert.ok(/remotes: remotes\.length/.test(implementation));
	assert.ok(/projectId: data\.project_id/.test(implementation));
	assert.equal(/\/proxy\/v1\/context\?/.test(implementation), false);
});

test("registers project memory tools and injects a one-line reminder at the end of the master prompt", async () => {
	const requests: Array<{ url: string; body: any }> = [];
	globalThis.fetch = async (input: string | URL | Request, init?: RequestInit) => {
		const url = String(input);
		const body = init?.body ? JSON.parse(String(init.body)) : undefined;
		if (body) requests.push({ url, body });
		if (url.endsWith("/proxy/v1/extension-version")) {
			return jsonResponse({ version: "1.3.17" });
		}
		if (url.endsWith("/proxy/v1/models")) {
			return jsonResponse({ providers: [] });
		}
		if (url.endsWith("/proxy/v1/observations")) {
			return jsonResponse({ status: "recorded" }, 201);
		}
		if (url.endsWith("/proxy/v1/context")) {
			return jsonResponse({
				project: "piproxy-tests",
				project_id: "mprj_test",
				inject: true,
				bundle: "# Project memory: piproxy-tests\n\nUse pnpm.",
				token_estimate: 12,
				degraded: false,
			});
		}
		return jsonResponse({}, 404);
	};

	const pi = new MockPi();
	await extension(pi as any);
	assert.deepEqual(pi.tools.map((tool) => tool.name), ["record_memory", "retrieve_memories"]);

	const record = pi.tools.find((tool) => tool.name === "record_memory");
	await record.execute("call-1", { kind: "decision", content: "The user chose pnpm." });
	const observation = requests.find((request) => request.url.endsWith("/proxy/v1/observations"));
	assert.equal(observation?.body.kind, "decision");
	assert.equal(observation?.body.content, "The user chose pnpm.");

	const retrieve = pi.tools.find((tool) => tool.name === "retrieve_memories");
	const result = await retrieve.execute("call-2", { query: "package manager" });
	assert.match(result.content[0].text, /Use pnpm/);
	const retrieval = requests.find((request) => request.url.endsWith("/proxy/v1/context") && request.body.retrieve);
	assert.equal(retrieval?.body.hint, "package manager");

	await pi.emit(
		"session_start",
		{ reason: "new" },
		{
			ui: { notify() {} },
			sessionManager: {
				getSessionFile: () => "/tmp/memory-guidance.jsonl",
				getEntries: () => [],
			},
		},
	);
	await new Promise<void>((resolve) => setTimeout(resolve, 0));

	const promptResults = await pi.emit("before_agent_start", {
		prompt: "Which package manager?",
		systemPrompt: "Base prompt",
	});
	const guidance = "Project memory tools: proactively use retrieve_memories to read relevant shared project context, and use record_memory to write durable project-specific lessons, mistakes, lookup pointers, and user-confirmed decisions; never store secrets, personal data, session trivia, or agent-inferred decisions.";
	assert.match(promptResults[0].systemPrompt, /Use pnpm\./);
	assert.ok(promptResults[0].systemPrompt.indexOf("Use pnpm.") < promptResults[0].systemPrompt.indexOf(guidance));
	assert.ok(promptResults[0].systemPrompt.endsWith(guidance));
	assert.equal(guidance.includes("\n"), false);

	await shutdown(pi);
});

test("record_memory explains a missing memory scope on HTTP 403", async () => {
	globalThis.fetch = async (input: string | URL | Request) => {
		const url = String(input);
		if (url.endsWith("/proxy/v1/extension-version")) {
			return jsonResponse({ version: "1.3.21" });
		}
		if (url.endsWith("/proxy/v1/models")) {
			return jsonResponse({ providers: [] });
		}
		if (url.endsWith("/proxy/v1/observations")) {
			return jsonResponse({
				error: { type: "forbidden", message: "insufficient permissions" },
			}, 403);
		}
		if (url.endsWith("/proxy/v1/context")) {
			return jsonResponse({ inject: false, bundle: "", token_estimate: 0, degraded: false });
		}
		return jsonResponse({}, 404);
	};

	const pi = new MockPi();
	await extension(pi as any);

	const record = pi.tools.find((tool) => tool.name === "record_memory");
	const result = await record.execute("call-403", {
		kind: "lesson",
		content: "Memory permissions should be actionable.",
	});
	assert.match(result.content[0].text, /HTTP 403/);
	assert.match(result.content[0].text, /memory:observe/);
	assert.match(result.content[0].text, /administrator/);

	await shutdown(pi);
});

function sessionJSONL(sessionID: string, text: string): string {
  return `${JSON.stringify({
    type: "session",
    version: 3,
    id: sessionID,
    timestamp: "2026-07-10T10:00:00Z",
    cwd: "/code/demo",
  })}\n${JSON.stringify({
    type: "message",
    id: "m1",
    parentId: null,
    timestamp: "2026-07-10T10:00:01Z",
    message: { role: "user", content: [{ type: "text", text }] },
  })}\n`;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

async function shutdown(pi: MockPi) {
  await pi.emit("session_shutdown", {}, {
    ui: { notify() {} },
    sessionManager: {
      getSessionFile: () => "/tmp/shutdown.jsonl",
      getEntries: () => [],
    },
  });
}
