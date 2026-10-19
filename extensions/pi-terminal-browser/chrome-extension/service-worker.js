const DEFAULT_PORT = 17319;
const TOKEN_KEY = "piTerminalBrowserToken";
const PORT_KEY = "piTerminalBrowserPort";
const APPROVED_TABS_KEY = "piTerminalBrowserApprovedTabs";
const DEFAULT_TABS_KEY = "piTerminalBrowserDefaultTabs";
const CDP_VERSION = "1.3";
const MAX_SOCKET_FRAMES = 400;
const MAX_SOCKET_PAYLOAD = 32768;
const BLOCKED_RAW_METHODS = new Set([
  "Browser.close",
  "Network.clearBrowserCache",
  "Network.clearBrowserCookies",
  "Storage.clearDataForOrigin",
  "Target.closeTarget",
  "Target.disposeBrowserContext",
]);

let pairingToken = "";
let bridgePort = DEFAULT_PORT;
let connected = false;
let polling = false;
let approvedTabs = new Set();
let defaultTabs = new Map();
const debugState = new Map();

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function hydrate() {
  const local = await chrome.storage.local.get([TOKEN_KEY, PORT_KEY]);
  pairingToken = typeof local[TOKEN_KEY] === "string" ? local[TOKEN_KEY] : "";
  bridgePort = Number(local[PORT_KEY]) || DEFAULT_PORT;
  const session = await chrome.storage.session.get([APPROVED_TABS_KEY, DEFAULT_TABS_KEY]).catch(() => ({}));
  approvedTabs = new Set(Array.isArray(session[APPROVED_TABS_KEY]) ? session[APPROVED_TABS_KEY] : []);
  defaultTabs = new Map(Object.entries(session[DEFAULT_TABS_KEY] || {}).map(([key, value]) => [key, Number(value)]));
  await refreshBadges();
}

async function persistTabState() {
  await chrome.storage.session.set({
    [APPROVED_TABS_KEY]: Array.from(approvedTabs),
    [DEFAULT_TABS_KEY]: Object.fromEntries(defaultTabs),
  }).catch(() => {});
}

async function refreshBadges() {
  const tabs = await chrome.tabs.query({}).catch(() => []);
  await Promise.all(tabs.map((tab) => {
    if (typeof tab.id !== "number") return Promise.resolve();
    return chrome.action.setBadgeText({ tabId: tab.id, text: approvedTabs.has(tab.id) ? "ON" : "" }).catch(() => {});
  }));
  await chrome.action.setBadgeBackgroundColor({ color: "#16833b" }).catch(() => {});
}

function bridgeURL(path) {
  return `http://127.0.0.1:${bridgePort}${path}`;
}

async function bridgeFetch(path, init = {}) {
  if (!pairingToken) throw new Error("Not paired");
  const headers = new Headers(init.headers || {});
  headers.set("Authorization", `Bearer ${pairingToken}`);
  headers.set("X-Pi-Terminal-Browser-Version", chrome.runtime.getManifest().version);
  return fetch(bridgeURL(path), { ...init, headers, cache: "no-store" });
}

async function checkBridge() {
  try {
    const response = await bridgeFetch("/v1/status");
    connected = response.ok;
    return connected;
  } catch {
    connected = false;
    return false;
  }
}

async function postResult(result) {
  const response = await bridgeFetch("/v1/result", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(result),
  });
  if (!response.ok) throw new Error(`Bridge rejected result (${response.status})`);
}

async function pollLoop() {
  if (polling || !pairingToken) return;
  const tokenAtStart = pairingToken;
  polling = true;
  let retryMs = 250;
  try {
    while (pairingToken) {
      try {
        const response = await bridgeFetch("/v1/poll");
        if (response.status === 401) {
          connected = false;
          break;
        }
        if (!response.ok && response.status !== 204) throw new Error(`Poll failed (${response.status})`);
        connected = true;
        retryMs = 250;
        if (response.status === 204) {
          await sleep(250);
          continue;
        }
        const command = await response.json();
        try {
          const result = await dispatchCommand(command.action, command.params || {});
          await postResult({ id: command.id, ok: true, result });
        } catch (error) {
          await postResult({ id: command.id, ok: false, error: errorMessage(error) });
        }
      } catch {
        connected = false;
        await sleep(retryMs);
        retryMs = Math.min(5000, retryMs * 2);
      }
    }
  } finally {
    polling = false;
    // Pairing can replace a token while an old unauthorized poll is still in
    // flight. Restart immediately with the new credential instead of waiting
    // for the next 30-second MV3 alarm.
    if (pairingToken && pairingToken !== tokenAtStart) void pollLoop();
  }
}

