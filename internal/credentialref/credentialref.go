// Package credentialref defines typed references for credential backends.
package credentialref

import "strings"

// ProviderCredentialPrefix identifies credentials stored in provider_credentials.
const ProviderCredentialPrefix = "provider-credential:"

// ProviderCredential returns a credential reference for a DB provider credential.
func ProviderCredential(id string) string {
	return ProviderCredentialPrefix + id
}

// ParseProviderCredential extracts a provider credential ID from a DB credential ref.
func ParseProviderCredential(ref string) (string, bool) {
	id := strings.TrimPrefix(ref, ProviderCredentialPrefix)
	return id, id != ref && id != ""
}
