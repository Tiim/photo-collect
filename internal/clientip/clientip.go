// Package clientip resolves the address of the real client of a request,
// honouring X-Forwarded-For only when the direct peer is a trusted proxy.
package clientip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// private is the meaning of the "private" shorthand: RFC 1918, loopback and IPv6 ULA.
var private = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
}

// ParseTrusted parses a comma-separated list of CIDRs, single IPs and the
// shorthand "private". An empty string trusts nothing.
func ParseTrusted(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			continue
		case strings.EqualFold(part, "private"):
			out = append(out, private...)
		case strings.Contains(part, "/"):
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
			}
			out = append(out, p.Masked())
		default:
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("invalid IP %q: %w", part, err)
			}
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out, nil
}

// Resolver derives client IPs.
type Resolver struct {
	trusted []netip.Prefix
}

func NewResolver(trusted []netip.Prefix) *Resolver { return &Resolver{trusted: trusted} }

// HasTrusted reports whether any proxy is trusted.
func (r *Resolver) HasTrusted() bool { return len(r.trusted) > 0 }

func (r *Resolver) isTrusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IP returns the client address of req. X-Forwarded-For is only consulted when
// the direct peer is trusted; the chain is walked from the right and the first
// untrusted hop wins, so entries a client prepends itself are ignored. A
// malformed entry ends the walk at the last hop that could be verified. The
// zero Addr is returned if even the peer address cannot be parsed.
func (r *Resolver) IP(req *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap().WithZone("")
	if !r.isTrusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	candidate := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return candidate
		}
		a = a.Unmap().WithZone("")
		if !r.isTrusted(a) {
			return a
		}
		candidate = a
	}
	return candidate
}

type ctxKey struct{}

// WithContext stores ip in ctx.
func WithContext(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, ctxKey{}, ip)
}

// FromContext returns the resolved client IP, or the zero Addr.
func FromContext(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(ctxKey{}).(netip.Addr)
	return ip
}

// String returns the resolved IP as text ("unknown" if none).
func String(ctx context.Context) string {
	if ip := FromContext(ctx); ip.IsValid() {
		return ip.String()
	}
	return "unknown"
}
