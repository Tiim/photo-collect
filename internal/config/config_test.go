package config

import (
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	base := map[string]string{
		"BASE_URL": "https://p.example", "OIDC_ISSUER_URL": "https://i", "OIDC_CLIENT_ID": "x",
		"OIDC_CLIENT_SECRET": "y", "SESSION_SECRET": strings.Repeat("s", 32),
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestTrustedProxiesAndVerifiedEmail(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 0 || !c.OIDCRequireVerifiedEmail {
		t.Errorf("defaults wrong: %v %v", c.TrustedProxies, c.OIDCRequireVerifiedEmail)
	}
	setEnv(t, map[string]string{"TRUSTED_PROXIES": "private, 203.0.113.0/24", "OIDC_REQUIRE_VERIFIED_EMAIL": "false"})
	if c, err = Load(); err != nil || len(c.TrustedProxies) != 7 || c.OIDCRequireVerifiedEmail {
		t.Errorf("got %v %v %v", c.TrustedProxies, c.OIDCRequireVerifiedEmail, err)
	}
	setEnv(t, map[string]string{"TRUSTED_PROXIES": "10.0.0.0/99"})
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Errorf("expected TRUSTED_PROXIES error, got %v", err)
	}
}

func TestWarnings(t *testing.T) {
	tests := []struct {
		base, listen string
		warn         bool
	}{
		{"http://x.example", ":8080", true},
		{"http://x.example", "0.0.0.0:8080", true},
		{"http://x.example", "127.0.0.1:8080", false},
		{"http://x.example", "[::1]:8080", false},
		{"http://localhost:8080", "localhost:8080", false},
		{"https://x.example", ":8080", false},
	}
	for _, tc := range tests {
		got := len((&Config{BaseURL: tc.base, ListenAddr: tc.listen}).Warnings()) > 0
		if got != tc.warn {
			t.Errorf("%s on %s: warn=%v", tc.base, tc.listen, got)
		}
	}
}
