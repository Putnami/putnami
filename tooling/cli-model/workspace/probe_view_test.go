package workspace

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// THE IDENTITY CUTOVER.
//
// An earlier migration guard lived here: it compared each provider's answer to the
// core parser it was scheduled to replace. The parsers are gone, so the guard is
// gone with its subject — there is nothing left to disagree with. What replaced
// it is the property the guard existed to make safe: the merged provider view IS
// the project's identity, and applying it must be idempotent and
// path-independent, because a project's cache key is computed from it.
//
// The tests below pin, in order: the merge precedence, dependency resolution
// from PATHS to NAMES, provider metadata carriage, and the equality that makes
// the cache key move safe — one tree resolves to one identity whether this run
// probed or replayed a recorded answer.

func mergedView(projects ...wsproto.MergedProject) map[string]wsproto.MergedProject {
	view := make(map[string]wsproto.MergedProject, len(projects))
	for _, project := range projects {
		view[project.Path] = project
	}
	return view
}

// adoptionWorkspace is a workspace whose projects declare only what an author
// would: `app` has no putnami.json identity at all, `lib` names itself, and
// `infra` is a Putnami-only project no provider will ever claim.
func adoptionWorkspace() *Workspace {
	app := &Project{ID: "/app", Path: "app"}
	lib := &Project{ID: "/lib", Path: "lib", Config: &wsproto.ProjectConfig{Name: "@acme/lib"}}
	infra := &Project{ID: "/infra", Path: "infra", Config: &wsproto.ProjectConfig{Name: "@acme/infra"}}
	return NewWorkspace("/ws", &wsproto.Config{Name: "acme"}, []*Project{app, lib, infra})
}

// Precedence, in force order: explicit putnami.json > provider source identity >
// scope namePattern > directory basename. SourceName records the identity BEFORE
// the namePattern override, which is what makes an unaligned manifest reportable.
func TestAdoptProbeView_AppliesTheMergePrecedence(t *testing.T) {
	ws := adoptionWorkspace()
	ws.Projects[0].Scope = ScopeContribution{Name: "@acme/app-by-convention"}

	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app", Type: "application",
			Tags: []string{"go"}},
		// The merge has already resolved lib's explicit name; a provider that
		// still reports its own manifest identity must not win over it.
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib", Type: "library"},
	))

	app := ws.ProjectByID("/app")
	if app.Name != "example.com/app" || app.SourceName != "example.com/app" {
		t.Errorf("app name/sourceName = %q/%q, want the provider's identity to outrank the scope pattern",
			app.Name, app.SourceName)
	}
	if app.Type != "application" || !slices.Equal(app.Tags, []string{"go"}) {
		t.Errorf("app type/tags = %q/%v, want them adopted from the view", app.Type, app.Tags)
	}
	if app.Version != "" {
		t.Errorf("app version = %q, want none: a version is derived from the line's git tags, never inherited", app.Version)
	}
	if lib := ws.ProjectByID("/lib"); lib.Name != "@acme/lib" {
		t.Errorf("lib name = %q, want the explicit putnami.json identity", lib.Name)
	}
	// A project no provider claimed keeps exactly what the author declared.
	if infra := ws.ProjectByID("/infra"); infra.Name != "@acme/infra" || infra.Version != "" {
		t.Errorf("infra = %+v, want the authored identity untouched", infra)
	}
}

// Classification has the same precedence as every other scalar, and it is the
// one that now MATTERS: the Go probe derives Type
// from the module's own sources, so a workload whose entrypoint the derivation
// cannot see (a main built out of tree, a module that exists only to be
// deployed) needs an authored `type` to stay an application — and a project that
// authors nothing must get the provider's answer rather than the "application"
// default that made every Go library plan serve, run, and a platform compile.
func TestAdoptProbeView_AuthoredTypeOutranksTheProviderClassification(t *testing.T) {
	ws := adoptionWorkspace()
	ws.Projects[1].Config.Type = "application"

	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app", Type: "library"},
		wsproto.MergedProject{Path: "lib", SourceName: "example.com/lib", Type: "library"},
	))

	if got := ws.ProjectByID("/app").Type; got != "library" {
		t.Errorf("unauthored app type = %q, want the provider's classification %q", got, "library")
	}
	if got := ws.ProjectByID("/lib").Type; got != "application" {
		t.Errorf("authored lib type = %q, want the putnami.json value to outrank the provider", got)
	}
	// A project no provider claimed is still unclassified, which core reads as
	// "application" — the classification is a provider's answer, not a default
	// core invents.
	if got := ws.ProjectByID("/infra").Type; got != "" {
		t.Errorf("unclaimed infra type = %q, want it left empty", got)
	}
}