function errorMessage(error) {
  return error instanceof Error ? error.message : String(error);
}

function safeTab(tab) {
  return {
    id: tab.id,
    windowId: tab.windowId,
    active: Boolean(tab.active),
    title: tab.title || "",
    url: tab.url || "",
    status: tab.status || "",
  };
}

async function listApprovedTabs() {
  const tabs = [];
  for (const id of Array.from(approvedTabs)) {
    const tab = await chrome.tabs.get(id).catch(() => null);
    if (!tab) {
      approvedTabs.delete(id);
      continue;
    }
    tabs.push(safeTab(tab));
  }
  await persistTabState();
  return tabs;
}

async function resolveTab(params = {}) {
  const tabs = await listApprovedTabs();
  let matches = tabs;
  if (Number.isInteger(params.tabId)) matches = matches.filter((tab) => tab.id === params.tabId);
  if (typeof params.urlIncludes === "string" && params.urlIncludes) {
    matches = matches.filter((tab) => tab.url.includes(params.urlIncludes));
  }
  if (!params.tabId && !params.urlIncludes && params.sessionKey && defaultTabs.has(params.sessionKey)) {
    matches = matches.filter((tab) => tab.id === defaultTabs.get(params.sessionKey));
  }
  if (matches.length === 0 && tabs.length === 1 && !params.tabId && !params.urlIncludes) matches = tabs;
  if (matches.length === 0) throw new Error("No approved tab matches. Approve it in the Pi Terminal Browser popup, then run /browser attach <url-fragment>.");
  if (matches.length > 1) throw new Error(`Target is ambiguous; approved tab IDs: ${matches.map((tab) => tab.id).join(", ")}`);
  return matches[0];
}

async function approveActiveTab() {
  const [tab] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
  if (!tab || typeof tab.id !== "number" || !/^https?:/i.test(tab.url || "")) {
    return { ok: false, error: "Open a normal http(s) tab before approving it." };
  }
  if (approvedTabs.has(tab.id)) {
    approvedTabs.delete(tab.id);
    for (const [key, value] of defaultTabs) if (value === tab.id) defaultTabs.delete(key);
    await detachDebugger(tab.id);
  } else {
    approvedTabs.add(tab.id);
  }
  await persistTabState();
  await refreshBadges();
  return { ok: true, approved: approvedTabs.has(tab.id) };
}

function stateFor(tabId) {
  let state = debugState.get(tabId);
  if (!state) {
    state = { contexts: new Map(), socketFrames: [], patches: new Map(), attached: false };
    debugState.set(tabId, state);
  }
  return state;
}

function debuggee(tabId, sessionId) {
  return sessionId ? { tabId, sessionId } : { tabId };
}

async function sendCDP(tabId, method, params = {}, sessionId) {
  return chrome.debugger.sendCommand(debuggee(tabId, sessionId), method, params);
}

async function enableTarget(tabId, sessionId) {
  const commands = [
    ["Runtime.enable", {}],
    ["Page.enable", {}],
    ["Network.enable", { maxTotalBufferSize: 1048576, maxResourceBufferSize: 262144 }],
    ["Target.setAutoAttach", { autoAttach: true, waitForDebuggerOnStart: false, flatten: true }],
  ];
  for (const [method, params] of commands) {
    await sendCDP(tabId, method, params, sessionId).catch(() => {});
  }
}

async function ensureDebugger(tabId) {
  if (!approvedTabs.has(tabId)) throw new Error(`Tab ${tabId} is not approved.`);
  const state = stateFor(tabId);
  if (state.attached) return state;
  try {
    await chrome.debugger.attach({ tabId }, CDP_VERSION);
  } catch (error) {
    if (!/already attached/i.test(errorMessage(error))) throw error;
  }
  state.attached = true;
  await enableTarget(tabId);
  await sleep(75);
  return state;
}

async function detachDebugger(tabId) {
  const state = debugState.get(tabId);
  debugState.delete(tabId);
  if (!state?.attached) return;
  await chrome.debugger.detach({ tabId }).catch(() => {});
}

