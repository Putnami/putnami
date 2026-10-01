package main

import (
	"net"
	"net/netip"
	"strings"

	phttp "go.putnami.dev/http"
)

// trustSet is a parsed trusted-proxy allowlist (exact addresses + CIDR ranges).
type trustSet struct {
	exact    []netip.Addr
	prefixes []netip.Prefix
}

func parseTrustSet(entries []string) trustSet {
	var ts trustSet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(e); err == nil {
			ts.prefixes = append(ts.prefixes, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(e); err == nil {
			ts.exact = append(ts.exact, addr)
		}
	}
	return ts
}

func (ts trustSet) empty() bool { return len(ts.exact) == 0 && len(ts.prefixes) == 0 }

func (ts trustSet) contains(addr netip.Addr) bool {
	for _, e := range ts.exact {
		if e == addr {
			return true
		}
	}
	for _, p := range ts.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP derives the rate-limit client key from a request in a
// trusted-proxy-aware, fail-safe way. It NEVER trusts a header decided by a
// spoofable source:
//
//   - Trust is decided only by the non-spoofable direct peer (RemoteAddr).
//   - An empty trusted list ⇒ key on RemoteAddr. Behind a shared proxy this
//     over-throttles (every client collapses onto the proxy IP), but it never
//     honors a forgeable header — the safe default.
//   - When the direct peer is a trusted proxy, the X-Forwarded-For chain is
//     walked right-to-left and the first (rightmost) non-trusted hop is used —
//     the closest address we cannot vouch for. This defeats a client that
//     prepends forged hops to rotate its key.
//
// The returned string is used only as an in-memory rate-limit key with a short
// TTL; it is never persisted to telemetry or logs.
func clientIP(ctx *phttp.Context, ts trustSet) string {
	remote := ctx.Request.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}

	if ts.empty() {
		return remote
	}

	remoteAddr, err := netip.ParseAddr(remote)
	if err != nil || !ts.contains(remoteAddr) {
		// Direct peer is not a trusted proxy: its XFF is unverifiable.
		return remote
	}

	hops := strings.Split(ctx.Header("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			continue
		}
		if ts.contains(addr) {
			continue // skip trusted hops, keep walking left
		}
		return addr.String()
	}

	// Every XFF hop is trusted (or none parsed): fall back to the direct peer.
	return remote
}
