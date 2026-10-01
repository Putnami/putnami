package qualify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	qualifyproto "go.putnami.dev/protocol/qualify"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// route is shorthand for one inventory route in a test.
func route(match httproutes.MatchKind, path string, public bool, methods ...string) httproutes.Route {
	return httproutes.Route{
		Match:      match,
		Path:       path,
		Methods:    methods,
		PublicEdge: public,
		Provenance: httproutes.Provenance{Project: "example", SourceKind: httproutes.SourceTypedAPI},
	}
}

// fixtureProject writes a canonical route inventory under dir/app at location
// ("schema" or ".gen/schema") and returns the workspace and project.
func fixtureProject(t *testing.T, location string, routes []httproutes.Route) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	root := t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "example", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	if routes == nil {
		return ws, project
	}
	manifest, diags := httproutes.Canonicalize(routes)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize fixture routes: %v", diags)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "app", filepath.FromSlash(location))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "http-routes.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, project
}

func requestIDs(contract *qualifyproto.Contract) []string {
	ids := make([]string, 0, len(contract.Requests))
	for _, request := range contract.Requests {
		ids = append(ids, request.ID)
	}
	return ids
}

// validContract round-trips a derived contract through the strict protocol
// parser: the CLI is the producer, so what it emits must be what the protocol
// accepts.
func validContract(t *testing.T, contract *qualifyproto.Contract) {
	t.Helper()
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, diags := qualifyproto.ParseAndValidateContract(data); diag.HasErrors(diags) {
		t.Fatalf("derived contract fails the protocol: %v\n%s", diags, data)
	}
}

func TestDerive_SelectsExactPublicGetRoutesSortedAndCapped(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "smoke-is-derived-from-the-route-inventory",
		"exact-public-read-only-routes-sorted-and-capped")
	routes := []httproutes.Route{
		route(httproutes.MatchExact, "/zeta", true, "GET", "POST"),
		route(httproutes.MatchExact, "/alpha", true, "HEAD"),
		route(httproutes.MatchExact, "/beta", true, "POST"),            // mutation only
		route(httproutes.MatchExact, "/internal", false, "GET"),        // not public
		route(httproutes.MatchTemplate, "/items/{id}", true, "GET"),    // not exact
		route(httproutes.MatchPrefix, "/assets/", true, "GET", "HEAD"), // not exact
		route(httproutes.MatchExact, "/gamma", true, "GET", "HEAD"),    // GET preferred
		route(httproutes.MatchExact, "/with%20space", true, "GET"),     // escaped path kept verbatim
	}
	ws, project := fixtureProject(t, "schema", routes)
	contract, unsupported, err := Derive(ws, project, "")
	if err != nil || unsupported != nil {
		t.Fatalf("Derive: %v, unsupported %+v", err, unsupported)
	}
	want := []string{"HEAD /alpha", "GET /gamma", "GET /with%20space", "GET /zeta"}
	if got := requestIDs(contract); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for _, request := range contract.Requests {
		if request.MaxStatus != 499 || request.Provenance != "typed-api" {
			t.Errorf("%s: maxStatus %d provenance %q, want 499 typed-api", request.ID, request.MaxStatus, request.Provenance)
		}
	}
	if len(contract.DerivedFrom) != 1 || contract.DerivedFrom[0].Path != "app/schema/http-routes.json" ||
		!strings.HasPrefix(contract.DerivedFrom[0].Digest, "sha256:") {
		t.Errorf("derivedFrom = %+v", contract.DerivedFrom)
	}
	if contract.Project != "/app" || contract.Digest != qualifyproto.ContractDigest(contract.Requests) {
		t.Errorf("project %q digest %q", contract.Project, contract.Digest)
	}
	validContract(t, contract)

	// Capped at MaxRequests, keeping the first paths in sorted order.
	var many []httproutes.Route
	for i := 0; i < MaxRequests+5; i++ {
		many = append(many, route(httproutes.MatchExact, fmt.Sprintf("/r%02d", i), true, "GET"))
	}
	ws, project = fixtureProject(t, ".gen/schema", many)
	capped, _, err := Derive(ws, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Requests) != MaxRequests || capped.Requests[0].Path != "/r00" || capped.Requests[MaxRequests-1].Path != "/r24" {
		t.Errorf("capped contract = %v", requestIDs(capped))
	}
	if capped.DerivedFrom[0].Path != "app/.gen/schema/http-routes.json" {
		t.Errorf("the generated inventory must be read when no committed one exists: %+v", capped.DerivedFrom)
	}
	validContract(t, capped)

	// Two exact routes on one path split GET and HEAD: one request, GET.
	split := []httproutes.Route{
		route(httproutes.MatchExact, "/split", true, "HEAD"),
		route(httproutes.MatchExact, "/split", true, "GET"),
	}
	if got := eligibleRequests(split, ""); len(got) != 1 || got[0].ID != "GET /split" {
		t.Errorf("split methods = %+v, want one GET request", got)
	}
	if got := eligibleRequests([]httproutes.Route{split[1], split[0]}, ""); len(got) != 1 || got[0].ID != "GET /split" {
		t.Errorf("split methods (reversed) = %+v, want one GET request", got)
	}
}