async function detachAll() {
  await Promise.all(Array.from(debugState.keys()).map(detachDebugger));
}

chrome.debugger.onDetach.addListener(({ tabId }) => {
  if (typeof tabId === "number") debugState.delete(tabId);
});

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (typeof source.tabId !== "number") return;
  const state = stateFor(source.tabId);
  if (method === "Runtime.executionContextCreated") {
    const context = params.context;
    const frameId = context?.auxData?.frameId;
    if (frameId && context?.auxData?.isDefault) {
      state.contexts.set(frameId, { contextId: context.id, sessionId: source.sessionId || "", origin: context.origin || "" });
    }
  } else if (method === "Runtime.executionContextsCleared") {
    for (const [frameId, context] of state.contexts) {
      if ((context.sessionId || "") === (source.sessionId || "")) state.contexts.delete(frameId);
    }
  } else if (method === "Target.attachedToTarget") {
    void enableTarget(source.tabId, params.sessionId);
  } else if (method === "Network.webSocketCreated") {
    state.socketFrames.push({ direction: "open", requestId: params.requestId, url: params.url, at: Date.now() });
  } else if (method === "Network.webSocketFrameReceived" || method === "Network.webSocketFrameSent") {
    state.socketFrames.push({
      direction: method.endsWith("Received") ? "received" : "sent",
      requestId: params.requestId,
      opcode: params.response?.opcode,
      payloadData: String(params.response?.payloadData || "").slice(0, MAX_SOCKET_PAYLOAD),
      at: Date.now(),
    });
  }
  if (state.socketFrames.length > MAX_SOCKET_FRAMES) state.socketFrames.splice(0, state.socketFrames.length - MAX_SOCKET_FRAMES);
});

function flattenFrameTree(tree, result = []) {
  if (!tree?.frame) return result;
  result.push({ id: tree.frame.id, parentId: tree.frame.parentId || null, url: tree.frame.url || "", name: tree.frame.name || "" });
  for (const child of tree.childFrames || []) flattenFrameTree(child, result);
  return result;
}

async function framesFor(tabId) {
  const tree = await sendCDP(tabId, "Page.getFrameTree");
  return flattenFrameTree(tree.frameTree);
}

async function executionTarget(tabId, frameId) {
  if (!frameId) return { sessionId: undefined, contextId: undefined };
  const state = stateFor(tabId);
  let context = state.contexts.get(frameId);
  if (!context) {
    await enableTarget(tabId);
    await sleep(100);
    context = state.contexts.get(frameId);
  }
  if (!context) throw new Error(`No main-world execution context is available for frame ${frameId}. Run terminal detect again after the frame finishes loading.`);
  return { sessionId: context.sessionId || undefined, contextId: context.contextId };
}

async function evaluate(tabId, expression, options = {}) {
  await ensureDebugger(tabId);
  const target = await executionTarget(tabId, options.frameId);
  const response = await sendCDP(tabId, "Runtime.evaluate", {
    expression,
    awaitPromise: options.awaitPromise !== false,
    returnByValue: options.returnByValue !== false,
    userGesture: options.userGesture === true,
    ...(target.contextId ? { contextId: target.contextId } : {}),
  }, target.sessionId);
  if (response.exceptionDetails) {
    const detail = response.exceptionDetails.exception?.description || response.exceptionDetails.text || "Evaluation failed";
    throw new Error(detail);
  }
  if (options.returnByValue === false) {
    return { type: response.result?.type, subtype: response.result?.subtype, description: response.result?.description };
  }
  return response.result?.value;
}

const TERMINAL_DETECT_EXPRESSION = `(() => {
  const terminals = Array.from(document.querySelectorAll('.xterm, [data-terminal], [role="application"]')).slice(0, 20);
  return {
    title: document.title,
    url: location.href,
    activeElement: document.activeElement ? {
      tag: document.activeElement.tagName,
      id: document.activeElement.id || '',
      classes: typeof document.activeElement.className === 'string' ? document.activeElement.className : ''
    } : null,
    terminals: terminals.map((element, index) => ({
      index,
      selector: element.id ? '#' + CSS.escape(element.id) : element.classList.contains('xterm') ? '.xterm' : '[data-terminal]',
      classes: typeof element.className === 'string' ? element.className : '',
      helperTextarea: Boolean(element.querySelector('.xterm-helper-textarea, textarea')),
      canvases: element.querySelectorAll('canvas').length,
      rows: Boolean(element.querySelector('.xterm-rows')),
      accessibility: Boolean(element.querySelector('.xterm-accessibility-tree, .xterm-accessibility')),
      rect: (() => { const r = element.getBoundingClientRect(); return { x: r.x, y: r.y, width: r.width, height: r.height }; })()
    }))
  };
})()`;

