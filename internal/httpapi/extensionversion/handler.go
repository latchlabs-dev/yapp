// Package extensionversion provides endpoints for the corp-proxy extension
// auto-update mechanism. The extension checks version info at startup and
// downloads the latest source when an update is available.
package extensionversion

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/earlgrey/piproxy/internal/auth"
)

// VersionInfo is returned by GET /proxy/v1/extension-version.
type VersionInfo struct {
	Version      string `json:"version"`
	Changelog    string `json:"changelog"`
	DownloadURL  string `json:"download_url"`
	MinPiVersion string `json:"min_pi_version,omitempty"`
}

// Current version of the corp-proxy extension. Bump this when the extension
// source in extensions/corp-proxy.ts changes.
const CurrentExtensionVersion = "1.3.26"

// embeddedKeyPlaceholder is replaced with the requesting client's own API key
// when serving the extension, so downloaded copies are personalized and work
// without any environment variables. Auto-updates therefore preserve
// credentials: each client re-downloads with its own key and gets its own
// key baked back in.
const embeddedKeyPlaceholder = "__CORP_PROXY_EMBEDDED_KEY__"

// embeddedBaseURLPlaceholder is replaced with this deployment's public base
// URL so non-default deployments serve a correctly-pointed extension. Left
// unsubstituted, the extension falls back to its hardcoded default.
const embeddedBaseURLPlaceholder = "__CORP_PROXY_EMBEDDED_BASE_URL__"

// LoadSource returns the corp-proxy extension source. Lookup order:
// PROXY_EXTENSION_SOURCE_PATH override, the Docker image path, then the
// repo-relative path (dev).
func LoadSource() (string, error) {
	candidates := []string{
		os.Getenv("PROXY_EXTENSION_SOURCE_PATH"),
		"/app/extensions/corp-proxy.ts",
		filepath.Join("extensions", "corp-proxy.ts"),
	}
	var lastErr error = os.ErrNotExist
	for _, c := range candidates {
		if c == "" {
			continue
		}
		data, err := os.ReadFile(c)
		if err == nil {
			return string(data), nil
		}
		lastErr = err
	}
	return "", lastErr
}

// Personalize substitutes the embedded credential and base-URL placeholders.
// Empty arguments leave the corresponding placeholder untouched (the
// extension then falls back to env vars / its built-in default).
func Personalize(source, key, baseURL string) string {
	if key != "" {
		source = strings.ReplaceAll(source, embeddedKeyPlaceholder, key)
	}
	if baseURL != "" {
		source = strings.ReplaceAll(source, embeddedBaseURLPlaceholder, strings.TrimRight(baseURL, "/"))
	}
	return source
}

// NewVersionHandler returns a handler for GET /proxy/v1/extension-version.
// The proxy is the source of truth for the extension version; the extension
// compares its embedded version against this and self-updates when needed.
func NewVersionHandler() http.HandlerFunc {
	downloadURL := "/proxy/v1/extension-download"
	if publicBaseURL := strings.TrimRight(os.Getenv("PROXY_PUBLIC_BASE_URL"), "/"); publicBaseURL != "" {
		downloadURL = publicBaseURL + downloadURL
	}
	info := VersionInfo{
		Version:      CurrentExtensionVersion,
		Changelog:    extensionChangelog,
		DownloadURL:  downloadURL,
		MinPiVersion: "",
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(info)
	}
}

// NewDownloadHandler returns a handler for GET /proxy/v1/extension-download.
// It serves the extension source file embedded in the proxy binary or read
// from disk at the configured path.
//
// The extension can also be served from an env var override for development:
//
//	PROXY_EXTENSION_SOURCE_PATH=/path/to/corp-proxy.ts
func NewDownloadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := LoadSource()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{
				"error": {"type": "extension_not_found", "message": "extension source not available on this server"},
			})
			return
		}

		// Personalize: bake the caller's key (the route sits behind auth, so
		// the extracted key was just validated) and this deployment's public
		// base URL into the served copy. Missing values leave placeholders
		// untouched and the extension falls back to env vars / defaults.
		key := auth.ExtractKey(
			r.Header.Get("Authorization"),
			r.Header.Get("X-Corp-Proxy-Key"),
			r.Header.Get("X-API-Key"),
		)
		source = Personalize(source, key, os.Getenv("PROXY_PUBLIC_BASE_URL"))

		w.Header().Set("Content-Type", "application/typescript")
		w.Header().Set("Content-Disposition", `attachment; filename="corp-proxy.ts"`)
		w.Header().Set("Cache-Control", "no-store, private")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(source))
	}
}

