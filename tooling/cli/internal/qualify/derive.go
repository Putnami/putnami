// Package qualify derives a workload's smoke contract from its route inventory,
// executes it against a target, and reduces the run to one fail-closed verdict
// (go.putnami.dev/protocol/qualify).
//
// It owns no process and no composition: a Target hands Execute a base URL and
// the binding it can prove, and takes back whatever it opened on Close. The URL
// target lives here; a local target composes through the compose package.
package qualify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	httproutes "go.putnami.dev/protocol/http-routes"
	protocolplatform "go.putnami.dev/protocol/platform"
	qualifyproto "go.putnami.dev/protocol/qualify"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// MaxRequests caps a derived contract: the smoke proves a workload serves real
// requests, not that every route does.
const MaxRequests = 25

// Unsupported reasons.
const (
	// ReasonNoRouteInventory means neither schema/http-routes.json nor
	// .gen/schema/http-routes.json exists for the project.
	ReasonNoRouteInventory = "no_route_inventory"
	// ReasonNoDerivableRequest means the inventory exists but declares no route
	// that is safe to request.
	ReasonNoDerivableRequest = "no_derivable_request"
)

// inventoryLocations are read in order; the first that exists is the source.
var inventoryLocations = []string{
	filepath.Join("schema", "http-routes.json"),
	filepath.Join(".gen", "schema", "http-routes.json"),
}

// Unsupported explains why a contract holds no request. A verdict for such a
// contract is unsupported, never passed.
type Unsupported struct {
	// Reason is ReasonNoRouteInventory or ReasonNoDerivableRequest.
	Reason string
	// Remedy is the action that makes the workload qualifiable.
	Remedy string
	// Source is the workspace-relative inventory path, when one was read.
	Source string
}

// Diagnostic renders u as the phase diagnostic a verdict records.
func (u *Unsupported) Diagnostic() diag.Diagnostic {
	code := qualifyproto.PhaseCodeNoDerivableRequest
	if u.Reason == ReasonNoRouteInventory {
		code = qualifyproto.PhaseCodeNoRouteInventory
	}
	return diag.Errorf(code, u.Source, "%s", u.Remedy)
}

// InvalidInventoryError reports a route inventory that exists but fails the
// strict http-routes parser. Qualification refuses to guess past it.
type InvalidInventoryError struct {
	// Path is the workspace-relative inventory path.
	Path string
	// Diagnostics are the parser's findings.
	Diagnostics []diag.Diagnostic
}

func (e *InvalidInventoryError) Error() string {
	messages := make([]string, 0, len(e.Diagnostics))
	for _, d := range e.Diagnostics {
		messages = append(messages, d.String())
	}
	return fmt.Sprintf("route inventory %s is invalid: %s", e.Path, strings.Join(messages, "; "))
}

// Derive builds p's smoke contract from its route inventory.
//
// A route is eligible when it matches exactly, accepts GET or HEAD, is
// publicEdge, is none of the platform canonical paths (bare or under
// platformPrefix) and is not under /debug/pprof or /_putnami/. Eligible routes
// become one request per path (GET preferred), sorted by path and capped at
// MaxRequests. An empty contract comes with an Unsupported explaining why; the
// contract itself stays valid and printable.
func Derive(ws *workspace.Workspace, p *workspace.Project, platformPrefix string) (*qualifyproto.Contract, *Unsupported, error) {
	contract := &qualifyproto.Contract{
		ProtocolVersion: qualifyproto.ProtocolVersion,
		Project:         p.ID,
		DerivedFrom:     []qualifyproto.Source{},
		Requests:        []qualifyproto.Request{},
	}
	relative, data, err := readInventory(ws.Root, p.Path)
	if err != nil {
		return nil, nil, err
	}
	if data == nil {
		contract.Digest = qualifyproto.ContractDigest(contract.Requests)
		return contract, &Unsupported{
			Reason: ReasonNoRouteInventory,
			Remedy: fmt.Sprintf("%s has no route inventory at schema/http-routes.json or .gen/schema/http-routes.json; run `putnami build --projects %s` to produce it", p.ID, p.ID),
		}, nil
	}
	manifest, diags := httproutes.ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		return nil, nil, &InvalidInventoryError{Path: relative, Diagnostics: diag.Errors(diags)}
	}
	contract.DerivedFrom = append(contract.DerivedFrom, qualifyproto.Source{
		Kind:   qualifyproto.SourceHTTPRoutes,
		Path:   relative,
		Digest: manifest.Digest,
	})
	contract.Requests = eligibleRequests(manifest.Routes, platformPrefix)
	contract.Digest = qualifyproto.ContractDigest(contract.Requests)
	if len(contract.Requests) == 0 {
		return contract, &Unsupported{
			Reason: ReasonNoDerivableRequest,
			Source: relative,
			Remedy: fmt.Sprintf("%s declares no exact, public GET or HEAD route outside the platform paths, so no request is safe to derive", relative),
		}, nil
	}
	return contract, nil, nil
}

