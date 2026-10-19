package extensionversion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeSource = `const EXTENSION_VERSION = "x";
const EMBEDDED_KEY = "__CORP_PROXY_EMBEDDED_KEY__";
// rest of extension source padding to pass the 100-byte sanity check ....
`

func newDownloadRequest(t *testing.T, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "corp-proxy.ts")
	if err := os.WriteFile(src, []byte(fakeSource), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_EXTENSION_SOURCE_PATH", src)

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/extension-download", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	NewDownloadHandler()(rec, req)
	return rec
}

func TestDownloadPersonalizesEmbeddedKey(t *testing.T) {
	// Deliberately not cpk_live_-shaped: the repo's pre-commit secret scanner
	// would flag a realistic key literal, and ExtractKey is prefix-agnostic.
	rec := newDownloadRequest(t, "Bearer fake-bearer-token-for-test")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `const EMBEDDED_KEY = "fake-bearer-token-for-test";`) {
		t.Errorf("served source not personalized:\n%s", body)
	}
	if strings.Contains(body, "__CORP_PROXY_EMBEDDED_KEY__") {
		t.Error("placeholder should be fully replaced")
	}
}

func TestDownloadWithoutKeyKeepsPlaceholder(t *testing.T) {
	rec := newDownloadRequest(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "__CORP_PROXY_EMBEDDED_KEY__") {
		t.Error("placeholder should remain when no key is present")
	}
}

func TestVersionHandlerUsesPublicBaseURLForDownload(t *testing.T) {
	t.Setenv("PROXY_PUBLIC_BASE_URL", "https://pproxy.example.com/")

	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/extension-version", nil)
	rec := httptest.NewRecorder()
	NewVersionHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var info VersionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if info.DownloadURL != "https://pproxy.example.com/proxy/v1/extension-download" {
		t.Fatalf("download_url = %q", info.DownloadURL)
	}
}

func TestRepoExtensionContainsPlaceholderAndVersion(t *testing.T) {
	// Guards the version-bump-in-three-places convention and the placeholder
	// the download personalization depends on.
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "extensions", "corp-proxy.ts"))
	if err != nil {
		t.Skipf("extension source not found: %v", err)
	}
	source := string(data)
	if !strings.Contains(source, `const EMBEDDED_KEY = "__CORP_PROXY_EMBEDDED_KEY__";`) {
		t.Error("extension source missing embedded-key placeholder")
	}
	if !strings.Contains(source, `const EXTENSION_VERSION = "`+CurrentExtensionVersion+`";`) {
		t.Errorf("extension EXTENSION_VERSION does not match CurrentExtensionVersion %q", CurrentExtensionVersion)
	}
}
