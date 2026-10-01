package http

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/logger"
)

// HealthCheckFunc is a function that checks the health of a dependency.
// It should return nil if healthy, or an error describing the issue.
type HealthCheckFunc func(ctx context.Context) error

// HealthPlugin provides liveness/readiness check endpoints.
// It exposes GET /_/health which returns 200 when the application is ready
// and 503 during startup or shutdown.
//
// A plugin added to an application mounts itself: during Configure it
// registers GET /_/health on the application's single ServerPlugin. RegisterOn
// chooses the server instead, for an application that holds several or keeps
// its server outside the module tree. A caller that takes Handler mounts the
// handler on its own route, and the plugin then registers nothing.
//
// Probes come from two sources:
//
//   - AddChecker registers a named callback explicitly (use for ad-hoc
//     probes that aren't owned by a plugin — e.g. an external URL).
//
//   - Plugins implementing app.HealthChecker (and probes added via
//     app.Contribute[app.HealthChecker]) are auto-discovered from the
//     module tree. The plugin's Name() is used as the probe name. This is
//     the preferred path for any probe whose state lives inside a plugin
//     (database pools, cache clients, upstream service clients).
type HealthPlugin struct {
	ready atomic.Bool
	// mounted is set once the handler has a route: RegisterOn registered it, or
	// a caller took it with Handler.
	mounted      atomic.Bool
	checkers     map[string]HealthCheckFunc
	root         *app.Module
	discoverOnce sync.Once
	log          *logger.Logger
}

// NewHealthPlugin creates a new health check plugin.
func NewHealthPlugin() *HealthPlugin {
	return &HealthPlugin{
		checkers: make(map[string]HealthCheckFunc),
		log:      logger.Default().Named("health"),
	}
}

// Name returns the plugin name.
func (p *HealthPlugin) Name() string { return "health" }

// AddChecker registers a named health checker for a downstream dependency.
// Checkers are called on each health request and their status is included
// in the response.
//
// For probes owned by a plugin, prefer implementing app.HealthChecker on
// the plugin itself — the HealthPlugin will discover and register it
// automatically.
func (p *HealthPlugin) AddChecker(name string, checker HealthCheckFunc) {
	p.checkers[name] = checker
}

// Configure captures the module tree root so probe discovery can run
// later, on the first health request. Discovery is NOT done here: plugin
// Configure runs sequentially in registration order, so a plugin that
// registers a probe via app.Contribute[app.HealthChecker] in its own
// Configure would be invisible to a HealthPlugin configured before it.
// Deferring to the request path makes the order irrelevant — by the time
// any probe is served, every plugin has finished configuring.
//
// Configure also mounts the plugin when nothing mounted it: it registers the
// health endpoint on the single ServerPlugin of the owner's module tree. A
// tree with no server, or with several, fails with the SingleServer error. A
// mounted plugin is left as it is, so configuring again registers nothing. A
// nil owner has no module tree: nothing is captured and nothing is mounted.
func (p *HealthPlugin) Configure(_ context.Context, owner *app.Module) error {
	if owner == nil {
		return nil
	}
	p.root = owner.Root()
	if p.mounted.Load() {
		return nil
	}
	server, err := SingleServer(owner, p.Name())
	if err != nil {
		return err
	}
	p.RegisterOn(server)
	return nil
}

// ensureDiscovered runs probe auto-discovery exactly once, walking the
// captured module tree for app.HealthChecker implementers (and values
// registered via app.Contribute). Probes added via AddChecker are
// preserved; auto-discovered probes do not overwrite an explicit
// AddChecker entry of the same name.
func (p *HealthPlugin) ensureDiscovered() {
	p.discoverOnce.Do(func() {
		if p.root == nil {
			return
		}
		for _, hc := range app.Collect[app.HealthChecker](p.root) {
			if _, exists := p.checkers[hc.Name()]; exists {
				continue
			}
			p.checkers[hc.Name()] = hc.CheckHealth
		}
	})
}

// Start marks the application as ready.
func (p *HealthPlugin) Start(_ context.Context, _ *app.Module) error {
	p.ready.Store(true)
	return nil
}

// Stop marks the application as not ready.
func (p *HealthPlugin) Stop(_ context.Context, _ *app.Module) error {
	p.ready.Store(false)
	return nil
}

// Handler returns the health check HTTP handler. The caller mounts it on a
// route of its own, so the plugin counts as mounted and Configure registers
// no route.
func (p *HealthPlugin) Handler() Handler {
	p.mounted.Store(true)
	return func(ctx *Context) *Response {
		if !p.ready.Load() {
			return JSONStatus(503, map[string]string{"status": "unavailable"})
		}

		p.ensureDiscovered()

		if len(p.checkers) == 0 {
			return JSON(map[string]string{"status": "ok"})
		}

		checkCtx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
		defer cancel()

		checks := make(map[string]string, len(p.checkers))
		allHealthy := true
		for name, checker := range p.checkers {
			if err := checker(checkCtx); err != nil {
				// Do not leak raw dependency error text (DSNs, hostnames,
				// upstream URLs, driver messages) to unauthenticated clients.
				// Report a sanitized status and keep the detail server-side.
				if p.log != nil {
					p.log.Error("health check failed", err, slog.String("check", name))
				}
				checks[name] = "unavailable"
				allHealthy = false
			} else {
				checks[name] = "ok"
			}
		}

		result := map[string]any{
			"status": "ok",
			"checks": checks,
		}
		if !allHealthy {
			result["status"] = "degraded"
			return JSONStatus(503, result)
		}
		return JSON(result)
	}
}

// RegisterOn registers the health endpoint on server and marks the plugin
// mounted. Call it before the application configures to choose the server;
// without it, Configure mounts the plugin on the application's single server.
func (p *HealthPlugin) RegisterOn(server *ServerPlugin) {
	server.GET("/_/health", p.Handler())
}