const TERMINAL_READ_EXPRESSION = `(() => {
  const root = document.querySelector('.xterm, [data-terminal], [role="application"]');
  const isTerminal = value => value && typeof value === 'object' && value.buffer && value.buffer.active && typeof value.buffer.active.getLine === 'function';
  const known = [window.term, window.terminal, window.xterm, window.app && window.app.terminal, root && root._xterm, root && root.terminal];
  const seen = new Set();
  const queue = [...known, root, root && root.parentElement].filter(Boolean);
  let terminal = null;
  let budget = 250;
  while (queue.length && budget-- > 0 && !terminal) {
    const value = queue.shift();
    if (!value || (typeof value !== 'object' && typeof value !== 'function') || seen.has(value)) continue;
    seen.add(value);
    if (isTerminal(value)) { terminal = value; break; }
    let names = [];
    try { names = Object.getOwnPropertyNames(value).slice(0, 80); } catch {}
    for (const name of names) {
      if (!/term|xterm|core|instance|ref|current/i.test(name)) continue;
      try { const child = value[name]; if (child && !seen.has(child)) queue.push(child); } catch {}
    }
  }
  if (terminal) {
    const buffer = terminal.buffer.active;
    const lines = [];
    const length = Math.min(Number(buffer.length || terminal.rows || 0), 10000);
    for (let i = 0; i < length; i++) {
      const line = buffer.getLine(i);
      if (line) lines.push(line.translateToString(true));
    }
    while (lines.length && !lines[lines.length - 1]) lines.pop();
    return { source: 'xterm-buffer', text: lines.join('\\n'), cols: terminal.cols, rows: terminal.rows };
  }
  const accessibility = root && root.querySelector('.xterm-accessibility-tree, .xterm-accessibility');
  if (accessibility && accessibility.innerText.trim()) return { source: 'xterm-accessibility', text: accessibility.innerText };
  const rows = root && root.querySelector('.xterm-rows');
  if (rows && rows.innerText.trim()) return { source: 'xterm-dom', text: rows.innerText };
  return { source: 'none', text: '' };
})()`;

function snapshotExpression(maxElements = 80) {
  return `(() => {
    const visible = element => { const r = element.getBoundingClientRect(); const s = getComputedStyle(element); return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'; };
    const elements = Array.from(document.querySelectorAll('a,button,input,textarea,select,[role],[contenteditable="true"],.xterm')).filter(visible).slice(0, ${Math.min(200, Math.max(1, maxElements))});
    return {
      title: document.title,
      url: location.href,
      text: (document.body && document.body.innerText || '').slice(0, 20000),
      activeElement: document.activeElement ? { tag: document.activeElement.tagName, id: document.activeElement.id || '', classes: String(document.activeElement.className || '') } : null,
      elements: elements.map((element, index) => {
        const r = element.getBoundingClientRect();
        return { index, tag: element.tagName, role: element.getAttribute('role') || '', label: element.getAttribute('aria-label') || element.innerText || element.value || '', id: element.id || '', classes: String(element.className || ''), rect: { x: r.x, y: r.y, width: r.width, height: r.height } };
      })
    };
  })()`;
}

async function detectTerminals(tabId) {
  await ensureDebugger(tabId);
  const frames = await framesFor(tabId);
  const found = [];
  for (const frame of frames) {
    try {
      const value = await evaluate(tabId, TERMINAL_DETECT_EXPRESSION, { frameId: frame.parentId ? frame.id : undefined });
      if (value?.terminals?.length) found.push({ frame, ...value });
    } catch (error) {
      found.push({ frame, error: errorMessage(error), terminals: [] });
    }
  }
  return { frames, matches: found };
}

function terminalFocusExpression(selector) {
  const encoded = JSON.stringify(selector || ".xterm");
  return `(() => {
    const root = document.querySelector(${encoded});
    if (!root) throw new Error('Terminal element not found: ' + ${encoded});
    const target = root.matches('textarea,input,[contenteditable="true"]') ? root : root.querySelector('.xterm-helper-textarea, textarea, input, [contenteditable="true"]') || root;
    target.focus({ preventScroll: false });
    if (typeof target.click === 'function') target.click();
    return { tag: target.tagName, id: target.id || '', classes: String(target.className || ''), focused: document.activeElement === target };
  })()`;
}

