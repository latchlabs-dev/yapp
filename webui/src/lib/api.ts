import type {
  Attachment,
  AuthConfig,
  BundlePreview,
  Conversation,
  Me,
  Message,
  Project,
  ProjectMemory,
  ProvidersResponse,
  QuotaResponse,
  UploadTicket,
} from "./types";

const BASE = "/web/v1";

/** Raised for any non-2xx API response, carrying the server's error type. */
export class ApiError extends Error {
  status: number;
  type: string;

  constructor(status: number, type: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.type = type;
  }
}

// The CSRF token is handed out by /me and echoed on every write. It lives in
// memory only: persisting it would outlive the session it belongs to.
let csrfToken = "";

export function setCsrfToken(token: string) {
  csrfToken = token;
}

/** Fired when the server rejects the session, so the app can bounce to login. */
export const SESSION_EXPIRED_EVENT = "piproxy-web-session-expired";

function notifySessionExpired() {
  window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT));
}

async function request<T>(method: string, path: string, body?: unknown, expireOnUnauthorized = true): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && csrfToken) headers["X-Web-CSRF"] = csrfToken;

  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
    // The session is a cookie, so credentials must ride along. The app and the
    // API share an origin in every deployment, which is what lets the cookie
    // stay SameSite=Lax.
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (!res.ok) {
    const parsed = await res.json().catch(() => null);
    if (res.status === 401 && expireOnUnauthorized) notifySessionExpired();
    throw new ApiError(
      res.status,
      parsed?.error?.type ?? "unknown",
      parsed?.error?.message ?? res.statusText,
    );
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

export const api = {
  authConfig: () => request<AuthConfig>("GET", "/auth/config"),
  loginWithKey: (key: string) => request<{ status: string }>("POST", "/auth/key", { key }),
  me: (expireOnUnauthorized = true) => request<Me>("GET", "/me", undefined, expireOnUnauthorized),
  // A failed logout is NOT proof that this browser's cookie was cleared.
  logout: () => request<{ status: string }>("POST", "/auth/logout", undefined, false),

  models: () => request<ProvidersResponse>("GET", "/models"),
  quota: () => request<QuotaResponse>("GET", "/quota"),
  projects: () => request<{ projects: Project[] }>("GET", "/projects"),

  conversations: () => request<{ conversations: Conversation[] | null }>("GET", "/conversations"),
  createConversation: (input: Partial<Conversation>) =>
    request<Conversation>("POST", "/conversations", input),
  conversation: (id: string) =>
    request<{ conversation: Conversation; messages: Message[] | null }>("GET", `/conversations/${id}`),
  updateConversation: (id: string, input: Partial<Conversation>) =>
    request<Conversation>("PATCH", `/conversations/${id}`, input),
  archiveConversation: (id: string) =>
    request<{ status: string }>("DELETE", `/conversations/${id}`),
  remember: (conversationId: string, messageId: string) =>
    request<{ status: string }>("POST", `/conversations/${conversationId}/messages/${messageId}/remember`),

  projectMemory: (projectId: string) =>
    request<ProjectMemory>("GET", `/projects/${projectId}/memory`),
  saveProjectMemory: (projectId: string, content: string) =>
    request<{ status: string; memory_tokens: number }>("PUT", `/projects/${projectId}/memory`, { content }),
  appendLesson: (projectId: string, content: string) =>
    request<{ status: string }>("POST", `/projects/${projectId}/lessons`, { content }),
  bundlePreview: (projectId: string) =>
    request<BundlePreview>("GET", `/projects/${projectId}/bundle`),
  searchMemory: (query: string) =>
    request<{ results: { path: string; title: string; snippet: string }[]; degraded: boolean }>(
      "GET",
      `/memory/search?q=${encodeURIComponent(query)}`,
    ),

  reserveAttachment: (input: {
    filename: string;
    content_type: string;
    size_bytes: number;
    conversation_id?: string;
  }) => request<UploadTicket>("POST", "/attachments", input),
  finalizeAttachment: (id: string) =>
    request<Attachment>("POST", `/attachments/${id}/finalize`),
  attachmentDownloadUrl: (id: string) => `${BASE}/attachments/${id}`,
};

/**
 * uploadAttachment reserves an upload, PUTs the bytes straight to object
 * storage, and finalizes.
 *
 * The bytes never pass through the proxy, so a large file costs the data plane
 * two small JSON calls. The signed URL binds the content type, reserved byte
 * range and create-only precondition, so the PUT sends every returned header.
 */
export async function uploadAttachment(file: File, conversationId?: string): Promise<Attachment> {
  const ticket = await api.reserveAttachment({
    filename: file.name,
    content_type: file.type || "application/octet-stream",
    size_bytes: file.size,
    conversation_id: conversationId,
  });

  const res = await fetch(ticket.upload_url, {
    method: ticket.method,
    headers: ticket.headers,
    body: file,
  });
  if (!res.ok) {
    throw new ApiError(res.status, "upload_failed", `upload of ${file.name} failed (${res.status})`);
  }
  return api.finalizeAttachment(ticket.id);
}

/**
 * sendMessage opens the turn's event stream. The caller gets the raw Response
 * so it can drive readEventStream and abort mid-turn.
 */
export async function sendMessage(
  conversationId: string,
  input: { content: string; attachment_ids?: string[]; model?: string; provider_id?: string },
  signal?: AbortSignal,
): Promise<Response> {
  const res = await fetch(`${BASE}/conversations/${conversationId}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Web-CSRF": csrfToken },
    credentials: "same-origin",
    body: JSON.stringify(input),
    signal,
  });
  if (!res.ok) {
    const parsed = await res.json().catch(() => null);
    if (res.status === 401) notifySessionExpired();
    throw new ApiError(
      res.status,
      parsed?.error?.type ?? "unknown",
      parsed?.error?.message ?? res.statusText,
    );
  }
  return res;
}

/** Sign-in is a top-level navigation, not a fetch: the IdP needs the browser. */
export function startLogin(returnTo: string) {
  window.location.href = `${BASE}/auth/login?return=${encodeURIComponent(returnTo)}`;
}
