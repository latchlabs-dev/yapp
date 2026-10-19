import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { chmod, mkdir, readFile, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

type BridgeCommand = {
  id: string;
  action: string;
  params: Record<string, unknown>;
  issuedAt: number;
};

type BridgeResult = {
  id: string;
  ok: boolean;
  result?: unknown;
  error?: string;
};

type PendingCommand = {
  command: BridgeCommand;
  resolve: (value: unknown) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
  deliveredAt?: number;
};

export type BridgeOptions = {
  host?: string;
  port?: number;
  token: string;
  persistTokenPath?: string;
  now?: () => number;
};

const DEFAULT_HOST = "127.0.0.1";
const DEFAULT_PORT = Number(process.env.PI_TERMINAL_BROWSER_PORT || "17319");
const DEFAULT_TIMEOUT_MS = 30_000;
const MAX_REQUEST_BODY_BYTES = 1_048_576;
const MAX_RESULT_CHARS = 30_000;
const TOKEN_PATH = join(homedir(), ".pi", "agent", "browser-terminal", "pairing-token");
const execFileAsync = promisify(execFile);

const BLOCKED_RAW_METHODS = new Set([
  "Browser.close",
  "Network.clearBrowserCache",
  "Network.clearBrowserCookies",
  "Storage.clearDataForOrigin",
  "Target.closeTarget",
  "Target.disposeBrowserContext",
]);

export function isBlockedRawMethod(method: string): boolean {
  return BLOCKED_RAW_METHODS.has(method);
}

export class AuthenticatedBridge {
  readonly host: string;
  readonly requestedPort: number;
  readonly persistTokenPath?: string;
  private tokenValue: string;
  private readonly now: () => number;
  private server?: Server;
  private actualPort = 0;
  private pending = new Map<string, PendingCommand>();
  private lastSeenAt = 0;
  private extensionVersion = "";

  constructor(options: BridgeOptions) {
    if (!options.token || options.token.length < 32) throw new Error("Bridge token must be at least 32 characters");
    this.host = options.host ?? DEFAULT_HOST;
    if (this.host !== DEFAULT_HOST) throw new Error("Pi Terminal Browser only binds to 127.0.0.1");
    this.requestedPort = options.port ?? DEFAULT_PORT;
    this.tokenValue = options.token;
    this.persistTokenPath = options.persistTokenPath;
    this.now = options.now ?? Date.now;
  }

  get port(): number {
    return this.actualPort || this.requestedPort;
  }

  get token(): string {
    return this.tokenValue;
  }

  status() {
    const ageMs = this.lastSeenAt ? Math.max(0, this.now() - this.lastSeenAt) : null;
    return {
      host: this.host,
      port: this.port,
      running: Boolean(this.server?.listening),
      extensionConnected: ageMs !== null && ageMs < 5000,
      extensionLastSeenMs: ageMs,
      extensionVersion: this.extensionVersion || null,
      pendingCommands: this.pending.size,
    };
  }

  async start(): Promise<void> {
    if (this.server?.listening) return;
    this.server = createServer((request, response) => void this.handle(request, response));
    await new Promise<void>((resolvePromise, reject) => {
      const onError = (error: Error) => reject(error);
      this.server!.once("error", onError);
      this.server!.listen(this.requestedPort, this.host, () => {
        this.server!.off("error", onError);
        const address = this.server!.address();
        this.actualPort = typeof address === "object" && address ? address.port : this.requestedPort;
        resolvePromise();
      });
    });
  }

  async close(): Promise<void> {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(new Error("Browser bridge closed"));
    }
    this.pending.clear();
    const server = this.server;
    this.server = undefined;
    if (!server?.listening) return;
    await new Promise<void>((resolvePromise) => server.close(() => resolvePromise()));
  }

  async rotateToken(): Promise<string> {
    const next = randomBytes(32).toString("base64url");
    this.tokenValue = next;
    this.lastSeenAt = 0;
    this.extensionVersion = "";
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(new Error("Browser bridge pairing token rotated"));
    }
    this.pending.clear();
    if (this.persistTokenPath) await persistToken(this.persistTokenPath, next);
    return next;
  }

  send(
    action: string,
    params: Record<string, unknown>,
    timeoutMs = DEFAULT_TIMEOUT_MS,
    signal?: AbortSignal,
  ): Promise<unknown> {
    if (!this.server?.listening) return Promise.reject(new Error("Browser bridge is not running"));
    const id = randomBytes(16).toString("hex");
    const command: BridgeCommand = { id, action, params, issuedAt: this.now() };
    return new Promise((resolvePromise, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`Browser command ${action} timed out after ${timeoutMs}ms`));
      }, timeoutMs);
      const entry: PendingCommand = { command, resolve: resolvePromise, reject, timer };
      this.pending.set(id, entry);
      if (signal) {
        const abort = () => {
          if (!this.pending.delete(id)) return;
          clearTimeout(timer);
          reject(new Error(`Browser command ${action} was cancelled`));
        };
        if (signal.aborted) abort();
        else signal.addEventListener("abort", abort, { once: true });
      }
    });
  }

  private async handle(request: IncomingMessage, response: ServerResponse) {
    applySecurityHeaders(request, response);
    if (request.method === "OPTIONS") {
      response.writeHead(204).end();
      return;
    }
    if (request.url === "/healthz" && request.method === "GET") {
      sendJSON(response, 200, { ok: true, service: "pi-terminal-browser" });
      return;
    }
    if (!request.url?.startsWith("/v1/") || !this.authenticated(request)) {
      sendJSON(response, 401, { error: "unauthorized" });
      return;
    }
    this.lastSeenAt = this.now();
    const version = request.headers["x-pi-terminal-browser-version"];
    if (typeof version === "string") this.extensionVersion = version.slice(0, 64);
    try {
      if (request.url === "/v1/status" && request.method === "GET") {
        sendJSON(response, 200, { ok: true, protocolVersion: 1 });
        return;
      }
      if (request.url === "/v1/poll" && request.method === "GET") {
        const now = this.now();
        // Commands are deliberately at-most-once. Replaying a terminal write
        // after the extension processed it but lost the result would execute a
        // shell command twice. An uncertain delivery therefore times out and
        // requires the caller to inspect terminal state before retrying.
        const next = Array.from(this.pending.values()).find((entry) => !entry.deliveredAt);
        if (!next) {
          response.writeHead(204).end();
          return;
        }
        next.deliveredAt = now;
        sendJSON(response, 200, next.command);
        return;
      }
      if (request.url === "/v1/result" && request.method === "POST") {
        const result = asBridgeResult(await readJSON(request));
        const pending = this.pending.get(result.id);
        if (!pending) {
          sendJSON(response, 202, { accepted: false, reason: "unknown-or-complete" });
          return;
        }
        this.pending.delete(result.id);
        clearTimeout(pending.timer);
        if (result.ok) pending.resolve(result.result);
        else pending.reject(new Error(result.error || "Chrome command failed"));
        sendJSON(response, 200, { accepted: true });
        return;
      }
      sendJSON(response, 404, { error: "not_found" });
    } catch (error) {
      sendJSON(response, 400, { error: errorMessage(error) });
    }
  }

  private authenticated(request: IncomingMessage): boolean {
    const header = request.headers.authorization || "";
    const supplied = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
    const expectedBuffer = Buffer.from(this.tokenValue);
    const suppliedBuffer = Buffer.from(supplied);
    return expectedBuffer.length === suppliedBuffer.length && timingSafeEqual(expectedBuffer, suppliedBuffer);
  }
}

