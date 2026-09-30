package http_test

import (
	"bytes"
	"log/slog"
	nethttp "net/http"
	"strings"
	"testing"

	"github.com/tiim/photo-collect/internal/clientip"
	"github.com/tiim/photo-collect/internal/config"
)

func logAfterRequest(t *testing.T, trusted string, secure bool, remote, xff string) string {
	t.Helper()
	var buf bytes.Buffer
	e := setupWith(t, func(c *config.Config) {
		p, err := clientip.ParseTrusted(trusted)
		if err != nil {
			t.Fatal(err)
		}
		c.TrustedProxies = p
		if secure {
			c.BaseURL = "https://example.test"
		}
	}, slog.New(slog.NewTextHandler(&buf, nil)))
	buf.Reset()
	req, _ := nethttp.NewRequest("GET", "/", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", xff)
	e.h.ServeHTTP(&discard{}, req)
	return buf.String()
}

type discard struct{ h nethttp.Header }

func (d *discard) Header() nethttp.Header {
	if d.h == nil {
		d.h = nethttp.Header{}
	}
	return d.h
}
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

func TestAccessLogClientIP(t *testing.T) {
	out := logAfterRequest(t, "10.0.0.0/8", false, "203.0.113.9:1", "1.2.3.4")
	if !strings.Contains(out, "remote=203.0.113.9") || strings.Contains(out, "1.2.3.4") {
		t.Errorf("spoofed header from untrusted peer must be ignored:\n%s", out)
	}
	out = logAfterRequest(t, "10.0.0.0/8", false, "10.0.0.2:1", "6.6.6.6, 198.51.100.7")
	if !strings.Contains(out, "remote=198.51.100.7") {
		t.Errorf("trusted proxy header should be used:\n%s", out)
	}
}

func TestForwardedWithoutTrustedProxiesWarnsOnce(t *testing.T) {
	out := logAfterRequest(t, "", true, "10.0.0.2:1", "198.51.100.7")
	if strings.Count(out, "TRUSTED_PROXIES is empty") != 1 {
		t.Errorf("expected one warning:\n%s", out)
	}
	if out := logAfterRequest(t, "", false, "10.0.0.2:1", "198.51.100.7"); strings.Contains(out, "TRUSTED_PROXIES") {
		t.Errorf("no warning expected for http BASE_URL:\n%s", out)
	}
}