async function focusTerminal(tabId, params) {
  return evaluate(tabId, terminalFocusExpression(params.selector), { frameId: params.frameId, userGesture: true });
}

function decodeSocketFrame(frame) {
  let text = String(frame.payloadData || "");
  if (frame.opcode === 2) {
    try {
      const bytes = Uint8Array.from(atob(text), (char) => char.charCodeAt(0));
      text = new TextDecoder().decode(bytes);
    } catch {}
  }
  const printable = text.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, "");
  return { direction: frame.direction, requestId: frame.requestId, at: frame.at, text: printable };
}

async function readTerminal(tabId, params) {
  const structured = await evaluate(tabId, TERMINAL_READ_EXPRESSION, { frameId: params.frameId });
  let result = structured;
  if (!result?.text && (params.source === "auto" || params.source === "websocket" || !params.source)) {
    const frames = stateFor(tabId).socketFrames
      .filter((frame) => frame.direction === "received")
      .slice(-(Number(params.maxFrames) || 80))
      .map(decodeSocketFrame);
    result = { source: "websocket", text: frames.map((frame) => frame.text).join(""), frames };
  }
  const maxChars = Math.min(100000, Math.max(100, Number(params.maxChars) || 30000));
  let text = String(result?.text || "");
  if (params.delta) {
    const key = `${tabId}:${params.frameId || "top"}`;
    const previous = stateFor(tabId).lastRead?.get?.(key) || "";
    if (!stateFor(tabId).lastRead) stateFor(tabId).lastRead = new Map();
    stateFor(tabId).lastRead.set(key, text);
    if (text.startsWith(previous)) text = text.slice(previous.length);
  }
  if (text.length > maxChars) text = text.slice(-maxChars);
  return { ...result, text };
}

const KEY_DEFINITIONS = {
  Enter: ["Enter", "Enter", 13], Tab: ["Tab", "Tab", 9], Escape: ["Escape", "Escape", 27],
  Backspace: ["Backspace", "Backspace", 8], Delete: ["Delete", "Delete", 46],
  ArrowUp: ["ArrowUp", "ArrowUp", 38], ArrowDown: ["ArrowDown", "ArrowDown", 40],
  ArrowLeft: ["ArrowLeft", "ArrowLeft", 37], ArrowRight: ["ArrowRight", "ArrowRight", 39],
  Home: ["Home", "Home", 36], End: ["End", "End", 35], PageUp: ["PageUp", "PageUp", 33],
  PageDown: ["PageDown", "PageDown", 34], Insert: ["Insert", "Insert", 45], Space: [" ", "Space", 32],
};

function normalizedKey(value) {
  const aliases = { esc: "Escape", return: "Enter", up: "ArrowUp", down: "ArrowDown", left: "ArrowLeft", right: "ArrowRight", del: "Delete", space: "Space" };
  const raw = String(value || "");
  return aliases[raw.toLowerCase()] || raw;
}

async function pressKey(tabId, keyValue, modifiers = []) {
  await ensureDebugger(tabId);
  const key = normalizedKey(keyValue);
  const modifierSet = new Set(modifiers.map((value) => String(value).toLowerCase()));
  const modifierBits = (modifierSet.has("alt") ? 1 : 0) | (modifierSet.has("ctrl") || modifierSet.has("control") ? 2 : 0) | (modifierSet.has("meta") || modifierSet.has("command") ? 4 : 0) | (modifierSet.has("shift") ? 8 : 0);
  const modifierDefs = [["Alt", "AltLeft", 18, 1], ["Control", "ControlLeft", 17, 2], ["Meta", "MetaLeft", 91, 4], ["Shift", "ShiftLeft", 16, 8]];
  const activeModifiers = modifierDefs.filter(([, , , bit]) => (modifierBits & bit) !== 0);
  for (const [modKey, code, vk, bit] of activeModifiers) {
    await sendCDP(tabId, "Input.dispatchKeyEvent", { type: "rawKeyDown", key: modKey, code, windowsVirtualKeyCode: vk, modifiers: bit });
  }
  const definition = KEY_DEFINITIONS[key] || [key, key.length === 1 ? `Key${key.toUpperCase()}` : key, key.length === 1 ? key.toUpperCase().charCodeAt(0) : 0];
  const [cdpKey, code, vk] = definition;
  const text = key.length === 1 && modifierBits === 0 ? key : undefined;
  await sendCDP(tabId, "Input.dispatchKeyEvent", { type: "rawKeyDown", key: cdpKey, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers: modifierBits, ...(text ? { text, unmodifiedText: text } : {}) });
  await sendCDP(tabId, "Input.dispatchKeyEvent", { type: "keyUp", key: cdpKey, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers: modifierBits });
  for (const [modKey, code, vk] of activeModifiers.reverse()) {
    await sendCDP(tabId, "Input.dispatchKeyEvent", { type: "keyUp", key: modKey, code, windowsVirtualKeyCode: vk, modifiers: 0 });
  }
  return { key, modifiers: Array.from(modifierSet) };
}

