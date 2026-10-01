package http

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/app"
	diag "go.putnami.dev/protocol/diagnostic"
	httproutes "go.putnami.dev/protocol/http-routes"
)

// HTTPRoutesDescribePath is the generated route inventory consumed by deploys.
const HTTPRoutesDescribePath = "schema/http-routes.json"

type httpRouteSource string

const (
	routeSourceManual      httpRouteSource = "manual"
	routeSourceTypedAPI    httpRouteSource = "typed-api"
	routeSourceStaticMount httpRouteSource = "static-mount"
)

type httpRouteFact struct {
	method string
	path   string
	source httpRouteSource
	// match, when non-empty, overrides the path-derived match kind. A mount
	// registers a MatchPrefix fact explicitly because its runtime pattern (a
	// trailing named catch-all) cannot be represented by the character-based
	// derivation.
	match httproutes.MatchKind
}

type groupedHTTPRoute struct {
	route   httproutes.Route
	methods map[string]struct{}
}

// Describe emits the configured server's complete, canonical HTTP route
// inventory. It runs after every Configurer, so api.Plugin registrations and
// direct server routes are both present without using OpenAPI as discovery.
func (p *ServerPlugin) Describe(ctx *app.DescribeContext) error {
	if !ctx.Wants(p.Name()) {
		return nil
	}

	routes := p.httpRoutes()
	manifest, diags := httproutes.Canonicalize(routes)
	if diag.HasErrors(diags) {
		return formatHTTPRouteDiagnostics(diags)
	}
	body, diags := httproutes.CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		return formatHTTPRouteDiagnostics(diags)
	}

	out := filepath.Join(ctx.OutputDir, HTTPRoutesDescribePath)
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return err
	}
	return os.WriteFile(out, body, 0o600)
}

func (p *ServerPlugin) httpRoutes() []httproutes.Route {
	grouped := make(map[string]*groupedHTTPRoute)
	for _, fact := range p.httpRouteFacts {
		method := strings.ToUpper(fact.method)
		match := fact.match
		if match == "" {
			match = httproutes.MatchExact
			if strings.ContainsAny(fact.path, "{}[]:*") {
				match = httproutes.MatchTemplate
			}
		}
		key := string(match) + "\x00" + fact.path + "\x00" + string(fact.source)
		entry := grouped[key]
		if entry == nil {
			entry = &groupedHTTPRoute{
				route: httproutes.Route{
					Match:      match,
					Path:       fact.path,
					PublicEdge: true,
					Provenance: httproutes.Provenance{
						Project:    p.projectName,
						Package:    "go.putnami.dev/http",
						SourceKind: httproutes.SourceKind(fact.source),
					},
				},
				methods: make(map[string]struct{}),
			}
			grouped[key] = entry
		}
		entry.methods[method] = struct{}{}
		// The Go server explicitly falls HEAD back to a matching GET handler.
		if method == "GET" {
			entry.methods["HEAD"] = struct{}{}
		}
	}

	routes := make([]httproutes.Route, 0, len(grouped))
	for _, entry := range grouped {
		for method := range entry.methods {
			entry.route.Methods = append(entry.route.Methods, method)
		}
		sort.Strings(entry.route.Methods)
		routes = append(routes, entry.route)
	}
	return routes
}

func formatHTTPRouteDiagnostics(diags []diag.Diagnostic) error {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Field == "" {
			parts = append(parts, fmt.Sprintf("[%s] %s", d.Code, d.Message))
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", d.Code, d.Field, d.Message))
	}
	return fmt.Errorf("http route inventory validation failed: %s", strings.Join(parts, "; "))
}