// A project with neither an authored name nor a provider claim falls back to its
// directory basename, and the scope namePattern outranks that fallback.
func TestAdoptProbeView_FallsBackToTheScopePatternThenTheBasename(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(nil)
	if got := ws.ProjectByID("/app").Name; got != "app" {
		t.Errorf("unclaimed app name = %q, want the directory basename", got)
	}

	ws.Projects[0].Scope = ScopeContribution{Name: "@acme/app"}
	ws.AdoptProbeView(nil)
	app := ws.ProjectByID("/app")
	if app.Name != "@acme/app" || app.SourceName != "app" {
		t.Errorf("app name/sourceName = %q/%q, want the pattern applied over the basename",
			app.Name, app.SourceName)
	}
}

// Providers report edges as repo-relative PATHS; the dependency graph is keyed on
// NAMES. Resolution happens after every name is final, so an edge's spelling
// cannot depend on discovery order.
func TestAdoptProbeView_ResolvesDependencyPathsToNames(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app",
			// "lib" is a path; "@acme/infra" is an authored dependency the merge
			// folded back in verbatim and resolves to no path; "app" is a
			// self-edge the graph must never carry.
			Dependencies: []string{"lib", "@acme/infra", "app"}},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"},
	))

	app := ws.ProjectByID("/app")
	if !slices.Equal(app.Dependencies, []string{"@acme/infra", "@acme/lib"}) {
		t.Fatalf("app dependencies = %v, want the path resolved to a name and the declared name kept",
			app.Dependencies)
	}
	// The graph is rebuilt from the adopted edges, or `--impacted` would select
	// from a graph that no longer exists.
	if !slices.Contains(ws.Graph.DependentsOf("/lib"), app.ID) {
		t.Errorf("dependents of @acme/lib = %v, want the adopted app edge",
			ws.Graph.DependentsOf("/lib"))
	}
}

// Provider metadata is carried verbatim and never interpreted for identity. It
// reaches the job context and the project's metadata digest; nothing else.
func TestAdoptProbeView_CarriesProviderMetadata(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(mergedView(wsproto.MergedProject{
		Path: "app", SourceName: "example.com/app",
		Metadata: map[string]json.RawMessage{
			"@putnami/typescript": json.RawMessage(`{"main":"dist/index.js"}`),
		},
	}))

	app := ws.ProjectByID("/app")
	if string(app.Metadata["@putnami/typescript"]) != `{"main":"dist/index.js"}` {
		t.Fatalf("metadata = %v, want the block carried verbatim", app.Metadata)
	}
	if got := ws.ProviderMetadataValue(app, "main"); got != "dist/index.js" {
		t.Errorf("ProviderMetadataValue(main) = %q, want dist/index.js", got)
	}
	if got := ws.ProviderMetadataValue(app, "absent"); got != "" {
		t.Errorf("ProviderMetadataValue(absent) = %q, want empty", got)
	}
}

// IDEMPOTENCE. Applying one view twice must produce one project — a second
// application that unioned into what the first wrote would grow tags and edges
// on every re-probe, and a watch session re-probes on every metadata edit.
func TestAdoptProbeView_IsIdempotent(t *testing.T) {
	ws := adoptionWorkspace()
	view := mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app",
			Tags: []string{"go"}, Dependencies: []string{"lib"}},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"},
	)

	ws.AdoptProbeView(view)
	first := ws.MetadataDigestFor(ws.ProjectByID("/app"))
	tags := slices.Clone(ws.ProjectByID("/app").Tags)
	deps := slices.Clone(ws.ProjectByID("/app").Dependencies)

	ws.AdoptProbeView(view)
	app := ws.ProjectByID("/app")
	if !slices.Equal(app.Tags, tags) || !slices.Equal(app.Dependencies, deps) {
		t.Fatalf("second adoption changed the project: tags %v→%v deps %v→%v",
			tags, app.Tags, deps, app.Dependencies)
	}
	if got := ws.MetadataDigestFor(app); got != first {
		t.Errorf("metadata digest moved on re-adoption: %q → %q", first, got)
	}
}