// AmbiguousPlatformPrefixError reports a route inventory that declares the
// platform readiness and version endpoints under more than one prefix. Probing
// either would be a guess, so qualification asks for --platform-prefix instead.
type AmbiguousPlatformPrefixError struct {
	// Path is the workspace-relative inventory path.
	Path string
	// Prefixes are the normalized candidates, sorted; "" is the root.
	Prefixes []string
}

func (e *AmbiguousPlatformPrefixError) Error() string {
	shown := make([]string, 0, len(e.Prefixes))
	for _, prefix := range e.Prefixes {
		if prefix == "" {
			prefix = "/"
		}
		shown = append(shown, prefix)
	}
	return fmt.Sprintf("route inventory %s declares GET %s and GET %s under several platform prefixes (%s); pass --platform-prefix to choose one",
		e.Path, protocolplatform.PathReadyz, protocolplatform.PathVersion, strings.Join(shown, ", "))
}

// ResolvePlatformPrefix returns the prefix p mounts its platform endpoints
// under. An explicit prefix, the --platform-prefix value, wins. Otherwise the
// route inventory Derive reads decides: the prefix is the one under which the
// inventory declares both GET <prefix>/readyz and GET <prefix>/version as exact
// routes, the two endpoints qualification calls. A workload that mounts its
// platform endpoints under "/_" already declares them there, so nobody has to
// repeat the prefix on the command line.
//
// No inventory, an invalid one, or no such pair is the root; Derive reports the
// first two. Pairs under two or more prefixes are an
// AmbiguousPlatformPrefixError.
func ResolvePlatformPrefix(ws *workspace.Workspace, p *workspace.Project, explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	relative, data, err := readInventory(ws.Root, p.Path)
	if err != nil || data == nil {
		return "", err
	}
	manifest, diags := httproutes.ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		return "", nil
	}
	prefixes := inventoryPlatformPrefixes(manifest.Routes)
	switch len(prefixes) {
	case 0:
		return "", nil
	case 1:
		return prefixes[0], nil
	}
	return "", &AmbiguousPlatformPrefixError{Path: relative, Prefixes: prefixes}
}

// inventoryPlatformPrefixes lists, sorted, every normalized prefix under which
// the routes declare both readiness and version as exact GET routes.
func inventoryPlatformPrefixes(routes []httproutes.Route) []string {
	gets := map[string]bool{}
	for _, route := range routes {
		if route.Match == httproutes.MatchExact && readOnlyMethod(route.Methods) == "GET" {
			gets[route.Path] = true
		}
	}
	prefixes := []string{}
	for routePath := range gets {
		prefix, ok := strings.CutSuffix(routePath, protocolplatform.PathReadyz)
		// The round trip refuses a path no normalized prefix mounts at, such
		// as "//readyz" or "/ops/readyz" declared as "/ops//readyz".
		if !ok || protocolplatform.JoinPrefix(prefix, protocolplatform.PathReadyz) != routePath {
			continue
		}
		if gets[protocolplatform.JoinPrefix(prefix, protocolplatform.PathVersion)] {
			prefixes = append(prefixes, protocolplatform.NormalizePrefix(prefix))
		}
	}
	sort.Strings(prefixes)
	return prefixes
}

