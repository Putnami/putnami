// Package security provides authentication and authorization middleware
// for the Putnami Go framework. It supports declarative role/scope/claim-based
// access control and custom guard functions.
package security

import (
	"strings"

	phttp "go.putnami.dev/http"
)

// Rule is a security authorization rule that can produce middleware.
// Implemented by Options and Guard.
type Rule interface {
	// Middleware returns the authorization middleware for this rule.
	Middleware() phttp.Middleware
}

// Options defines declarative access control rules.
type Options struct {
	// Roles requires all listed roles to be present.
	Roles []string
	// RolesAny requires at least one of the listed roles.
	RolesAny []string
	// Scopes requires all listed scopes to be present.
	Scopes []string
	// ScopesAny requires at least one of the listed scopes.
	ScopesAny []string
	// Client restricts access to specific client IDs.
	Client []string
	// ExcludePaths are path prefixes that skip authorization (e.g. "/_/").
	ExcludePaths []string
	// Optional serves a request that presents no credential: no identity and an
	// absent or empty Authorization header. Every other field applies to an
	// authenticated caller only. A request whose non-empty Authorization header
	// resolved no identity is still refused with 401, never served anonymously. The published client
	// contract may then offer an anonymous alternative after the credentialed
	// ones (ADR 0002).
	Optional bool
}

// Middleware implements Rule for Options.
func (o Options) Middleware() phttp.Middleware {
	return optionsMiddleware(o)
}

// SecurityRoles returns the required roles, implementing the optional
// phttp.SecurityClaims interface so documentation generators read declared
// roles through a typed accessor rather than reflecting on field names.
func (o Options) SecurityRoles() []string { return o.Roles }

// SecurityScopes returns the required scopes (see SecurityRoles).
func (o Options) SecurityScopes() []string { return o.Scopes }

// OptionalAuthentication reports Optional, implementing the optional
// phttp.SecurityOptionalAuthentication claim contract generators read.
func (o Options) OptionalAuthentication() bool { return o.Optional }

// Guard is a custom authorization function.
// Return true to allow access, false to deny.
type Guard func(user *phttp.Claims, ctx *phttp.Context) bool

// Middleware implements Rule for Guard.
func (g Guard) Middleware() phttp.Middleware {
	return guardMiddleware(g)
}

// Middleware creates authorization middleware from a Rule.
//
// With declarative options:
//
//	security.Middleware(security.Options{Roles: []string{"admin"}})
//
// With a custom guard:
//
//	security.Middleware(security.Guard(func(user *phttp.Claims, ctx *phttp.Context) bool {
//	    return user.Subject == ctx.Param("userId")
//	}))
func Middleware(rule Rule) phttp.Middleware {
	return rule.Middleware()
}

func optionsMiddleware(opts Options) phttp.Middleware {
	return func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if isExcludedPath(ctx.Path, opts.ExcludePaths) {
			return next()
		}
		if ctx.User == nil {
			if admitsAnonymous(ctx, opts) {
				recordDecision(ctx, decisionAllow, "")
				return next()
			}
			recordDecision(ctx, decisionDenyUnauthenticated, "no identity")
			return phttp.Unauthorized()
		}

		// Check client
		if len(opts.Client) > 0 {
			if !contains(opts.Client, ctx.User.ClientID) {
				recordDecision(ctx, decisionDenyClient, "client not allowed")
				return phttp.Forbidden()
			}
		}

		// Check all required scopes
		if len(opts.Scopes) > 0 {
			if !containsAll(ctx.User.Scopes, opts.Scopes) {
				recordDecision(ctx, decisionDenyScope, "missing required scope")
				return phttp.Forbidden()
			}
		}

		// Check any scope
		if len(opts.ScopesAny) > 0 {
			if !containsAny(ctx.User.Scopes, opts.ScopesAny) {
				recordDecision(ctx, decisionDenyScope, "missing any scope")
				return phttp.Forbidden()
			}
		}

		// Check all required roles
		if len(opts.Roles) > 0 {
			if !containsAll(ctx.User.Roles, opts.Roles) {
				recordDecision(ctx, decisionDenyRole, "missing required role")
				return phttp.Forbidden()
			}
		}

		// Check any role
		if len(opts.RolesAny) > 0 {
			if !containsAny(ctx.User.Roles, opts.RolesAny) {
				recordDecision(ctx, decisionDenyRole, "missing any role")
				return phttp.Forbidden()
			}
		}

		recordDecision(ctx, decisionAllow, "")
		return next()
	}
}

// admitsAnonymous reports whether an optional rule serves a request that
// resolved no identity. Only a request whose Authorization header is absent or
// empty after trimming qualifies, because an empty header presents no
// credential; a credential the resolvers refused is answered 401, so a caller
// holding an expired or forged token learns it instead of silently receiving
// the anonymous answer, and a generated client can invalidate and re-acquire
// its credential. The TypeScript twin is admitsAnonymous in
// typescript/framework/application/src/security/security.middleware.ts.
func admitsAnonymous(ctx *phttp.Context, opts Options) bool {
	return opts.Optional && strings.TrimSpace(ctx.Header("Authorization")) == ""
}

func guardMiddleware(guard Guard) phttp.Middleware {
	return func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.User == nil {
			recordDecision(ctx, decisionDenyUnauthenticated, "no identity")
			return phttp.Unauthorized()
		}
		if !guard(ctx.User, ctx) {
			recordDecision(ctx, decisionDenyGuard, "guard rejected")
			return phttp.Forbidden()
		}
		recordDecision(ctx, decisionAllow, "")
		return next()
	}
}

// IdentityResolver creates middleware that extracts user identity from the request.
// The resolve function should parse the Authorization header (or other mechanism)
// and return user claims, or nil if not authenticated.
//
// If ctx.User is already set (by a previous resolver), the resolve function
// is not called. This allows chaining multiple resolvers where the first
// successful one wins.
func IdentityResolver(resolve func(ctx *phttp.Context) *phttp.Claims) phttp.Middleware {
	return func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.User == nil {
			if user := resolve(ctx); user != nil {
				ctx.User = user
			}
		}
		return next()
	}
}

// --- helpers ---

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, s := range haystack {
		set[s] = true
	}
	for _, need := range needles {
		if !set[need] {
			return false
		}
	}
	return true
}

func isExcludedPath(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func containsAny(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, s := range haystack {
		set[s] = true
	}
	for _, need := range needles {
		if set[need] {
			return true
		}
	}
	return false
}
