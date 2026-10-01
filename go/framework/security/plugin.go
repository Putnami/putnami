package security

import (
	"context"
	"log/slog"

	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
)

// Config configures authentication and authorization.
type Config struct {
	// JWKSURL is the URL to fetch the JSON Web Key Set from.
	JWKSURL string `json:"jwksUrl" env:"AUTH_JWKS_URL"`

	// Issuer is the OIDC issuer URL. The JWKS URL is discovered
	// via /.well-known/openid-configuration.
	Issuer string `json:"issuer" env:"AUTH_ISSUER"`

	// Audience, if set, validates the "aud" claim matches.
	Audience string `json:"audience" env:"AUTH_AUDIENCE"`

	// RequiredIssuer, if set, requires the "iss" claim to equal this value.
	// When Issuer (OIDC discovery) is configured the expected issuer defaults
	// to Issuer; RequiredIssuer is for the JWKSURL path or to override it.
	RequiredIssuer string `json:"requiredIssuer" env:"AUTH_REQUIRED_ISSUER"`

	// Scopes requires all listed scopes on every authenticated request.
	Scopes []string `json:"scopes"`

	// ExcludePaths are path prefixes that skip authorization (e.g. "/_/").
	ExcludePaths []string `json:"excludePaths"`

	// AllowUnauthenticated disables authentication entirely. When false (the
	// default), a plugin configured without JWKSURL or Issuer fails closed:
	// every request to a non-excluded path is rejected with 401 instead of
	// being served unauthenticated. Set true only to intentionally run an
	// open server (e.g. local development), making the insecure choice explicit.
	AllowUnauthenticated bool `json:"allowUnauthenticated" env:"AUTH_ALLOW_UNAUTHENTICATED"`

	// AllowInsecure permits fetching JWKS/OIDC metadata over plaintext http
	// from non-loopback hosts. Defaults to false (https required). Do not
	// enable in production.
	AllowInsecure bool `json:"allowInsecure" env:"AUTH_ALLOW_INSECURE"`
}

// Plugin applies authentication and authorization middleware to the server.
// If neither JWKSURL nor Issuer is configured the plugin fails closed,
// rejecting every request to a non-excluded path, unless AllowUnauthenticated
// is set.
type Plugin struct {
	server     *phttp.ServerPlugin
	cfg        Config
	configured bool
}

// NewPlugin creates a security plugin that applies authentication
// and authorization middleware to the given server during Configure.
func NewPlugin(server *phttp.ServerPlugin, cfg Config) *Plugin {
	return &Plugin{server: server, cfg: cfg}
}

// Name implements app.Plugin.
func (p *Plugin) Name() string { return "security" }

// Configure implements app.Configurer. Applies identity resolution and
// authorization middleware if configured.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	if p.configured {
		return nil // idempotent — middleware already applied
	}
	p.configured = true

	if p.cfg.JWKSURL == "" && p.cfg.Issuer == "" {
		if p.cfg.AllowUnauthenticated {
			decisionLogger().Warn("security plugin running without authentication — all requests will be accepted (AllowUnauthenticated is set)")
			return nil
		}
		// Fail closed: no identity resolver can be built, so reject every
		// request to a non-excluded path instead of silently serving open. Do
		// not delegate to Options middleware here; that would allow a request
		// through if ctx.User had already been populated by another middleware.
		decisionLogger().Error("security plugin configured without JWKS URL or Issuer — failing closed; requests to non-excluded paths will be rejected with 401 (set AllowUnauthenticated to run open)", nil)
		p.server.Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
			if isExcludedPath(ctx.Path, p.cfg.ExcludePaths) {
				return next()
			}
			recordDecision(ctx, decisionDenyUnauthenticated, "no identity resolver configured")
			return phttp.Unauthorized()
		})
		return nil
	}

	p.server.Use(JWKSJWT(JWKSJWTConfig{
		JWKSURL:        p.cfg.JWKSURL,
		Issuer:         p.cfg.Issuer,
		Audience:       p.cfg.Audience,
		RequiredIssuer: p.cfg.RequiredIssuer,
		AllowInsecure:  p.cfg.AllowInsecure,
	}))

	p.server.Use(Options{
		Scopes:       p.cfg.Scopes,
		ExcludePaths: p.cfg.ExcludePaths,
	}.Middleware())

	decisionLogger().Debug("authentication enabled",
		slog.String("issuer", p.cfg.Issuer),
		slog.String("jwks", p.cfg.JWKSURL),
	)

	return nil
}