async function clickSelector(tabId, params) {
  const selector = JSON.stringify(params.selector);
  const rect = await evaluate(tabId, `(() => { const e = document.querySelector(${selector}); if (!e) throw new Error('Element not found'); e.scrollIntoView({block:'center',inline:'center'}); const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`, { frameId: params.frameId });
  if (params.frameId) {
    const result = await evaluate(tabId, `(() => { const e = document.querySelector(${selector}); e.focus(); e.click(); return true; })()`, { frameId: params.frameId, userGesture: true });
    return { clicked: result, trusted: false, note: "Frame-local DOM click used; keyboard input remains CDP-backed." };
  }
  await sendCDP(tabId, "Input.dispatchMouseEvent", { type: "mousePressed", x: rect.x, y: rect.y, button: "left", clickCount: 1 });
  await sendCDP(tabId, "Input.dispatchMouseEvent", { type: "mouseReleased", x: rect.x, y: rect.y, button: "left", clickCount: 1 });
  return { clicked: true, trusted: true, point: rect };
}

async function applyPatch(tabId, params) {
  const patchId = params.patchId || crypto.randomUUID();
  const styleId = `pi-terminal-browser-patch-${patchId}`;
  const source = `(() => {
    const apply = () => {
      let style = document.getElementById(${JSON.stringify(styleId)});
      if (!style) { style = document.createElement('style'); style.id = ${JSON.stringify(styleId)}; (document.head || document.documentElement).appendChild(style); }
      style.textContent = ${JSON.stringify(params.css || "")};
      ${params.javascript || ""}
    };
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', apply, {once:true}); else apply();
  })()`;
  let scriptIdentifier = null;
  if (params.persist !== false && !params.frameId) {
    const added = await sendCDP(tabId, "Page.addScriptToEvaluateOnNewDocument", { source });
    scriptIdentifier = added.identifier;
  }
  await evaluate(tabId, source, { frameId: params.frameId, userGesture: true });
  stateFor(tabId).patches.set(patchId, { styleId, scriptIdentifier, frameId: params.frameId });
  return { patchId, persistent: Boolean(scriptIdentifier) };
}

async function revertPatch(tabId, patchId) {
  const patch = stateFor(tabId).patches.get(patchId);
  if (!patch) throw new Error(`Unknown patch: ${patchId}`);
  if (patch.scriptIdentifier) await sendCDP(tabId, "Page.removeScriptToEvaluateOnNewDocument", { identifier: patch.scriptIdentifier }).catch(() => {});
  await evaluate(tabId, `document.getElementById(${JSON.stringify(patch.styleId)})?.remove()`, { frameId: patch.frameId }).catch(() => {});
  stateFor(tabId).patches.delete(patchId);
  return { reverted: patchId };
}

async function screenshot(tabId, params) {
  await ensureDebugger(tabId);
  let clip;
  if (params.selector && !params.frameId) {
    clip = await evaluate(tabId, `(() => { const e=document.querySelector(${JSON.stringify(params.selector)}); if(!e) throw new Error('Element not found'); const r=e.getBoundingClientRect(); return {x:r.x+scrollX,y:r.y+scrollY,width:r.width,height:r.height,scale:1}; })()`);
  }
  const capture = await sendCDP(tabId, "Page.captureScreenshot", { format: "png", fromSurface: true, captureBeyondViewport: Boolean(params.fullPage), ...(clip ? { clip } : {}) });
  return { image: { mimeType: "image/png", data: capture.data }, tabId };
}

