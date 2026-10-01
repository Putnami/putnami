package main

import (
	stderrors "errors"
	stdhttp "net/http"
	"strings"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	"go.putnami.dev/security"

	"telemetry.putnami.dev/cliagg"
)

// aggregatePath is the private, aggregate-only read route.
// It is the ONLY non-anonymous route on this workload; POST /v1/logs stays
// exactly as it was, anonymous and unauthenticated.
const aggregatePath = "/v1/cli-usage/aggregate"

// windowParam is the single query parameter the contract accepts. Any other
// parameter is rejected, so a caller can never steer the datasource, workspace,
// project, tenant, or any upstream target: no such lever exists.
const windowParam = "window"

// aggregateAuth pins the service identity allowed to read aggregates. All three
// fields must be set for the route to answer at all — an unset audience,
// issuer, or caller allowlist denies every request (fail closed).
type aggregateAuth struct {
	// Audience is the exact "aud" the caller's token must carry.
	Audience string
	// Issuer is the OIDC issuer used for JWKS discovery AND enforced as the
	// exact "iss". Pinning both sides is what stops a token minted for another
	// audience or by another issuer from being replayed here.
	Issuer string
	// Callers is the allowlist of permitted subjects (exact) or email claims
	// (case-insensitive). An empty allowlist denies everyone.
	Callers []string
}

// configured reports whether the read route has a complete authentication
// configuration. Anything less is treated as "closed", never as "open".
func (a aggregateAuth) configured() bool {
	return strings.TrimSpace(a.Audience) != "" &&
		strings.TrimSpace(a.Issuer) != "" &&
		len(a.Callers) > 0
}

// allows reports whether the authenticated claims belong to a permitted caller.
// The issuer is re-checked here even though the resolver already pinned it, so
// the authorization decision never depends on resolver ordering.
func (a aggregateAuth) allows(user *phttp.Claims) bool {
	if user == nil || !a.configured() || user.Issuer != a.Issuer {
		return false
	}
	email, _ := user.Extra["email"].(string)
	for _, caller := range a.Callers {
		caller = strings.TrimSpace(caller)
		if caller == "" {
			continue
		}
		if user.Subject != "" && user.Subject == caller {
			return true
		}
		if email != "" && strings.EqualFold(email, caller) {
			return true
		}
	}
	return false
}

// aggregateEndpoint is everything the read route needs: the bounded report
// source, the caller pinning, and the clock that fixes the UTC window.
type aggregateEndpoint struct {
	source cliagg.ReportSource
	auth   aggregateAuth
	now    func() time.Time
	log    *logger.Logger
}

func (e *aggregateEndpoint) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *aggregateEndpoint) logger() *logger.Logger {
	if e.log != nil {
		return e.log
	}
	return logger.Default().Named("aggregate-read")
}

// middlewares returns the authentication chain for the read route, scoped so it
// never runs on the anonymous ingest route.
//
// Fail-closed shape: when the configuration is incomplete there is no identity
// resolver at all and the route is guarded by a flat deny. The route is still
// mounted (so the describe-time route inventory does not depend on deploy-time
// config), it simply answers 401 to everyone.
func (e *aggregateEndpoint) middlewares() []phttp.Middleware {
	if !e.auth.configured() {
		return []phttp.Middleware{onRoute(aggregatePath, denyAll())}
	}
	auth := e.auth
	resolver := security.JWKSJWT(security.JWKSJWTConfig{
		Issuer:         auth.Issuer,
		RequiredIssuer: auth.Issuer,
		Audience:       auth.Audience,
	})
	guard := security.Middleware(security.Guard(func(user *phttp.Claims, _ *phttp.Context) bool {
		return auth.allows(user)
	}))
	return []phttp.Middleware{
		onRoute(aggregatePath, resolver),
		onRoute(aggregatePath, guard),
	}
}

// onRoute restricts a middleware to one matched route. The server applies
// middleware to every route it serves, so without this the identity resolver and
// guard would also run on POST /v1/logs — turning the anonymous ingest route
// into an authenticated one and letting an anonymous client drive JWKS fetches.
func onRoute(route string, mw phttp.Middleware) phttp.Middleware {
	return func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.Route != route {
			return next()
		}
		return mw(ctx, next)
	}
}

// denyAll rejects every request it sees. It is the unconfigured-auth posture.
func denyAll() phttp.Middleware {
	return func(*phttp.Context, func() *phttp.Response) *phttp.Response {
		return phttp.Unauthorized()
	}
}

// handle serves GET /v1/cli-usage/aggregate. Status contract:
//
//   - unknown query parameter → 400 (no steering lever exists, and the
//     offending name is never echoed back)
//   - window outside {1d,7d,30d} → 400 with the fixed allowed list
//   - datasource/complete projection unavailable → 503
//   - query failure              → 500 with a fixed code
//   - otherwise                  → 200 with the suppressed report
//
// Every error body is a fixed code: no request value, database error text,
// device id, or IP is ever reflected.
func (e *aggregateEndpoint) handle(ctx *phttp.Context) *phttp.Response {
	for name := range ctx.QueryParams() {
		if name != windowParam {
			return phttp.JSONStatus(stdhttp.StatusBadRequest, map[string]any{
				"error":     "unsupported_parameter",
				"supported": []string{windowParam},
			})
		}
	}

	spec := strings.TrimSpace(ctx.Query(windowParam))
	if spec == "" {
		spec = cliagg.DefaultWindow
	}
	window, err := cliagg.ParseWindow(spec, e.clock())
	if err != nil {
		return phttp.JSONStatus(stdhttp.StatusBadRequest, map[string]any{
			"error":   "unsupported_window",
			"allowed": cliagg.Windows(),
		})
	}

	if e.source == nil {
		return unavailableResponse()
	}
	report, err := e.source.Report(ctx.Context(), window)
	if err != nil {
		if stderrors.Is(err, cliagg.ErrNoDatasource) ||
			stderrors.Is(err, cliagg.ErrProjectionOverflow) {
			return unavailableResponse()
		}
		// The statement text and its parameters are compile-time constants plus
		// computed calendar dates, so the driver error carries no request or
		// subject data; the response still says nothing but a fixed code.
		e.logger().Warn("cli-usage aggregate read failed: " + err.Error())
		return phttp.JSONStatus(stdhttp.StatusInternalServerError, map[string]any{
			"error": "aggregate_read_failed",
		})
	}
	// no-store: the response is scoped to one authenticated caller and must not
	// be retained by a proxy or a shared cache on the way back.
	return phttp.JSON(report).WithHeader("Cache-Control", "no-store")
}

func unavailableResponse() *phttp.Response {
	return phttp.JSONStatus(stdhttp.StatusServiceUnavailable, map[string]any{
		"error": "aggregate_store_unavailable",
	})
}