// extensionChangelog is updated alongside CurrentExtensionVersion.
var extensionChangelog = `## 1.3.26 — Official DeepSeek V4.1 Flash
- Uses the official Flash model with image input in fallback discovery.
- Keeps the Qwen token-plan copy separate from the official model.

## 1.3.25 — Session attribution
- Reports the effective Pi thinking level, session name (set via /name),
  first user prompt (title fallback), and run mode (tui/rpc/json/print)
  with every session submission so the admin sessions table shows them.

## 1.3.24 — Durable extension-version attribution
- Sends the extension version on every proxied model request and stores it with
  the accepted usage event so operators can identify stale extension traffic.

## 1.3.23 — Server-observed project activity
- Sends the canonical project ID on proxied model requests so project activity
  comes from accepted server traffic, not only client session timestamps.
- Detects failed session metadata submissions and reports them silently to the
  authenticated extension monitoring endpoint.

## 1.3.22 — Project memory reminder
- Adds a concise final master-prompt line encouraging agents to retrieve useful
  shared memories and record durable project knowledge.

## 1.3.21 — Actionable memory errors
- Explains that HTTP 403 from record_memory means the key lacks the
  memory:observe permission and tells the user how to restore access.

## 1.3.20 — Shared quota percentages
- Shows user and tenant quota usage and remaining allowance as percentages.
- Keeps API-key quota balances dollar-denominated.

## 1.3.19 — Self-service quota and cache trends
- Adds /quota for live daily/monthly used and remaining spend.
- Shows the binding quota scope plus seven-day cache-read and request-hit trends.

## 1.3.18 — Provider-account routing
- Pins each discovered model request to the provider account selected in Pi,
  so overlapping GLM and DeepSeek model IDs use the intended credential.

## 1.3.17 — Project memory tools
- Adds record_memory and retrieve_memories tools for durable project knowledge.
- Adds system-prompt guidance telling agents when to retrieve and record memory.

## 1.3.16 — Shared repository identity
- Resolves HTTPS/SSH origin and upstream remotes to one tenant-scoped project
  ID so multiple users distill into the same repository memory.
- Sends the resolved project ID with session metadata, canonical logs, and
  observations; non-Git directories remain unresolved instead of being
  silently treated as shared repositories.

## 1.3.15 — Session privacy hardening
- Historical Pi session uploads are now opt-in; current-session sync remains
  available only to keys with the explicit session-log scope.
- Sends project-memory context inputs in a POST body so prompts and repository
  remotes do not enter access-log query strings.

## 1.3.14 — Canonical Pi session uploads
- Uploads gzip-compressed native Pi JSONL sessions in the background so tool
  calls/results, branches, and compactions retain their original structure.
- Uses a persistent size/mtime-to-hash cache to avoid reopening unchanged
  files; SHA-256 remains the authoritative server-side identity.

## 1.3.13 — Loader-safe OpenAI-compatible delegate
- Uses Pi's compat-exported openAICompletionsApi factory instead of a package
  subpath import, so the extension works under Pi's extension loader.

## 1.3.12 — DeepSeek stream recursion guard
- Strips provider-local streamSimple hooks before delegating DeepSeek/ZAI
  requests to Pi's OpenAI-compatible transport, avoiding recursive dispatch
  failures such as "Maximum call stack size exceeded".

## 1.3.11 — Codex helper compatibility
- Loads cleanly on Pi installs that do not include the Codex Responses helper.
- Skips proxy-backed Codex registration on those installs while keeping other
  corp-proxy providers available.

## 1.3.10 — Proxy-backed Codex selection
- Keeps Codex model ids visibly corp-prefixed in Pi while replacing the
  built-in Codex provider entry, so proxy-backed Codex cannot be confused with
  local ChatGPT-authenticated Codex.
- Narrows the Codex fetch auth rewrite to the configured proxy base path.

## 1.3.9 — Proxy-backed Codex adapter
- Adds a Codex-specific proxy adapter that keeps Pi authentication pointed at
  corp-proxy while satisfying Pi's local Codex account-id checks.
- Strips the public corp- model prefix before forwarding Codex payloads
  upstream, while keeping the advertised Pi model ids stable.

## 1.3.8 — Proxy review hardening
- Bumps the extension version so existing 1.3.7 clients fetch the latest
  deployed corp-proxy source on their next startup.

## 1.3.7 — Stable DeepSeek stream adapter import
- The extension now imports Pi's streamSimple adapter once at module load
  instead of resolving @earendil-works/pi-ai through eval("require") inside
  each stream call. This avoids a Node/Pi module-resolution spin that could
  peg CPU and freeze session-control sends.

## 1.3.6 — DeepSeek API adapter registration
- The extension now registers a deepseek-completions stream adapter that
  delegates to Pi's OpenAI-compatible transport while preserving the native
  /deepseek/v1 proxy endpoint and DeepSeek compat metadata.

## 1.3.5 — Reliable auto-update downloads
- Version checks now return an absolute extension download URL when
  PROXY_PUBLIC_BASE_URL is configured, so older clients can self-update.
- The extension also resolves relative download URLs defensively.

## 1.3.4 — Native DeepSeek route
- DeepSeek now registers with Pi as api_type=deepseek-completions and uses
  the proxy's /deepseek/v1 endpoint instead of piggybacking on /openai/v1.

## 1.3.3 — Opaque session ids
- Conversation ids now use the stable UUID suffix from Pi's session filename
  instead of the full local session file path. The full path is still submitted
  as session_file metadata, but admin URLs and hook payload ids are now short,
  stable, and URL-safe.

## 1.3.2 — Session correlation via header
- Session id is now sent as an X-Session-ID request header instead of a body
  field. Pi's provider SDKs strip non-standard body keys, so the 1.3.0/1.3.1
  body injection never reached the proxy; the header survives and populates
  conversation_payloads.conversation_id, making the session detail view work.

## 1.3.1 — Distinct model ids (fix proxy bypass)
- Proxy models are now "corp-deepseek-v4-flash"/"corp-deepseek-v4-pro".
  Bare ids collided with Pi's built-in DeepSeek provider (active whenever
  DEEPSEEK_API_KEY is set or a models.json defines deepseek): Pi dedupes by
  id, the built-in won, and traffic silently bypassed the proxy — no
  capture, no session correlation, no billing. The proxy strips the corp-
  prefix before forwarding upstream, so DeepSeek still sees its real id.
  Re-pick the model after updating; it appears as "DeepSeek V4 Flash (corp)".

## 1.3.0 — Session ↔️ payload correlation
- Injects conversation_id into every provider request body so the proxy's
  capture layer stores it in conversation_payloads. This links session
  submissions to captured chat payloads, enabling the sessions page to
  show full conversation details on click.

## 1.2.3 — Full DeepSeek parity
- Models now carry Pi's required cost table (fixes "Cannot read properties
  of undefined (reading 'input')" after each response)
- thinkingLevelMap moved to its correct top-level position: xhigh is now
  selectable, exactly like native DeepSeek (high→high, xhigh→max,
  lower levels unsupported)
- Context window (1M) and max output tokens (384k) now match the native
  DeepSeek catalog

## 1.2.2 — Provider renamed to "corp"
- Pi overrides credentials for providers named "deepseek" with auth.json /
  DEEPSEEK_API_KEY, silently replacing the proxy key and causing 401s.
  Models now register under the "corp" provider; select e.g.
  "deepseek-v4-flash [corp]". Update settings defaultProvider if pinned.

## 1.2.1 — Correct install path
- Install path is ~/.pi/agent/extensions (Pi never scanned ~/.pi/extensions);
  self-update now prefers the agent path
- Onboarding serves the real extension source and a models.json generated
  from the proxy's canonical catalog — no more drifting duplicates
- Deployment base URL is baked in at download time

## 1.2.0 — Personalized credentials + model badges
- Downloaded copies carry your API key embedded (no env vars needed);
  CORP_PROXY_KEY is now only an optional override
- Corp proxy models are marked "(corp)" in the model selector

## 1.1.0 — Project memory
- Duffel-backed project memory: context bundle (curated memory, recent
  lessons, related notes) fetched at session start and appended to the
  system prompt; injection is decided server-side via hooks + admin rules
- Hint refinement: the first user prompt re-fetches the bundle with
  hint-matched related notes (eval: recall 1.0 vs 0.0 hintless)
- record_observation tool so agents can record mistakes/lessons/decisions
- Project detection (CORP_PROXY_PROJECT env, git origin, directory name);
  new projects are auto-scaffolded in duffel with their repo URL
- DeepSeek thinking-level compat passthrough

## 1.0.0 — Initial release
- Dynamic model discovery from proxy
- Session submission to proxy for correlation with usage logs
- Auto-update support`
