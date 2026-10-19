const statusElement = document.querySelector("#status");
const tokenElement = document.querySelector("#token");
const tabElement = document.querySelector("#tab");
const toggleTabButton = document.querySelector("#toggle-tab");

async function send(type, extra = {}) {
  return chrome.runtime.sendMessage({ type, ...extra });
}

function showStatus(text, kind = "") {
  statusElement.textContent = text;
  statusElement.className = `status ${kind}`.trim();
}

async function refresh() {
  const state = await send("popup.status");
  tokenElement.value = state.hasToken ? "••••••••••••••••" : "";
  showStatus(
    state.connected ? `Paired with Pi on 127.0.0.1:${state.port}` : state.hasToken ? "Pi bridge is not connected." : "Not paired.",
    state.connected ? "ok" : state.hasToken ? "error" : "",
  );
  if (!state.tab) {
    tabElement.textContent = "No controllable active tab.";
    toggleTabButton.disabled = true;
    return;
  }
  tabElement.textContent = `${state.tab.title || "Untitled"}\n${state.tab.url || ""}`;
  toggleTabButton.disabled = false;
  toggleTabButton.textContent = state.tab.approved ? "Detach this tab" : "Approve this tab";
}

document.querySelector("#pair").addEventListener("click", async () => {
  const token = tokenElement.value.trim();
  if (!token || token.includes("•")) {
    showStatus("Paste the token printed by /browser pair.", "error");
    return;
  }
  const result = await send("popup.pair", { token });
  showStatus(result.ok ? "Paired." : result.error || "Pairing failed.", result.ok ? "ok" : "error");
  await refresh();
});

document.querySelector("#forget").addEventListener("click", async () => {
  await send("popup.forget");
  await refresh();
});

toggleTabButton.addEventListener("click", async () => {
  const result = await send("popup.toggleTab");
  if (!result.ok) showStatus(result.error || "Could not change the tab grant.", "error");
  await refresh();
});

await refresh();
