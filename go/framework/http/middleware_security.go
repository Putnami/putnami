package http

import (
	"fmt"
	"strings"
)

// CORSOptions configures CORS behavior.
type CORSOptions struct {
	// AllowOrigins is the list of allowed origins. Use "*" to allow all.
	AllowOrigins []string
	// AllowMethods is the list of allowed HTTP methods. Defaults to GET, POST, PUT, PATCH, DELETE.
	AllowMethods []string
	// AllowHeaders is the list of allowed request headers. Defaults to Content-Type, Authorization.
	AllowHeaders []string
	// ExposeHeaders is the list of headers clients can access.
	ExposeHeaders []string
	// AllowCredentials indicates whether credentials (cookies, auth headers) are allowed.
	AllowCredentials bool
	// MaxAge is the preflight cache duration in seconds. Defaults to 86400 (24h).
	MaxAge int
}

// CORS returns a middleware that handles Cross-Origin Resource Sharing.
func CORS(opts CORSOptions) Middleware {
	if len(opts.AllowOrigins) == 0 {
		opts.AllowOrigins = []string{"*"}
	}
	if len(opts.AllowMethods) == 0 {
		opts.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	}
	if len(opts.AllowHeaders) == 0 {
		opts.AllowHeaders = []string{"Content-Type", "Authorization"}
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = 86400
	}

	methods := strings.Join(opts.AllowMethods, ", ")
	headers := strings.Join(opts.AllowHeaders, ", ")
	maxAge := fmt.Sprintf("%d", opts.MaxAge)

	return func(ctx *Context, next func() *Response) *Response {
		origin := ctx.Header("Origin")
		if origin == "" {
			return next()
		}

		allowed := false
		matchedExplicit := false
		for _, o := range opts.AllowOrigins {
			if o == origin {
				allowed = true
				matchedExplicit = true
				break
			}
			if o == "*" {
				allowed = true
			}
		}
		if !allowed {
			return next()
		}

		ctx.SetHeader("Access-Control-Allow-Origin", origin)
		// The reflected Access-Control-Allow-Origin varies by request Origin, so
		// a shared/CDN cache keyed on method+URL must key on Origin too —
		// otherwise it can replay one origin's ACAO (or credentialed grant) to
		// another. Append rather than Set so an existing Vary (e.g. compression's
		// Accept-Encoding) is preserved.
		ctx.Writer.Header().Add("Vary", "Origin")
		// Secure-by-default: never combine credentials with a wildcard origin.
		// Reflecting an arbitrary Origin together with
		// Access-Control-Allow-Credentials: true lets any site read
		// authenticated responses — the misconfiguration the "* + credentials"
		// rule in the CORS spec forbids. Only emit the credentials header when
		// the origin matched an explicit (non-wildcard) allowlist entry.
		if opts.AllowCredentials && matchedExplicit {
			ctx.SetHeader("Access-Control-Allow-Credentials", "true")
		}
		if len(opts.ExposeHeaders) > 0 {
			ctx.SetHeader("Access-Control-Expose-Headers", strings.Join(opts.ExposeHeaders, ", "))
		}

		// Handle preflight
		if ctx.Method == "OPTIONS" {
			ctx.SetHeader("Access-Control-Allow-Methods", methods)
			ctx.SetHeader("Access-Control-Allow-Headers", headers)
			ctx.SetHeader("Access-Control-Max-Age", maxAge)
			return NoContent()
		}

		return next()
	}
}

// SecurityHeaders returns a middleware that sets standard security headers
// on every response to protect against common web vulnerabilities.
func SecurityHeaders() Middleware {
	return func(ctx *Context, next func() *Response) *Response {
		ctx.SetHeader("X-Content-Type-Options", "nosniff")
		ctx.SetHeader("X-Frame-Options", "DENY")
		ctx.SetHeader("X-XSS-Protection", "0")
		ctx.SetHeader("Referrer-Policy", "strict-origin-when-cross-origin")
		return next()
	}
}