func TestDerive_ExcludesPlatformAndPprofPaths(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "smoke-is-derived-from-the-route-inventory",
		"platform-and-reserved-paths-are-never-requested")
	routes := []httproutes.Route{
		route(httproutes.MatchExact, "/healthz", true, "GET"),
		route(httproutes.MatchExact, "/livez", true, "GET"),
		route(httproutes.MatchExact, "/readyz", true, "GET"),
		route(httproutes.MatchExact, "/version", true, "GET"),
		route(httproutes.MatchExact, "/debug/pprof", true, "GET"),
		route(httproutes.MatchExact, "/debug/pprof/heap", true, "GET"),
		route(httproutes.MatchExact, "/_putnami/state", true, "GET"),
		route(httproutes.MatchExact, "/ops/readyz", true, "GET"),
		route(httproutes.MatchExact, "/ops/debug/pprof/goroutine", true, "GET"),
		route(httproutes.MatchExact, "/debugger", true, "GET"),
		route(httproutes.MatchExact, "/items", true, "GET"),
	}
	ws, project := fixtureProject(t, "schema", routes)
	contract, _, err := Derive(ws, project, "/ops/")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requestIDs(contract), ","); got != "GET /debugger,GET /items" {
		t.Fatalf("requests = %s, want only /debugger and /items", got)
	}
	// Without the prefix, a route at /ops/readyz is an ordinary business route.
	unprefixed, _, err := Derive(ws, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requestIDs(unprefixed), ","); !strings.Contains(got, "GET /ops/readyz") {
		t.Errorf("without a prefix, /ops/readyz must be derivable: %s", got)
	}
}