async function waitForPage(tabId, params) {
  const deadline = Date.now() + Math.min(60000, Math.max(100, Number(params.timeoutMs) || 10000));
  while (Date.now() < deadline) {
    let matched = false;
    if (params.selector) matched = Boolean(await evaluate(tabId, `Boolean(document.querySelector(${JSON.stringify(params.selector)}))`, { frameId: params.frameId }));
    else if (params.text) matched = Boolean(await evaluate(tabId, `(document.body?.innerText || '').includes(${JSON.stringify(params.text)})`, { frameId: params.frameId }));
    else if (params.expression) matched = Boolean(await evaluate(tabId, params.expression, { frameId: params.frameId }));
    if (matched) return { matched: true };
    await sleep(100);
  }
  throw new Error(`Page wait timed out after ${Number(params.timeoutMs) || 10000}ms`);
}

async function waitForTerminal(tabId, params) {
  const timeoutMs = Math.min(60000, Math.max(100, Number(params.timeoutMs) || 10000));
  const deadline = Date.now() + timeoutMs;
  const matcher = params.regex ? new RegExp(params.pattern, params.caseSensitive ? "" : "i") : null;
  while (Date.now() < deadline) {
    const value = await readTerminal(tabId, { ...params, delta: false, maxChars: 100000 });
    const text = String(value.text || "");
    const matched = matcher ? matcher.test(text) : params.caseSensitive ? text.includes(params.pattern) : text.toLowerCase().includes(String(params.pattern).toLowerCase());
    if (matched) return { matched: true, source: value.source, text: text.slice(-30000) };
    await sleep(150);
  }
  throw new Error(`Terminal wait timed out after ${timeoutMs}ms`);
}

async function dispatchCommand(action, params) {
  if (action === "tabs.list") return { tabs: await listApprovedTabs(), defaults: Object.fromEntries(defaultTabs) };
  if (action === "tabs.select") {
    const tab = await resolveTab(params);
    if (!params.sessionKey) throw new Error("Session key is required");
    for (const [owner, ownedTabId] of defaultTabs) {
      if (owner !== params.sessionKey && ownedTabId === tab.id) {
        throw new Error(`Tab ${tab.id} is already owned by another Pi session.`);
      }
    }
    defaultTabs.set(params.sessionKey, tab.id);
    await persistTabState();
    return { tab };
  }
  if (action === "bridge.detachAll") {
    await detachAll();
    approvedTabs.clear();
    defaultTabs.clear();
    await persistTabState();
    await refreshBadges();
    return { detached: true };
  }
  if (action === "bridge.detachSession") {
    const tabId = params.sessionKey ? defaultTabs.get(params.sessionKey) : undefined;
    if (params.sessionKey) defaultTabs.delete(params.sessionKey);
    if (typeof tabId === "number") await detachDebugger(tabId);
    await persistTabState();
    return { detached: typeof tabId === "number", tabId: tabId || null };
  }

  const tab = await resolveTab(params);
  const tabId = tab.id;
  if (action === "page.inspect") return { tab, frames: await framesForAfterAttach(tabId), snapshot: await evaluate(tabId, snapshotExpression(params.maxElements), { frameId: params.frameId }) };
  if (action === "page.evaluate") return { tab, value: await evaluate(tabId, params.expression, { frameId: params.frameId, awaitPromise: params.awaitPromise, returnByValue: params.returnByValue, userGesture: params.userGesture }) };
  if (action === "page.patch") return { tab, ...(await applyPatch(tabId, params)) };
  if (action === "page.revertPatch") return { tab, ...(await revertPatch(tabId, params.patchId)) };
  if (action === "page.screenshot") return { tab, ...(await screenshot(tabId, params)) };
  if (action === "page.waitFor") return { tab, ...(await waitForPage(tabId, params)) };
  if (action === "input.click") return { tab, ...(await clickSelector(tabId, params)) };
  if (action === "input.insertText") {
    await ensureDebugger(tabId);
    await sendCDP(tabId, "Input.insertText", { text: String(params.text || "") });
    return { tab, insertedChars: String(params.text || "").length };
  }
  if (action === "input.press") return { tab, ...(await pressKey(tabId, params.key, params.modifiers || [])) };
  if (action === "input.scroll") {
    await ensureDebugger(tabId);
    await sendCDP(tabId, "Input.dispatchMouseEvent", { type: "mouseWheel", x: Number(params.x) || 100, y: Number(params.y) || 100, deltaX: Number(params.deltaX) || 0, deltaY: Number(params.deltaY) || 0 });
    return { tab, scrolled: true };
  }
  if (action === "terminal.detect") return { tab, ...(await detectTerminals(tabId)) };
  if (action === "terminal.focus") return { tab, focus: await focusTerminal(tabId, params) };
  if (action === "terminal.write") {
    const focus = await focusTerminal(tabId, params);
    if (params.mode === "keys") {
      for (const character of String(params.text || "")) await pressKey(tabId, character, []);
    } else {
      await sendCDP(tabId, "Input.insertText", { text: String(params.text || "") });
    }
    if (params.submit) await pressKey(tabId, "Enter", []);
    return { tab, focus, insertedChars: String(params.text || "").length, submitted: Boolean(params.submit), mode: params.mode || "fast" };
  }
  if (action === "terminal.press") {
    await focusTerminal(tabId, params);
    return { tab, ...(await pressKey(tabId, params.key, params.modifiers || [])) };
  }
  if (action === "terminal.read") return { tab, ...(await readTerminal(tabId, params)) };
  if (action === "terminal.waitFor") return { tab, ...(await waitForTerminal(tabId, params)) };
  if (action === "terminal.interrupt") {
    await focusTerminal(tabId, params);
    return { tab, ...(await pressKey(tabId, "c", ["Ctrl"])) };
  }
  if (action === "terminal.resize") {
    const value = await evaluate(tabId, `(() => { const candidates=[window.term,window.terminal,window.xterm]; const t=candidates.find(v=>v&&typeof v.resize==='function'); if(!t) throw new Error('No exposed xterm instance with resize()'); t.resize(${Number(params.cols)},${Number(params.rows)}); return {cols:t.cols,rows:t.rows}; })()`, { frameId: params.frameId });
    return { tab, value };
  }
  if (action === "raw.cdp") {
    if (BLOCKED_RAW_METHODS.has(params.method)) throw new Error(`Blocked destructive CDP method: ${params.method}`);
    await ensureDebugger(tabId);
    return { tab, value: await sendCDP(tabId, params.method, params.params || {}, params.sessionId) };
  }
  throw new Error(`Unknown bridge action: ${action}`);
}

