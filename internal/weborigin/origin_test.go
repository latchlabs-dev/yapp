package weborigin

import "testing"

func TestCanonical(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://Console.Example.COM:443/", "https://console.example.com"},
		{"HTTPS://Console.Example.COM:00443", "https://console.example.com"},
		{"http://LOCALHOST:80/", "http://localhost"},
		{"https://Console.Example.COM:3335", "https://console.example.com:3335"},
		{"http://[0:0:0:0:0:0:0:1]:3335/", "http://[::1]:3335"},
		{"https://[::ffff:127.0.0.1]:443", "https://[::ffff:127.0.0.1]"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := Canonical(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCanonicalRejectsNonOrigins(t *testing.T) {
	for _, raw := range []string{"", "null", "//console.test", "https://user:pass@console.test", "https://console.test/path", "https://console.test?", "https://console.test#", "https://console.test:65536", "https://console.test:0", "https://console.test:wrong", "https://bücher.test", "ftp://console.test", "https://console.test\\evil", "https://[fe80::1%25en0]", "https://127.1", "https://0177.0.0.1", "https://0x7f000001", "https://2130706433", "https://console.123", "https://127.0.0.1.", "https://[not-ip]", "https://[127.0.0.1]", "https://::1:443"} {
		if got, err := Canonical(raw); err == nil {
			t.Errorf("accepted %q as %q", raw, got)
		}
	}
}
