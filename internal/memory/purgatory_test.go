package memory

import "testing"

func TestPurgatoryFilePath(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, user, key, want string
	}{
		{"user owns across keys", "TENANT-1", "USER-1", "key-1", "purgatory/tenant-1/user-user-1/observations.md"},
		{"key-only caller", "tenant-1", "", "key-1", "purgatory/tenant-1/key-key-1/observations.md"},
		{"owner namespaces differ", "tenant-1", "key-1", "", "purgatory/tenant-1/user-key-1/observations.md"},
		{"missing tenant", "", "user-1", "key-1", ""},
		{"invalid tenant", "..", "user-1", "key-1", ""},
		{"missing owner", "tenant-1", "", "", ""},
		{"invalid owner", "tenant-1", "..", "key-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PurgatoryFilePath(tc.tenant, tc.user, tc.key)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected invalid scope, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("path = %q, err = %v, want %q", got, err, tc.want)
			}
		})
	}
}
