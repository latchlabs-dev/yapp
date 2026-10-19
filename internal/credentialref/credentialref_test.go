package credentialref_test

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/credentialref"
)

func TestProviderCredential(t *testing.T) {
	got := credentialref.ProviderCredential("pcr_123")
	if got != "provider-credential:pcr_123" {
		t.Fatalf("ProviderCredential() = %q", got)
	}
}

func TestParseProviderCredential(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		wantID string
		wantOK bool
	}{
		{name: "valid", ref: "provider-credential:pcr_123", wantID: "pcr_123", wantOK: true},
		{name: "empty id", ref: "provider-credential:", wantID: "", wantOK: false},
		{name: "plain secret ref", ref: "providers/openai/api-key", wantID: "providers/openai/api-key", wantOK: false},
		{name: "prefix in middle", ref: "x-provider-credential:pcr_123", wantID: "x-provider-credential:pcr_123", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := credentialref.ParseProviderCredential(tt.ref)
			if gotID != tt.wantID || gotOK != tt.wantOK {
				t.Fatalf("ParseProviderCredential(%q) = (%q, %v), want (%q, %v)", tt.ref, gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}