// REPLACEMENT, NOT ACCUMULATION. Adopting a DIFFERENT view must produce the
// project that view describes. A blend of the two would make a project's
// identity depend on how many times a session happened to probe.
func TestAdoptProbeView_ReplacesTheProviderContribution(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app",
			Tags: []string{"go"}, Dependencies: []string{"lib"}},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"},
	))
	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/renamed", Tags: []string{"tools"}},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"},
	))

	app := ws.ProjectByID("/app")
	if app.Name != "example.com/renamed" {
		t.Errorf("app name = %q, want the new view's identity", app.Name)
	}
	if !slices.Equal(app.Tags, []string{"tools"}) {
		t.Errorf("app tags = %v, want only the new view's tags", app.Tags)
	}
	if len(app.Dependencies) != 0 {
		t.Errorf("app dependencies = %v, want the dropped edge gone", app.Dependencies)
	}
	if ws.ProjectByName("example.com/app") != nil {
		t.Error("the name index still answers to the previous name")
	}
}

// The metadata digest is what moves cache keys, so it must observe every part of
// the adopted view — including the provider metadata block that replaced job
// context v2's npm-shaped members.
func TestMetadataDigest_MovesWithTheAdoptedView(t *testing.T) {
	digestOf := func(view map[string]wsproto.MergedProject) string {
		ws := adoptionWorkspace()
		ws.AdoptProbeView(view)
		return ws.MetadataDigestFor(ws.ProjectByID("/app"))
	}

	base := wsproto.MergedProject{Path: "app", SourceName: "example.com/app"}
	baseline := digestOf(mergedView(base))

	renamed := base
	renamed.SourceName = "example.com/other"
	if digestOf(mergedView(renamed)) == baseline {
		t.Error("source identity does not key the digest")
	}

	withEdge := base
	withEdge.Dependencies = []string{"lib"}
	if digestOf(mergedView(withEdge, wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"})) == baseline {
		t.Error("a dependency edge does not key the digest")
	}

	withMetadata := base
	withMetadata.Metadata = map[string]json.RawMessage{"@putnami/typescript": json.RawMessage(`{"bin":"cli.js"}`)}
	withMetadataDigest := digestOf(mergedView(withMetadata))
	if withMetadataDigest == baseline {
		t.Error("provider metadata does not key the digest; a task that reads project.metadata would serve a stale hit")
	}

	// A provider re-ordering its own JSON object keys means nothing changed.
	reordered := base
	reordered.Metadata = map[string]json.RawMessage{
		"@putnami/typescript": json.RawMessage("{\n  \"bin\" : \"cli.js\"\n}"),
	}
	if digestOf(mergedView(reordered)) != withMetadataDigest {
		t.Error("metadata is not canonicalized; a provider's own formatting moves every cache key")
	}
}

// A resolved-name collision produced by a probe is reported, not fatal: adoption
// happens after the workspace is loaded, and a recovery command must still run on
// a workspace whose providers disagree.
func TestAdoptProbeView_ReportsDuplicateResolvedNames(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "@acme/lib"},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"},
	))

	found := false
	for _, warning := range ws.Warnings {
		if strings.Contains(warning, "duplicate project name") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want a duplicate-name warning", ws.Warnings)
	}
}

// The divergence report is what `projects sync` prints and each provider's sync
// task acts on: the manifest on disk spells the project differently from the
// name the workspace resolved for it. Findings are sorted so two runs over one
// tree print the same lines in the same order.
func TestNameDivergences_ReportsUnalignedManifestsAndSorts(t *testing.T) {
	ws := adoptionWorkspace()
	ws.Projects[0].Scope = ScopeContribution{Name: "@acme/app"}
	ws.Projects[1].Scope = ScopeContribution{Name: "@acme/renamed-lib"}
	ws.AdoptProbeView(nil)

	findings := NameDivergences(ws)
	if len(findings) != 1 || !strings.Contains(findings[0], "app") {
		t.Fatalf("findings = %v, want only the convention-named app", findings)
	}
	if !slices.IsSorted(findings) {
		t.Errorf("findings are not sorted: %v", findings)
	}
	// lib declares its own name, so the scope pattern never applies to it and
	// there is nothing to align.
	if got := ws.ProjectByID("/lib").Name; got != "@acme/lib" {
		t.Errorf("lib name = %q, want the declared identity to defer nothing to the pattern", got)
	}
}

