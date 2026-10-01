package http

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitOptions configures the rate limiting middleware.
type RateLimitOptions struct {
	// WindowMs is the time window in milliseconds. Default: 60000 (1 minute).
	WindowMs int
	// Max is the maximum number of requests per window. Default: 100.
	Max int
	// KeyFunc extracts a rate limit key from the request context.
	// Default: uses X-Forwarded-For or remote address.
	KeyFunc func(ctx *Context) string
	// Message is the response body when rate limited. Default: "Too Many Requests".
	Message string
	// TrustedProxies is a list of trusted proxy IP addresses or CIDR ranges.
	// When set, X-Forwarded-For is only used if the request comes from a trusted proxy.
	// When empty (default), only RemoteAddr is used for rate limit keying.
	TrustedProxies []string
	// Headers controls whether to include RateLimit-* response headers. Default: true.
	Headers *bool
	// StopCh is an optional channel that, when closed, stops the background cleanup goroutine.
	// If nil, the goroutine runs for the lifetime of the process.
	StopCh <-chan struct{}
}

type rateLimitEntry struct {
	count     int
	expiresAt time.Time
}

// rateLimiter manages per-key request counts with a sliding window.
type rateLimiter struct {
	mu          sync.Mutex
	entries     map[string]*rateLimitEntry
	window      time.Duration
	max         int
	lastCleanup time.Time
}

// allow checks if a request for the given key is allowed.
// When inlineCleanup is true, expired entries are cleaned up during the request
// instead of relying on a background goroutine.
// Returns the current count and window expiry time.
func (rl *rateLimiter) allow(key string, inlineCleanup bool) (count int, expiresAt time.Time) {
	now := time.Now()

	rl.mu.Lock()
	if inlineCleanup && now.Sub(rl.lastCleanup) >= rl.window {
		for k, e := range rl.entries {
			if now.After(e.expiresAt) {
				delete(rl.entries, k)
			}
		}
		rl.lastCleanup = now
	}
	entry, ok := rl.entries[key]
	if !ok || now.After(entry.expiresAt) {
		entry = &rateLimitEntry{
			count:     0,
			expiresAt: now.Add(rl.window),
		}
		rl.entries[key] = entry
	}
	entry.count++
	count = entry.count
	expiresAt = entry.expiresAt
	rl.mu.Unlock()

	return count, expiresAt
}

// cleanup removes expired entries from the limiter.
func (rl *rateLimiter) cleanup() {
	now := time.Now()
	rl.mu.Lock()
	for k, e := range rl.entries {
		if now.After(e.expiresAt) {
			delete(rl.entries, k)
		}
	}
	rl.mu.Unlock()
}

// startCleanup runs periodic cleanup until stopCh is closed.
func (rl *rateLimiter) startCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-stopCh:
			return
		}
	}
}

// RateLimit middleware limits requests per client using a sliding window.
//
//	server.Use(http.RateLimit(http.RateLimitOptions{
//	    WindowMs: 60_000,
//	    Max:      100,
//	}))
func RateLimit(opts RateLimitOptions) Middleware {
	if opts.WindowMs <= 0 {
		opts.WindowMs = 60_000
	}
	if opts.Max <= 0 {
		opts.Max = 100
	}
	if opts.KeyFunc == nil {
		opts.KeyFunc = buildRateLimitKeyFunc(opts.TrustedProxies)
	}
	if opts.Message == "" {
		opts.Message = "Too Many Requests"
	}
	includeHeaders := opts.Headers == nil || *opts.Headers

	rl := &rateLimiter{
		entries: make(map[string]*rateLimitEntry),
		window:  time.Duration(opts.WindowMs) * time.Millisecond,
		max:     opts.Max,
	}

	// Only start a background cleanup goroutine when an explicit StopCh is provided.
	// Otherwise, cleanup happens inline during request handling to avoid goroutine leaks.
	if opts.StopCh != nil {
		go rl.startCleanup(opts.StopCh)
	}

	return func(ctx *Context, next func() *Response) *Response {
		count, expiresAt := rl.allow(opts.KeyFunc(ctx), opts.StopCh == nil)

		remaining := opts.Max - count
		if remaining < 0 {
			remaining = 0
		}

		if includeHeaders {
			ctx.SetHeader("RateLimit-Limit", strconv.Itoa(opts.Max))
			ctx.SetHeader("RateLimit-Remaining", strconv.Itoa(remaining))
			ctx.SetHeader("RateLimit-Reset", strconv.FormatInt(expiresAt.Unix(), 10))
		}

		if count > opts.Max {
			retryAfter := time.Until(expiresAt).Seconds()
			if retryAfter < 1 {
				retryAfter = 1
			}
			resp := JSONStatus(429, map[string]string{
				"error":   "Too Many Requests",
				"message": opts.Message,
			})
			resp.Headers.Set("Retry-After", fmt.Sprintf("%d", int(retryAfter)))
			return resp
		}

		return next()
	}
}

// buildRateLimitKeyFunc returns a key function that extracts client IP.
// When trustedProxies is empty, only RemoteAddr is used (safe default).
// When trustedProxies is set, X-Forwarded-For is trusted only if the
// request arrives from a listed proxy. Each entry may be an exact IP address
// (e.g. "10.0.0.1") or a CIDR range (e.g. "10.0.0.0/8"); the request's
// RemoteAddr is matched against both.
//
// Trust is decided by the non-spoofable RemoteAddr, never by a header. But once
// the peer is trusted, the leftmost XFF hop is only as trustworthy as that
// proxy's configuration: configure the trusted proxy to overwrite (not append
// to) X-Forwarded-For with the real client address, or a client can prepend a
// forged hop and rotate its rate-limit key.
func buildRateLimitKeyFunc(trustedProxies []string) func(ctx *Context) string {
	var exact []netip.Addr
	var prefixes []netip.Prefix
	for _, p := range trustedProxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(p); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(p); err == nil {
			exact = append(exact, addr)
		}
	}
	hasTrusted := len(exact) > 0 || len(prefixes) > 0

	isTrusted := func(remoteAddr string) bool {
		addr, err := netip.ParseAddr(remoteAddr)
		if err != nil {
			return false
		}
		for _, e := range exact {
			if e == addr {
				return true
			}
		}
		for _, pfx := range prefixes {
			if pfx.Contains(addr) {
				return true
			}
		}
		return false
	}

	return func(ctx *Context) string {
		remoteAddr := ctx.Request.RemoteAddr
		// Strip port from RemoteAddr (host:port)
		if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
			remoteAddr = host
		}

		// Only trust X-Forwarded-For when the direct connection is from a trusted proxy
		if hasTrusted && isTrusted(remoteAddr) {
			if xff := ctx.Header("X-Forwarded-For"); xff != "" {
				if first, _, ok := strings.Cut(xff, ","); ok {
					return strings.TrimSpace(first)
				}
				return strings.TrimSpace(xff)
			}
		}

		return remoteAddr
	}
}