type Grant = { until: number; raw: boolean; timer?: ReturnType<typeof setTimeout> };
type Runtime = {
  bridge: AuthenticatedBridge;
  started: Promise<void>;
  grants: Map<string, Grant>;
};

declare global {
  // eslint-disable-next-line no-var
  var __piTerminalBrowserRuntime__: Runtime | undefined;
}

function sendJSON(response: ServerResponse, status: number, value: unknown) {
  const body = JSON.stringify(value);
  response.writeHead(status, { "Content-Type": "application/json; charset=utf-8", "Content-Length": Buffer.byteLength(body) });
  response.end(body);
}

function applySecurityHeaders(request: IncomingMessage, response: ServerResponse) {
  response.setHeader("Cache-Control", "no-store");
  response.setHeader("X-Content-Type-Options", "nosniff");
  const origin = request.headers.origin;
  if (typeof origin === "string" && /^chrome-extension:\/\/[a-p]{32}$/.test(origin)) {
    response.setHeader("Access-Control-Allow-Origin", origin);
    response.setHeader("Vary", "Origin");
    response.setHeader("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Pi-Terminal-Browser-Version");
    response.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
  }
}

async function readJSON(request: IncomingMessage): Promise<unknown> {
  const chunks: Uint8Array[] = [];
  let size = 0;
  for await (const chunk of request) {
    const bytes = typeof chunk === "string" ? Buffer.from(chunk) : chunk;
    size += bytes.length;
    if (size > MAX_REQUEST_BODY_BYTES) throw new Error("Request body is too large");
    chunks.push(bytes);
  }
  const text = Buffer.concat(chunks).toString("utf8");
  return JSON.parse(text || "null");
}

