package scripts

import (
	"os"
	"strings"
	"testing"
)

// Deployment-specific edge assertions live with the private nginx config.
func TestPortalOverwritesForwardedClientHeaders(t *testing.T) {
	body, err := os.ReadFile("../webui/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	config := string(body)
	for _, header := range []string{
		"proxy_set_header X-Real-IP $remote_addr;",
		"proxy_set_header X-Forwarded-For $remote_addr;",
	} {
		if !strings.Contains(config, header) {
			t.Errorf("portal must overwrite forwarding headers: %s", header)
		}
	}
	if strings.Contains(config, "$http_x_real_ip") || strings.Contains(config, "$proxy_add_x_forwarded_for") {
		t.Fatal("portal must not propagate untrusted forwarding headers")
	}
}
