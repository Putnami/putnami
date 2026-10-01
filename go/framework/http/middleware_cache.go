package http

import (
	"strconv"
	"strings"
)

// CacheOptions configures the Cache-Control response header written by Cache.
type CacheOptions struct {
	// MaxAge is the max-age in seconds: how long a response may be reused before
	// it is considered stale. Zero omits the directive.
	MaxAge int
	// Private restricts storage to a single user's cache (Cache-Control: private)
	// instead of allowing shared/proxy caches. The default is public.
	Private bool
	// NoCache requires a cache to revalidate with the origin before reusing a
	// stored response (Cache-Control: no-cache).
	NoCache bool
	// NoStore disables caching entirely (Cache-Control: no-store). It takes
	// precedence over every other field.
	NoStore bool
}

// Cache returns a middleware that sets the Cache-Control response header from
// opts. Unlike recording cache metadata for documentation only, this enforces
// the directive at runtime, giving .Cache() parity with CORS and RateLimit.
func Cache(opts CacheOptions) Middleware {
	value := cacheControlValue(opts)
	return func(ctx *Context, next func() *Response) *Response {
		if value != "" {
			ctx.SetHeader("Cache-Control", value)
		}
		return next()
	}
}

// cacheControlValue renders the Cache-Control header value for opts. no-store
// wins outright; otherwise the response is marked public or private, with
// optional no-cache and max-age directives appended.
func cacheControlValue(opts CacheOptions) string {
	if opts.NoStore {
		return "no-store"
	}
	directives := make([]string, 0, 3)
	if opts.Private {
		directives = append(directives, "private")
	} else {
		directives = append(directives, "public")
	}
	if opts.NoCache {
		directives = append(directives, "no-cache")
	}
	if opts.MaxAge > 0 {
		directives = append(directives, "max-age="+strconv.Itoa(opts.MaxAge))
	}
	return strings.Join(directives, ", ")
}
