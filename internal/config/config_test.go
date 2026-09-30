package config

import (
	"fmt"
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

func TestAbuseLimitDefaultsAndValidation(t *testing.T) {
	setEnv(t, map[string]string{"WORKER_COUNT": "3"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RateUploadPerIP != 100 || c.RateUploadPerLink != 300 || c.RateAuthPerIP != 10 || c.RateNicknamePerIP != 5 {
		t.Errorf("rate defaults wrong: %+v", c)
	}
	if c.UploadMaxConcurrent != 6 || c.ExportMaxConcurrent != 1 || c.ExportMaxBytes != 20<<30 {
		t.Errorf("concurrency/export defaults wrong: %d %d %d", c.UploadMaxConcurrent, c.ExportMaxConcurrent, c.ExportMaxBytes)
	}
	for _, kv := range []map[string]string{
		{"RATE_UPLOAD_PER_IP": "-1"}, {"UPLOAD_MAX_CONCURRENT": "-2"},
		{"EXPORT_MAX_CONCURRENT": "0"}, {"EXPORT_MAX_BYTES": "-1"},
	} {
		t.Run(fmt.Sprint(kv), func(t *testing.T) {
			setEnv(t, kv)
			if _, err := Load(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
	t.Run("zero disables", func(t *testing.T) {
		setEnv(t, map[string]string{"RATE_UPLOAD_PER_IP": "0", "EXPORT_MAX_BYTES": "0"})
		if c, err := Load(); err != nil || c.RateUploadPerIP != 0 || c.ExportMaxBytes != 0 {
			t.Errorf("0 must disable the limit: %v %v", c, err)
		}
	})
}