function asBridgeResult(value: unknown): BridgeResult {
  if (!value || typeof value !== "object") throw new Error("Invalid bridge result");
  const record = value as Record<string, unknown>;
  if (typeof record.id !== "string" || typeof record.ok !== "boolean") throw new Error("Invalid bridge result");
  return {
    id: record.id,
    ok: record.ok,
    result: record.result,
    error: typeof record.error === "string" ? record.error : undefined,
  };
}

async function loadOrCreateToken(path: string): Promise<string> {
  try {
    const current = (await readFile(path, "utf8")).trim();
    if (current.length >= 32) {
      await chmod(path, 0o600);
      return current;
    }
  } catch {}
  const token = randomBytes(32).toString("base64url");
  await persistToken(path, token);
  return token;
}

async function persistToken(path: string, token: string) {
  const directory = dirname(path);
  await mkdir(directory, { recursive: true, mode: 0o700 });
  await chmod(directory, 0o700);
  await writeFile(path, `${token}\n`, { encoding: "utf8", mode: 0o600 });
  await chmod(path, 0o600);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function sessionKey(ctx: any): string {
  const sessionFile = ctx?.sessionManager?.getSessionFile?.() || ctx?.sessionManager?.getSessionDir?.() || process.cwd();
  return createHash("sha256").update(String(sessionFile)).digest("hex").slice(0, 20);
}

export function parseAuthorizationDuration(input: string, now = Date.now()): { label: string; until: number } | null {
  const normalized = input.trim().toLowerCase() || "30m";
  const match = normalized.match(/^(\d{1,4})(m|min|minutes?)?$/);
  if (!match) return null;
  const minutes = Number(match[1]);
  if (!Number.isFinite(minutes) || minutes < 1 || minutes > 480) return null;
  return { label: `${minutes}m`, until: now + minutes * 60_000 };
}

async function resolveSecret(reference: string): Promise<string> {
  if (reference.startsWith("env:")) {
    const name = reference.slice(4);
    if (!/^[A-Z_][A-Z0-9_]*$/.test(name)) throw new Error("Invalid env secret reference");
    const value = process.env[name];
    if (value === undefined) throw new Error(`Secret environment variable is not set: ${name}`);
    return value;
  }
  if (reference.startsWith("keychain:")) {
    const [service, ...accountParts] = reference.slice("keychain:".length).split("/");
    const account = accountParts.join("/");
    if (!service || !account) throw new Error("Keychain references use keychain:SERVICE/ACCOUNT");
    const { stdout } = await execFileAsync("security", ["find-generic-password", "-s", service, "-a", account, "-w"], { timeout: 10_000, maxBuffer: 1_048_576 });
    return stdout.replace(/[\r\n]+$/, "");
  }
  if (reference.startsWith("1password:")) {
    const item = reference.slice("1password:".length);
    if (!item.startsWith("op://")) throw new Error("1Password references use 1password:op://vault/item/field");
    const { stdout } = await execFileAsync("op", ["read", item, "--no-newline"], { timeout: 10_000, maxBuffer: 1_048_576 });
    return stdout;
  }
  throw new Error("Unsupported secretRef. Use env:, keychain:, or 1password:.");
}

function requireGrant(runtime: Runtime, ctx: unknown, raw = false): string {
  const key = sessionKey(ctx);
  const grant = runtime.grants.get(key);
  if (!grant || grant.until <= Date.now()) {
    if (grant?.timer) clearTimeout(grant.timer);
    runtime.grants.delete(key);
    void runtime.bridge.send("bridge.detachSession", { sessionKey: key }, 3000).catch(() => {});
    throw new Error("Browser control is locked. Ask the user to run /browser authorize 30m.");
  }
  if (raw && !grant.raw) throw new Error("Raw CDP is locked. Ask the user to run /browser raw on.");
  return key;
}

function commonTarget(params: Record<string, unknown>, key: string) {
  return {
    sessionKey: key,
    ...(typeof params.tabId === "number" ? { tabId: params.tabId } : {}),
    ...(typeof params.urlIncludes === "string" && params.urlIncludes ? { urlIncludes: params.urlIncludes } : {}),
    ...(typeof params.frameId === "string" && params.frameId ? { frameId: params.frameId } : {}),
  };
}

function truncate(value: string, max = MAX_RESULT_CHARS) {
  return value.length <= max ? value : `${value.slice(0, max)}\n\n[truncated ${value.length - max} characters]`;
}

function toolResult(value: unknown) {
  const record = value && typeof value === "object" ? value as Record<string, any> : null;
  const image = record?.image || record?.value?.image;
  const withoutImage = record ? { ...record } : value;
  if (record && "image" in record) delete (withoutImage as Record<string, unknown>).image;
  const text = truncate(JSON.stringify(withoutImage, null, 2));
  return {
    content: [
      { type: "text", text },
      ...(image?.data && image?.mimeType ? [{ type: "image", data: image.data, mimeType: image.mimeType }] : []),
    ],
    details: record ? withoutImage as Record<string, unknown> : undefined,
  };
}

function commandError(error: unknown) {
  return { content: [{ type: "text", text: `Browser command failed: ${errorMessage(error)}` }], details: undefined };
}

async function textOrSecret(params: Record<string, unknown>): Promise<{ text: string; secret: boolean }> {
  const hasText = typeof params.text === "string";
  const hasSecret = typeof params.secretRef === "string" && params.secretRef.length > 0;
  if (hasText === hasSecret) throw new Error("Provide exactly one of text or secretRef");
  return hasSecret ? { text: await resolveSecret(String(params.secretRef)), secret: true } : { text: String(params.text), secret: false };
}

function registerTools(pi: ExtensionAPI, runtime: Runtime) {
  const registerTool = (pi as any).registerTool;
  if (typeof registerTool !== "function") return;

  const targetFields = {
    tabId: Type.Optional(Type.Number({ description: "Exact approved Chrome tab ID." })),
    urlIncludes: Type.Optional(Type.String({ description: "Unique substring of an approved tab URL." })),
    frameId: Type.Optional(Type.String({ description: "CDP frame ID returned by detect or inspect." })),
  };

  registerTool.call(pi, {
    name: "browser_page",
    label: "Browser page",
    description: "Inspect, evaluate JavaScript, patch/revert CSS or JS, wait, or capture an approved Chrome tab. Requires /browser authorize.",
    parameters: Type.Object({
      action: Type.Union(["inspect", "evaluate", "patch", "revert_patch", "screenshot", "wait_for"].map(Type.Literal)),
      ...targetFields,
      expression: Type.Optional(Type.String()),
      css: Type.Optional(Type.String()),
      javascript: Type.Optional(Type.String()),
      patchId: Type.Optional(Type.String()),
      persist: Type.Optional(Type.Boolean()),
      selector: Type.Optional(Type.String()),
      text: Type.Optional(Type.String()),
      timeoutMs: Type.Optional(Type.Number()),
      maxElements: Type.Optional(Type.Number()),
      fullPage: Type.Optional(Type.Boolean()),
      returnByValue: Type.Optional(Type.Boolean()),
      userGesture: Type.Optional(Type.Boolean()),
    }),
    async execute(_id: string, params: Record<string, unknown>, signal: AbortSignal, _update: unknown, ctx: unknown) {
      try {
        const key = requireGrant(runtime, ctx);
        const target = commonTarget(params, key);
        const action = String(params.action);
        let bridgeAction = `page.${action}`;
        if (action === "revert_patch") bridgeAction = "page.revertPatch";
        if (action === "wait_for") bridgeAction = "page.waitFor";
        if (action === "evaluate" && typeof params.expression !== "string") throw new Error("evaluate requires expression");
        if (action === "patch" && !params.css && !params.javascript) throw new Error("patch requires css and/or javascript");
        if (action === "revert_patch" && !params.patchId) throw new Error("revert_patch requires patchId");
        if (action === "wait_for" && !params.selector && !params.text && !params.expression) throw new Error("wait_for requires selector, text, or expression");
        const timeout = Math.min(65_000, (Number(params.timeoutMs) || DEFAULT_TIMEOUT_MS) + 2000);
        return toolResult(await runtime.bridge.send(bridgeAction, { ...params, ...target }, timeout, signal));
      } catch (error) {
        return commandError(error);
      }
    },
  });

  registerTool.call(pi, {
    name: "browser_input",
    label: "Browser input",
    description: "Send trusted CDP click, bulk text, keyboard, and scroll input to an approved Chrome tab.",
    parameters: Type.Object({
      action: Type.Union(["click", "insert_text", "press", "scroll"].map(Type.Literal)),
      ...targetFields,
      selector: Type.Optional(Type.String()),
      text: Type.Optional(Type.String()),
      secretRef: Type.Optional(Type.String()),
      key: Type.Optional(Type.String()),
      modifiers: Type.Optional(Type.Array(Type.String())),
      x: Type.Optional(Type.Number()),
      y: Type.Optional(Type.Number()),
      deltaX: Type.Optional(Type.Number()),
      deltaY: Type.Optional(Type.Number()),
    }),
    async execute(_id: string, params: Record<string, unknown>, signal: AbortSignal, _update: unknown, ctx: unknown) {
      try {
        const key = requireGrant(runtime, ctx);
        const action = String(params.action);
        const target = commonTarget(params, key);
        if (action === "click" && !params.selector) throw new Error("click requires selector");
        if (action === "press" && !params.key) throw new Error("press requires key");
        let forwarded: Record<string, unknown> = { ...params, ...target };
        let secret = false;
        if (action === "insert_text") {
          const resolved = await textOrSecret(params);
          secret = resolved.secret;
          forwarded = { ...forwarded, text: resolved.text, secretRef: undefined };
        }
        const value = await runtime.bridge.send(action === "insert_text" ? "input.insertText" : `input.${action}`, forwarded, DEFAULT_TIMEOUT_MS, signal);
        if (secret) return { content: [{ type: "text", text: "Secret inserted into the approved browser tab." }], details: { secretInserted: true } };
        return toolResult(value);
      } catch (error) {
        return commandError(error);
      }
    },
  });

  registerTool.call(pi, {
    name: "browser_terminal",
    label: "Browser terminal",
    description: "Detect, focus, write to, read, wait for, resize, or interrupt a browser-hosted TTY terminal using CDP input and xterm/WebSocket readback.",
    parameters: Type.Object({
      action: Type.Union(["detect", "focus", "write", "press", "read", "wait_for", "interrupt", "resize"].map(Type.Literal)),
      ...targetFields,
      selector: Type.Optional(Type.String()),
      text: Type.Optional(Type.String()),
      secretRef: Type.Optional(Type.String()),
      mode: Type.Optional(Type.Union([Type.Literal("fast"), Type.Literal("keys")])),
      submit: Type.Optional(Type.Boolean()),
      key: Type.Optional(Type.String()),
      modifiers: Type.Optional(Type.Array(Type.String())),
      source: Type.Optional(Type.Union(["auto", "xterm", "accessibility", "dom", "websocket"].map(Type.Literal))),
      delta: Type.Optional(Type.Boolean()),
      maxChars: Type.Optional(Type.Number()),
      maxFrames: Type.Optional(Type.Number()),
      pattern: Type.Optional(Type.String()),
      regex: Type.Optional(Type.Boolean()),
      caseSensitive: Type.Optional(Type.Boolean()),
      timeoutMs: Type.Optional(Type.Number()),
      cols: Type.Optional(Type.Number()),
      rows: Type.Optional(Type.Number()),
    }),
    async execute(_id: string, params: Record<string, unknown>, signal: AbortSignal, _update: unknown, ctx: unknown) {
      try {
        const key = requireGrant(runtime, ctx);
        const action = String(params.action);
        const actionMap: Record<string, string> = { detect: "terminal.detect", focus: "terminal.focus", write: "terminal.write", press: "terminal.press", read: "terminal.read", wait_for: "terminal.waitFor", interrupt: "terminal.interrupt", resize: "terminal.resize" };
        if (action === "press" && !params.key) throw new Error("press requires key");
        if (action === "wait_for" && typeof params.pattern !== "string") throw new Error("wait_for requires pattern");
        if (action === "resize" && (!params.cols || !params.rows)) throw new Error("resize requires cols and rows");
        let forwarded: Record<string, unknown> = { ...params, ...commonTarget(params, key) };
        let secret = false;
        if (action === "write") {
          const resolved = await textOrSecret(params);
          secret = resolved.secret;
          forwarded = { ...forwarded, text: resolved.text, secretRef: undefined };
        }
        const timeout = action === "wait_for" ? Math.min(65_000, (Number(params.timeoutMs) || 10_000) + 2000) : DEFAULT_TIMEOUT_MS;
        const value = await runtime.bridge.send(actionMap[action], forwarded, timeout, signal);
        if (secret) return { content: [{ type: "text", text: `Secret inserted into the terminal${params.submit ? " and submitted" : ""}.` }], details: { secretInserted: true, submitted: Boolean(params.submit) } };
        return toolResult(value);
      } catch (error) {
        return commandError(error);
      }
    },
  });

  registerTool.call(pi, {
    name: "browser_raw_cdp",
    label: "Browser raw CDP",
    description: "Escape hatch for one raw Chrome DevTools Protocol command. Requires /browser authorize and /browser raw on; destructive browser-wide methods are blocked.",
    parameters: Type.Object({
      ...targetFields,
      method: Type.String(),
      paramsJson: Type.Optional(Type.String({ description: "JSON object passed as CDP params." })),
      sessionId: Type.Optional(Type.String({ description: "Optional OOPIF CDP session ID." })),
    }),
    async execute(_id: string, params: Record<string, unknown>, signal: AbortSignal, _update: unknown, ctx: unknown) {
      try {
        const key = requireGrant(runtime, ctx, true);
        const method = String(params.method || "");
        if (!method.includes(".")) throw new Error("A CDP domain method is required");
        if (isBlockedRawMethod(method)) throw new Error(`Blocked destructive CDP method: ${method}`);
        let cdpParams: Record<string, unknown> = {};
        if (params.paramsJson) {
          const parsed = JSON.parse(String(params.paramsJson));
          if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("paramsJson must decode to an object");
          cdpParams = parsed;
        }
        return toolResult(await runtime.bridge.send("raw.cdp", { ...commonTarget(params, key), method, params: cdpParams, sessionId: params.sessionId }, DEFAULT_TIMEOUT_MS, signal));
      } catch (error) {
        return commandError(error);
      }
    },
  });
}

function registerCommands(pi: ExtensionAPI, runtime: Runtime, extensionPath: string) {
  const registerCommand = (pi as any).registerCommand;
  if (typeof registerCommand !== "function") return;
  registerCommand.call(pi, "browser", {
    description: "Onboard, pair, authorize, attach, inspect, and revoke Pi Terminal Browser",
    handler: async (args: string, ctx: ExtensionContext) => {
      const [subcommand = "status", ...rest] = args.trim().split(/\s+/).filter(Boolean);
      const tail = rest.join(" ");
      try {
        await runtime.started;
        if (subcommand === "onboard") {
          ctx.ui.notify(`Load this unpacked extension in Chrome:\n${extensionPath}\n\nThen run /browser pair.`, "info");
          return;
        }
        if (subcommand === "pair") {
          if (runtime.bridge.status().extensionConnected) await runtime.bridge.send("bridge.detachAll", {}, 3000).catch(() => {});
          for (const grant of runtime.grants.values()) if (grant.timer) clearTimeout(grant.timer);
          runtime.grants.clear();
          const token = await runtime.bridge.rotateToken();
          ctx.ui.notify(`Pairing token (paste it into the Chrome extension popup):\n${token}\n\nBridge: 127.0.0.1:${runtime.bridge.port}`, "info");
          return;
        }
        if (subcommand === "authorize") {
          const duration = parseAuthorizationDuration(tail);
          if (!duration) {
            ctx.ui.notify("Use /browser authorize <minutes>, from 1m to 480m.", "warning");
            return;
          }
          const confirm = (ctx.ui as any).confirm;
          if (typeof confirm === "function" && !await confirm.call(ctx.ui, "Authorize browser control", `Allow this Pi session to control explicitly approved Chrome tabs for ${duration.label}?`)) return;
          const key = sessionKey(ctx);
          const previous = runtime.grants.get(key);
          if (previous?.timer) clearTimeout(previous.timer);
          const timer = setTimeout(() => {
            runtime.grants.delete(key);
            void runtime.bridge.send("bridge.detachSession", { sessionKey: key }, 3000).catch(() => {});
          }, Math.max(0, duration.until - Date.now()));
          runtime.grants.set(key, { until: duration.until, raw: false, timer });
          ctx.ui.notify(`Browser control authorized for ${duration.label}. Raw CDP remains locked.`, "info");
          return;
        }
        if (subcommand === "attach") {
          const key = requireGrant(runtime, ctx);
          if (!tail) throw new Error("Use /browser attach <unique-url-fragment> after approving the tab in Chrome.");
          const selected = await runtime.bridge.send("tabs.select", { sessionKey: key, urlIncludes: tail }, 5000);
          ctx.ui.notify(`Selected approved tab:\n${JSON.stringify(selected, null, 2)}`, "info");
          return;
        }
        if (subcommand === "raw") {
          const key = requireGrant(runtime, ctx);
          const grant = runtime.grants.get(key)!;
          if (tail === "off") {
            grant.raw = false;
            ctx.ui.notify("Raw CDP locked.", "info");
            return;
          }
          if (tail !== "on") throw new Error("Use /browser raw on or /browser raw off.");
          const confirm = (ctx.ui as any).confirm;
          if (typeof confirm === "function" && !await confirm.call(ctx.ui, "Enable raw CDP", "Allow unrestricted, non-denylisted CDP commands for this authorized session?")) return;
          grant.raw = true;
          ctx.ui.notify("Raw CDP enabled until the browser grant expires or is revoked.", "warning");
          return;
        }
        if (subcommand === "revoke") {
          if (runtime.bridge.status().extensionConnected) await runtime.bridge.send("bridge.detachAll", {}, 3000).catch(() => {});
          for (const grant of runtime.grants.values()) if (grant.timer) clearTimeout(grant.timer);
          runtime.grants.clear();
          await runtime.bridge.rotateToken();
          ctx.ui.notify("Browser access revoked. All tab grants were detached and the pairing token was rotated.", "info");
          return;
        }
        if (subcommand === "status" || subcommand === "doctor") {
          let tabs: unknown = { unavailable: true };
          if (runtime.bridge.status().extensionConnected) tabs = await runtime.bridge.send("tabs.list", {}, 3000).catch((error) => ({ error: errorMessage(error) }));
          const grant = runtime.grants.get(sessionKey(ctx));
          const status = {
            ...runtime.bridge.status(),
            sessionAuthorized: Boolean(grant && grant.until > Date.now()),
            authorizationExpiresAt: grant && grant.until > Date.now() ? new Date(grant.until).toISOString() : null,
            rawCDP: Boolean(grant?.raw && grant.until > Date.now()),
            tabs,
            extensionPath: subcommand === "doctor" ? extensionPath : undefined,
          };
          ctx.ui.notify(JSON.stringify(status, null, 2), status.extensionConnected ? "info" : "warning");
          return;
        }
        ctx.ui.notify("Usage: /browser onboard | pair | authorize <minutes> | attach <url-fragment> | status | doctor | raw on|off | revoke", "warning");
      } catch (error) {
        ctx.ui.notify(`Browser command failed: ${errorMessage(error)}`, "error");
      }
    },
  });
}

export default async function extension(pi: ExtensionAPI) {
  const extensionPath = resolve(dirname(fileURLToPath(import.meta.url)), "..", "chrome-extension");
  let runtime = globalThis.__piTerminalBrowserRuntime__;
  if (!runtime) {
    const token = await loadOrCreateToken(TOKEN_PATH);
    const bridge = new AuthenticatedBridge({ token, port: DEFAULT_PORT, persistTokenPath: TOKEN_PATH });
    runtime = { bridge, started: bridge.start(), grants: new Map() };
    globalThis.__piTerminalBrowserRuntime__ = runtime;
  }
  registerCommands(pi, runtime, extensionPath);
  registerTools(pi, runtime);
  pi.on("session_shutdown", async (_event: unknown, ctx: unknown) => {
    const key = sessionKey(ctx);
    const grant = runtime!.grants.get(key);
    if (grant?.timer) clearTimeout(grant.timer);
    runtime!.grants.delete(key);
    if (runtime!.bridge.status().extensionConnected) {
      await runtime!.bridge.send("bridge.detachSession", { sessionKey: key }, 3000).catch(() => {});
    }
  });
}