func TestDerive_NoInventoryIsUnsupportedWithRemedy(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "no-derivable-request-is-unsupported-not-success",
		"a-missing-inventory-is-unsupported-with-a-build-remedy")
	ws, project := fixtureProject(t, "schema", nil)
	contract, unsupported, err := Derive(ws, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if unsupported == nil || unsupported.Reason != ReasonNoRouteInventory {
		t.Fatalf("unsupported = %+v, want %s", unsupported, ReasonNoRouteInventory)
	}
	if !strings.Contains(unsupported.Remedy, "putnami build --projects /app") {
		t.Errorf("remedy %q must name putnami build for the project", unsupported.Remedy)
	}
	if d := unsupported.Diagnostic(); d.Code != qualifyproto.PhaseCodeNoRouteInventory || d.Severity != diag.Error {
		t.Errorf("diagnostic = %+v", d)
	}
	if len(contract.Requests) != 0 || len(contract.DerivedFrom) != 0 {
		t.Errorf("contract = %+v, want empty", contract)
	}
	validContract(t, contract)

	// An inventory with nothing safe to request is unsupported for another reason.
	ws, project = fixtureProject(t, "schema", []httproutes.Route{route(httproutes.MatchExact, "/items", true, "POST")})
	contract, unsupported, err = Derive(ws, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if unsupported == nil || unsupported.Reason != ReasonNoDerivableRequest || unsupported.Source != "app/schema/http-routes.json" {
		t.Fatalf("unsupported = %+v, want %s naming the inventory", unsupported, ReasonNoDerivableRequest)
	}
	if d := unsupported.Diagnostic(); d.Code != qualifyproto.PhaseCodeNoDerivableRequest {
		t.Errorf("diagnostic code = %s", d.Code)
	}
	validContract(t, contract)
}

func TestDerive_RefusesAnInvalidInventory(t *testing.T) {
	ws, project := fixtureProject(t, "schema", nil)
	dir := filepath.Join(ws.Root, "app", "schema")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "http-routes.json"), []byte(`{"protocol":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Derive(ws, project, "")
	var invalid *InvalidInventoryError
	if !errors.As(err, &invalid) || invalid.Path != "app/schema/http-routes.json" || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v, want an InvalidInventoryError naming the file", err)
	}

	// An unreadable inventory (a directory where the file should be) is an error, not "missing".
	if err := os.Remove(filepath.Join(dir, "http-routes.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "http-routes.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Derive(ws, project, ""); err == nil || errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a read error", err)
	}
}

func TestJoinURLPath(t *testing.T) {
	for _, tc := range []struct{ base, prefix, route, want string }{
		{"", "", "/items", "/items"},
		{"/", "", "/items", "/items"},
		{"/app/", "/ops/", "/readyz", "/app/ops/readyz"},
		{"/app", "ops", "/a%20b", "/app/ops/a%20b"},
	} {
		if got := joinURLPath(tc.base, tc.prefix, tc.route); got != tc.want {
			t.Errorf("joinURLPath(%q, %q, %q) = %q, want %q", tc.base, tc.prefix, tc.route, got, tc.want)
		}
	}
}

// A workload that mounts its platform endpoints under a prefix declares them
// there in the route inventory Derive already reads, so qualification finds
// /readyz and /version without being told. The flag still wins, and an
// inventory that declares the pair under two prefixes is refused rather than
// guessed.
func TestResolvePlatformPrefix_ReadsTheInventory(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "smoke-is-derived-from-the-route-inventory",
		"the-platform-prefix-is-read-from-the-inventory")
	platformUnder := func(prefix string) []httproutes.Route {
		return []httproutes.Route{
			route(httproutes.MatchExact, prefix+"/livez", false, "GET"),
			route(httproutes.MatchExact, prefix+"/readyz", false, "GET"),
			route(httproutes.MatchExact, prefix+"/version", false, "GET"),
		}
	}
	app := route(httproutes.MatchExact, "/items", true, "GET")

	for name, tc := range map[string]struct {
		routes   []httproutes.Route
		explicit string
		want     string
	}{
		"prefixed pair":             {routes: append(platformUnder("/_"), app), want: "/_"},
		"root pair":                 {routes: append(platformUnder(""), app), want: ""},
		"explicit flag wins":        {routes: append(platformUnder("/_"), app), explicit: "/ops", want: "/ops"},
		"explicit root wins":        {routes: append(platformUnder("/_"), app), explicit: "/", want: "/"},
		"readiness without version": {routes: []httproutes.Route{route(httproutes.MatchExact, "/_/readyz", false, "GET"), app}, want: ""},
		"head only is not a mount":  {routes: []httproutes.Route{route(httproutes.MatchExact, "/_/readyz", false, "HEAD"), route(httproutes.MatchExact, "/_/version", false, "HEAD"), app}, want: ""},
		"no platform route":         {routes: []httproutes.Route{app}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			ws, project := fixtureProject(t, ".gen/schema", tc.routes)
			got, err := ResolvePlatformPrefix(ws, project, tc.explicit)
			if err != nil || got != tc.want {
				t.Fatalf("ResolvePlatformPrefix = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	// No inventory: the root, and Derive reports the missing inventory.
	ws, project := fixtureProject(t, "schema", nil)
	if got, err := ResolvePlatformPrefix(ws, project, ""); err != nil || got != "" {
		t.Fatalf("no inventory = %q, %v; want the root", got, err)
	}

	// The pair under two prefixes is a guess qualification refuses to make.
	ws, project = fixtureProject(t, "schema", append(append(platformUnder(""), platformUnder("/_")...), app))
	_, err := ResolvePlatformPrefix(ws, project, "")
	var ambiguous *AmbiguousPlatformPrefixError
	if !errors.As(err, &ambiguous) || strings.Join(ambiguous.Prefixes, ",") != ",/_" {
		t.Fatalf("error = %v, want an AmbiguousPlatformPrefixError naming the root and /_", err)
	}
	if !strings.Contains(err.Error(), "(/, /_)") || !strings.Contains(err.Error(), "--platform-prefix") {
		t.Fatalf("error %q must name both prefixes and the flag", err)
	}
	if got, err := ResolvePlatformPrefix(ws, project, "/_"); err != nil || got != "/_" {
		t.Fatalf("the flag must resolve an ambiguous inventory: %q, %v", got, err)
	}
}
