package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func resolve(t *testing.T, trusted, remote string, xff ...string) string {
	t.Helper()
	p, err := ParseTrusted(trusted)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remote
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	ip := NewResolver(p).IP(req)
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

func TestIP(t *testing.T) {
	tests := []struct {
		name, trusted, remote string
		xff                   []string
		want                  string
	}{
		{"no proxies trusted ignores header", "", "203.0.113.5:1234", []string{"1.2.3.4"}, "203.0.113.5"},
		{"spoofed header from untrusted peer", "10.0.0.0/8", "203.0.113.5:1234", []string{"10.0.0.1"}, "203.0.113.5"},
		{"trusted peer, single hop", "10.0.0.1", "10.0.0.1:80", []string{"198.51.100.7"}, "198.51.100.7"},
		{"multiple hops, rightmost untrusted wins", "private", "10.0.0.1:80", []string{"6.6.6.6, 198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"client-supplied prefix ignored", "10.0.0.1", "10.0.0.1:80", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"several header lines", "private", "10.0.0.1:80", []string{"6.6.6.6", "198.51.100.7, 192.168.1.1"}, "198.51.100.7"},
		{"all hops trusted returns leftmost", "private", "10.0.0.1:80", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
		{"trusted peer without header", "private", "10.0.0.1:80", nil, "10.0.0.1"},
		{"malformed entry stops at verified hop", "private", "10.0.0.1:80", []string{"6.6.6.6, garbage, 10.0.0.9"}, "10.0.0.9"},
		{"malformed rightmost entry", "private", "10.0.0.1:80", []string{"198.51.100.7, garbage"}, "10.0.0.1"},
		{"empty header", "private", "10.0.0.1:80", []string{""}, "10.0.0.1"},
		{"ipv6 client", "private", "[::1]:80", []string{"2001:db8::7"}, "2001:db8::7"},
		{"ipv6 ULA proxy", "private", "[fd00::1]:80", []string{"2001:db8::7, fd00::2"}, "2001:db8::7"},
		{"ipv4-mapped peer", "10.0.0.0/8", "[::ffff:10.0.0.1]:80", []string{"198.51.100.7"}, "198.51.100.7"},
		{"ipv6 zone stripped", "", "[fe80::1%eth0]:80", nil, "fe80::1"},
		{"unparsable peer", "private", "garbage", []string{"1.2.3.4"}, ""},
		{"peer without port", "", "203.0.113.5", nil, "203.0.113.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(t, tc.trusted, tc.remote, tc.xff...); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrusted(t *testing.T) {
	for _, s := range []string{"", "private", "10.0.0.0/8, 192.168.1.1 ,2001:db8::/32", "PRIVATE,203.0.113.0/24"} {
		if _, err := ParseTrusted(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"10.0.0.0/33", "nope", "10.0.0.1,bad"} {
		if _, err := ParseTrusted(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}
