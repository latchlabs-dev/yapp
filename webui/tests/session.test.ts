import assert from "node:assert/strict";
import { after, before, test, type TestContext } from "node:test";
import { act, createElement } from "react";
import type { Root } from "react-dom/client";
import { JSDOM } from "jsdom";
import { api, setCsrfToken } from "../src/lib/api.ts";

let createRoot: typeof import("react-dom/client").createRoot;
let SessionProvider: typeof import("../src/hooks/use-session.tsx").SessionProvider;
let useSession: typeof import("../src/hooks/use-session.tsx").useSession;
let AppShell: typeof import("../src/components/layout/app-shell.tsx").AppShell;
let MemoryRouter: typeof import("react-router-dom").MemoryRouter;
let dom: JSDOM;
let session: ReturnType<typeof useSession>;

before(async () => {
  dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "https://console.test" });
  for (const name of ["window", "document", "navigator", "HTMLElement", "Event", "MouseEvent"]) {
    Object.defineProperty(globalThis, name, { value: (dom.window as unknown as Record<string, unknown>)[name], configurable: true });
  }
  Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
  ({ createRoot } = await import("react-dom/client"));
  ({ SessionProvider, useSession } = await import("../src/hooks/use-session.tsx"));
  ({ AppShell } = await import("../src/components/layout/app-shell.tsx"));
  ({ MemoryRouter } = await import("react-router-dom"));
});
after(() => dom.window.close());

const me = (csrf = "first") => ({
  user: { id: "u", email: "test@example.test", display_name: "Test User" },
  tenant: { id: "t", name: "Test Tenant" }, scopes: ["llm:invoke"], csrf_token: csrf,
  expires_at: "2099-01-01T00:00:00Z", attachments: { enabled: false, max_size_bytes: 0 },
});
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const denied = (status: number) => json({ error: { type: "denied", message: "request denied" } }, status);

async function mount(t: TestContext, fetcher: typeof fetch) {
  const original = globalThis.fetch;
  globalThis.fetch = fetcher;
  setCsrfToken("");
  const container = document.createElement("div");
  document.body.append(container);
  let root: Root;
  function Probe() {
    session = useSession();
    return createElement("output", null, session.me ? "authenticated" : "signed out");
  }
  await act(async () => {
    root = createRoot(container);
    root.render(createElement(SessionProvider, null, createElement(MemoryRouter, null,
      createElement(Probe), createElement(AppShell))));
  });
  t.after(async () => {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = original;
    setCsrfToken("");
  });
  assert.equal(session.me?.user.id, "u");
  return container;
}

async function clickSignOut(container: HTMLElement) {
  const button = container.querySelector<HTMLButtonElement>('button[aria-label="Sign out"]')!;
  await act(async () => { button.dispatchEvent(new MouseEvent("click", { bubbles: true })); });
  return button;
}

test("logout network failure retains identity and CSRF, shows an error, and permits retry", async (t) => {
  let fail = true;
  let writeToken = "";
  const container = await mount(t, async (url, options) => {
    if (url === "/web/v1/me") return json(me());
    if (url === "/web/v1/auth/logout") {
      if (fail) throw new TypeError("network unavailable");
      return json({ status: "signed_out" });
    }
    writeToken = new Headers(options?.headers).get("X-Web-CSRF") ?? "";
    return json({ status: "saved" });
  });
  const button = await clickSignOut(container);
  assert.equal(session.me?.user.id, "u");
  assert.match(container.querySelector('[role="alert"]')!.textContent!, /could not be confirmed/);
  assert.equal(button.disabled, false);
  await api.saveProjectMemory("project", "unchanged identity");
  assert.equal(writeToken, "first");
  fail = false;
  await clickSignOut(container);
  assert.equal(session.me, null);
  assert.equal(session.signOutError, "");
});

test("reauthentication in another tab refreshes CSRF then retries logout once", async (t) => {
  let cookieCsrf = "first";
  let reads = 0;
  const attempts: string[] = [];
  const container = await mount(t, async (url, options) => {
    if (url === "/web/v1/me") { reads++; return json(me(cookieCsrf)); }
    const token = new Headers(options?.headers).get("X-Web-CSRF") ?? "";
    attempts.push(token);
    return token === cookieCsrf ? json({ status: "signed_out" }) : denied(403);
  });
  cookieCsrf = "other-tab";
  await clickSignOut(container);
  assert.deepEqual(attempts, ["first", "other-tab"]);
  assert.equal(reads, 2);
  assert.equal(session.me, null);
  assert.equal(session.signOutError, "");
});

test("repeated CSRF failure is bounded and retains the freshly resolved identity", async (t) => {
  let reads = 0;
  const attempts: string[] = [];
  const container = await mount(t, async (url, options) => {
    if (url === "/web/v1/me") return json(me(++reads === 1 ? "first" : "other-tab"));
    attempts.push(new Headers(options?.headers).get("X-Web-CSRF") ?? "");
    return denied(403);
  });
  await clickSignOut(container);
  assert.deepEqual(attempts, ["first", "other-tab"]);
  assert.equal(session.me?.csrf_token, "other-tab");
  assert.ok(container.querySelector('[role="alert"]'));
});

for (const failure of ["network", "401", "503"]) {
  test(`failed stale-CSRF refresh (${failure}) does not claim sign-out`, async (t) => {
    let reads = 0;
    let attempts = 0;
    const container = await mount(t, async (url) => {
      if (url === "/web/v1/me") {
        if (++reads === 1) return json(me());
        if (failure === "network") throw new TypeError("network unavailable");
        return denied(Number(failure));
      }
      attempts++;
      return denied(403);
    });
    await clickSignOut(container);
    assert.equal(attempts, 1);
    assert.equal(session.me?.csrf_token, "first");
    assert.ok(container.querySelector('[role="alert"]'));
  });
}

for (const status of [401, 503]) {
  test(`logout HTTP ${status} does not clear frontend authentication`, async (t) => {
    const container = await mount(t, async (url) => url === "/web/v1/me" ? json(me()) : denied(status));
    await clickSignOut(container);
    assert.equal(session.me?.user.id, "u");
    assert.ok(container.querySelector('[role="alert"]'));
  });
}

test("a late session refresh cannot resurrect a confirmed logout", async (t) => {
  let reads = 0;
  let resolve!: (value: Response) => void;
  await mount(t, async (url) => {
    if (url !== "/web/v1/me") return json({ status: "signed_out" });
    if (++reads === 1) return json(me());
    return new Promise<Response>((done) => { resolve = done; });
  });
  let refresh!: Promise<boolean>;
  await act(async () => { refresh = session.refresh(); });
  await act(async () => { assert.equal(await session.signOut(), true); });
  await act(async () => { resolve(json(me())); assert.equal(await refresh, false); });
  assert.equal(session.me, null);
});