// readInventory returns the first inventory that exists, as a workspace-relative
// slash path and its bytes. A missing inventory returns nil bytes and no error.
func readInventory(root, projectPath string) (string, []byte, error) {
	for _, location := range inventoryLocations {
		absolute := filepath.Join(root, filepath.FromSlash(projectPath), location)
		data, err := os.ReadFile(absolute)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("read route inventory: %w", err)
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			return "", nil, fmt.Errorf("locate route inventory: %w", err)
		}
		return filepath.ToSlash(relative), data, nil
	}
	return "", nil, nil
}

func eligibleRequests(routes []httproutes.Route, platformPrefix string) []qualifyproto.Request {
	excluded := excludedPaths(platformPrefix)
	byPath := map[string]qualifyproto.Request{}
	for _, route := range routes {
		if route.Match != httproutes.MatchExact || !route.PublicEdge || excluded[route.Path] || underReservedPrefix(route.Path, platformPrefix) {
			continue
		}
		method := readOnlyMethod(route.Methods)
		if method == "" {
			continue
		}
		// One request per path: an inventory may split GET and HEAD across two
		// exact routes, and GET proves more than HEAD.
		if existing, ok := byPath[route.Path]; ok && existing.Method == "GET" {
			continue
		}
		byPath[route.Path] = qualifyproto.Request{
			ID:         method + " " + route.Path,
			Method:     method,
			Path:       route.Path,
			MaxStatus:  qualifyproto.DefaultMaxStatus,
			Provenance: string(route.Provenance.SourceKind),
		}
	}
	paths := make([]string, 0, len(byPath))
	for routePath := range byPath {
		paths = append(paths, routePath)
	}
	sort.Strings(paths)
	if len(paths) > MaxRequests {
		paths = paths[:MaxRequests]
	}
	requests := make([]qualifyproto.Request, 0, len(paths))
	for _, routePath := range paths {
		requests = append(requests, byPath[routePath])
	}
	return requests
}

func readOnlyMethod(methods []string) string {
	head := false
	for _, method := range methods {
		switch method {
		case "GET":
			return "GET"
		case "HEAD":
			head = true
		}
	}
	if head {
		return "HEAD"
	}
	return ""
}

// excludedPaths is every platform canonical path, bare and under the prefix,
// each in its exact and trailing-slash form (an exact route matches both).
func excludedPaths(platformPrefix string) map[string]bool {
	excluded := map[string]bool{}
	for _, canonical := range protocolplatform.CanonicalPaths {
		for _, mounted := range []string{canonical, protocolplatform.JoinPrefix(platformPrefix, canonical)} {
			excluded[mounted] = true
			excluded[mounted+"/"] = true
		}
	}
	return excluded
}

// underReservedPrefix reports a path under /debug/pprof or /_putnami, bare or
// under the platform prefix.
func underReservedPrefix(routePath, platformPrefix string) bool {
	for _, reserved := range []string{protocolplatform.PathPprofPrefix, "/_putnami"} {
		for _, mounted := range []string{reserved, protocolplatform.JoinPrefix(platformPrefix, reserved)} {
			if routePath == mounted || strings.HasPrefix(routePath, mounted+"/") {
				return true
			}
		}
	}
	return false
}

// joinURLPath joins a base URL path, the platform prefix and a route path.
// Route paths and normalized prefixes always begin with '/', so trimming the
// base path's trailing slash is the only separator to reconcile.
func joinURLPath(basePath, platformPrefix, routePath string) string {
	return strings.TrimRight(basePath, "/") + protocolplatform.NormalizePrefix(platformPrefix) + routePath
}