// REGRESSION: the shape that broke a whole workspace — a project whose
// putnami.json declares a PATH-SHAPED name while its native manifest carries the
// published package name.
//
// The resolved name comes from putnami.json, so SourceName equals it and there
// is NOTHING to align: the package.json name is not core's business, and the
// provider's reported source identity is only ever a fallback for a project that
// declared none. Core got this right (it reported "0 names to align"); the
// provider's sync task did not, because it only received the resolved name and
// rewrote every manifest that disagreed with it. This pins the value the wire
// now carries, which is what lets the task reach the same conclusion.
func TestNameDivergences_IgnoresAPathShapedDeclaredName(t *testing.T) {
	core := &Project{ID: "/surfaces/libs/console-core", Path: "surfaces/libs/console-core",
		Config: &wsproto.ProjectConfig{Name: "surfaces/libs/console-core"}}
	ws := NewWorkspace("/ws", &wsproto.Config{Name: "acme"}, []*Project{core})
	// The provider truthfully reports the npm identity it read from
	// package.json; the explicit putnami.json name outranks it.
	ws.AdoptProbeView(mergedView(wsproto.MergedProject{
		Path: "surfaces/libs/console-core", SourceName: "@putnami/console-core"}))

	resolved := ws.ProjectByID("/surfaces/libs/console-core")
	if resolved.Name != "surfaces/libs/console-core" {
		t.Fatalf("name = %q, want the explicit putnami.json identity", resolved.Name)
	}
	if resolved.SourceName != resolved.Name {
		t.Fatalf("sourceName = %q, want it equal to the resolved name %q: the identity was DECLARED, so a "+
			"sync task must not rewrite package.json and orphan every workspace:* reference to it",
			resolved.SourceName, resolved.Name)
	}
	if findings := NameDivergences(ws); len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

// HasProbeView is what the callers whose output is a lie without one ask, so it
// must answer about the ADOPTED view and not about whether a map was passed.
func TestHasProbeView_TracksTheAdoptedView(t *testing.T) {
	ws := adoptionWorkspace()
	if ws.HasProbeView() {
		t.Error("a freshly built workspace reports an adopted view")
	}
	ws.AdoptProbeView(mergedView(wsproto.MergedProject{Path: "app", SourceName: "example.com/app"}))
	if !ws.HasProbeView() {
		t.Error("an adopted view is not reported")
	}
	ws.AdoptProbeView(nil)
	if ws.HasProbeView() {
		t.Error("a dropped view is still reported")
	}
}

// AdoptProbeView is reached after the workspace is already loaded, so it may not
// fail — but it may not adopt a view Load refuses either, or a live workspace
// ends up in the exact state its own loader rejects.
func TestAdoptProbeView_KeepsThePreviousViewWhenTheNewOneIsUnloadable(t *testing.T) {
	ws := adoptionWorkspace()
	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app"},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib"}))

	ws.AdoptProbeView(mergedView(
		wsproto.MergedProject{Path: "app", SourceName: "example.com/app", Dependencies: []string{"lib"}},
		wsproto.MergedProject{Path: "lib", SourceName: "@acme/lib", Dependencies: []string{"app"}}))

	if cycle := ws.Graph.FindCycle(); cycle != nil {
		t.Fatalf("a cyclic view was adopted onto a live workspace: %v", cycle)
	}
	if got := ws.ProjectByID("/app"); got == nil || got.Name != "example.com/app" {
		t.Errorf("app = %+v, want the previous view's identity kept", got)
	}
	if warning := strings.Join(ws.Warnings, "\n"); !strings.Contains(warning, "dependency cycle") {
		t.Errorf("the refusal was silent:\n%s", warning)
	}
}