async function framesForAfterAttach(tabId) {
  await ensureDebugger(tabId);
  return framesFor(tabId);
}

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  (async () => {
    if (message?.type === "popup.status") {
      const [tab] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
      return {
        connected,
        hasToken: Boolean(pairingToken),
        port: bridgePort,
        tab: tab && typeof tab.id === "number" && /^https?:/i.test(tab.url || "") ? { ...safeTab(tab), approved: approvedTabs.has(tab.id) } : null,
      };
    }
    if (message?.type === "popup.pair") {
      const token = String(message.token || "").trim();
      if (token.length < 32) return { ok: false, error: "Pairing token is too short." };
      pairingToken = token;
      await chrome.storage.local.set({ [TOKEN_KEY]: token, [PORT_KEY]: bridgePort });
      const ok = await checkBridge();
      if (ok) void pollLoop();
      return { ok, error: ok ? undefined : "The Pi bridge did not accept this token." };
    }
    if (message?.type === "popup.forget") {
      pairingToken = "";
      connected = false;
      await chrome.storage.local.remove(TOKEN_KEY);
      await detachAll();
      approvedTabs.clear();
      defaultTabs.clear();
      await persistTabState();
      await refreshBadges();
      return { ok: true };
    }
    if (message?.type === "popup.toggleTab") return approveActiveTab();
    return { ok: false, error: "Unknown popup action." };
  })().then(sendResponse, (error) => sendResponse({ ok: false, error: errorMessage(error) }));
  return true;
});

chrome.tabs.onRemoved.addListener((tabId) => {
  approvedTabs.delete(tabId);
  for (const [key, value] of defaultTabs) if (value === tabId) defaultTabs.delete(key);
  void detachDebugger(tabId);
  void persistTabState();
});

chrome.alarms?.onAlarm?.addListener(() => {
  if (pairingToken) void pollLoop();
});

await hydrate();
if (chrome.alarms) await chrome.alarms.create("pi-terminal-browser-poll", { periodInMinutes: 0.5 }).catch(() => {});
if (pairingToken) void pollLoop();
