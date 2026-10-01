package workspace

import (
	"encoding/json"
	"errors"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// --- identity digest --------------------------------------------------------

func identityWorkspace() *Workspace {
	lib := &Project{ID: "/lib", Name: "lib", Path: "lib", SourceName: "example.com/lib",
		Type: "library", Tags: []string{"go"}}
	app := &Project{ID: "/app", Name: "app", Path: "app", SourceName: "example.com/app",
		Type: "application", Dependencies: []string{"lib"}}
	return NewWorkspace("/ws", nil, []*Project{app, lib})
}

func TestProbeViewOf_KeysDependenciesOnPaths(t *testing.T) {
	ws := identityWorkspace()
	view := ProbeViewOf(ws.ProjectByID("/app"), ws.DependencyPathResolver())

	if view.Path != "app" {
		t.Errorf("path = %q", view.Path)
	}
	if len(view.Dependencies) != 1 || view.Dependencies[0] != "lib" {
		t.Errorf("dependencies = %v, want the depended-on project's PATH", view.Dependencies)
	}
	if view.SourceName != "example.com/app" {
		t.Errorf("sourceName = %q", view.SourceName)
	}
	// The authored version is deliberately absent: it keys the cache through
	// EmbeddedVersion, and only for version-bearing tasks.
	if view.Version != "" {
		t.Errorf("version = %q, want empty", view.Version)
	}
	if ProbeViewOf(nil, nil).Path != "" {
		t.Error("a nil project must project to the zero view")
	}
}

func TestProbeViewOf_WorkspaceRootUsesProtocolSpelling(t *testing.T) {
	root := &Project{ID: "/", Name: "root", Path: ""}
	ws := NewWorkspace("/ws", nil, []*Project{root})
	if got := ProbeViewOf(root, ws.DependencyPathResolver()).Path; got != wsproto.ProbeRootPath {
		t.Errorf("root path = %q, want %q", got, wsproto.ProbeRootPath)
	}
}

func TestProbeViewOf_UnresolvableDependencyIsNotAPath(t *testing.T) {
	ws := identityWorkspace()
	app := ws.ProjectByID("/app")
	app.Dependencies = []string{"lib", "some-external-package"}

	view := ProbeViewOf(app, ws.DependencyPathResolver())
	if len(view.Dependencies) != 1 || view.Dependencies[0] != "lib" {
		t.Fatalf("dependencies = %v — an unresolvable name must not be recorded as a path", view.Dependencies)
	}
	// ...but it must still key: the raw names are folded in beside the view.
	withExternal := ProjectMetadataDigest(view, app.Dependencies, nil, nil)
	withoutExternal := ProjectMetadataDigest(view, []string{"lib"}, nil, nil)
	if withExternal == withoutExternal {
		t.Error("an unresolvable dependency vanished from the metadata digest")
	}
}

// The digest must move for every metadata member that can change what a task
// does or what its output contains, and must not move for anything else.
func TestMetadataDigestFor_MovesWithMetadataAndEdges(t *testing.T) {
	mutations := map[string]func(*Project){
		"type":       func(p *Project) { p.Type = "library" },
		"tags":       func(p *Project) { p.Tags = []string{"e2e"} },
		"publish":    func(p *Project) { p.Publish = []string{"npm"} },
		"runsWith":   func(p *Project) { p.RunsWith = []string{"postgres"} },
		"extensions": func(p *Project) { p.Extensions = []string{"/go/extension"} },
		"sourceName": func(p *Project) { p.SourceName = "example.com/renamed" },
		"dependency": func(p *Project) { p.Dependencies = nil },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			base := identityWorkspace()
			baseDigest := base.MetadataDigestFor(base.ProjectByID("/app"))

			mutated := identityWorkspace()
			mutate(mutated.ProjectByID("/app"))
			if got := mutated.MetadataDigestFor(mutated.ProjectByID("/app")); got == baseDigest {
				t.Errorf("moving %s did not move the metadata digest", name)
			}
		})
	}
}

func TestMetadataDigestFor_IgnoresVersionAndListOrder(t *testing.T) {
	base := identityWorkspace()
	baseDigest := base.MetadataDigestFor(base.ProjectByID("/app"))

	// The authored version must not move a task's key.
	versioned := identityWorkspace()
	versioned.ProjectByID("/app").Version = "9.9.9"
	if versioned.MetadataDigestFor(versioned.ProjectByID("/app")) != baseDigest {
		t.Error("the authored version moved the per-project cache-key digest; that is EmbeddedVersion's job")
	}

	// Reordering equivalent metadata must not move it either — discovery order
	// is not a fact about the project. Each variant gets its own workspace so
	// the per-workspace memo cannot serve one variant's digest for another's.
	digestWithTags := func(tags []string) string {
		ws := identityWorkspace()
		ws.ProjectByID("/app").Tags = tags
		return ws.MetadataDigestFor(ws.ProjectByID("/app"))
	}
	if digestWithTags([]string{"b", "a"}) != digestWithTags([]string{"a", "b"}) {
		t.Error("tag ordering moved the metadata digest")
	}
	if digestWithTags([]string{"a", "a", "b"}) != digestWithTags([]string{"a", "b"}) {
		t.Error("a duplicated tag moved the metadata digest")
	}
}

func TestMetadataDigestFor_MemoizedAndNilSafe(t *testing.T) {
	ws := identityWorkspace()
	app := ws.ProjectByID("/app")
	first := ws.MetadataDigestFor(app)
	if first == "" {
		t.Fatal("empty metadata digest")
	}
	if ws.MetadataDigestFor(app) != first {
		t.Error("metadata digest is not stable across calls")
	}
	if ws.MetadataDigestFor(nil) != "" {
		t.Error("a nil project must digest to the empty string")
	}
}

func TestWorkspaceIdentityDigest_FoldsProbeAndName(t *testing.T) {
	ws := identityWorkspace()
	identity := ws.IdentityDigest()
	if identity == "" || identity == ws.ProbeDigest() {
		t.Fatalf("identity = %q, probe = %q", identity, ws.ProbeDigest())
	}
	if ws.IdentityDigest() != identity {
		t.Error("identity digest is not stable across calls")
	}

	renamed := identityWorkspace()
	renamed.Name = "other"
	if renamed.IdentityDigest() == identity {
		t.Error("the workspace name did not move workspace identity")
	}

	changed := identityWorkspace()
	changed.ProjectByID("/lib").Tags = []string{"go", "protocol"}
	if changed.IdentityDigest() == identity {
		t.Error("a project's metadata did not move workspace identity")
	}
}

func TestWorkspaceProbeDigest_IndependentOfProjectOrder(t *testing.T) {
	lib := &Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}

	forward := NewWorkspace("/ws", nil, []*Project{app, lib}).ProbeDigest()
	reverse := NewWorkspace("/ws", nil, []*Project{lib, app}).ProbeDigest()
	if forward != reverse {
		t.Fatalf("probe digest depends on discovery order: %s vs %s", forward, reverse)
	}
}

// Two checkouts of the same commit at different absolute paths must agree.
// Nothing derived from the workspace root may enter the digest.
func TestWorkspaceProbeDigest_IndependentOfCheckoutLocation(t *testing.T) {
	build := func(root string) string {
		lib := &Project{ID: "/lib", Name: "lib", Path: "lib"}
		app := &Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
		return NewWorkspace(root, nil, []*Project{app, lib}).ProbeDigest()
	}
	if build("/home/a/repo") != build("/var/tmp/other/checkout") {
		t.Fatal("the probe digest depends on the checkout location")
	}
}

// --- probe runner -----------------------------------------------------------

type stubProvider struct {
	name   string
	result wsproto.ProbeResult
	err    error
	seen   *wsproto.ProbeRequest
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Probe(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	if p.seen != nil {
		*p.seen = request
	}
	return p.result, p.err
}

func TestRunProbe_MergesProvidersOrderIndependently(t *testing.T) {
	goProvider := &stubProvider{name: "@putnami/go", result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
		Projects: []wsproto.ProbeProject{{Path: "svc", SourceName: "example.com/svc",
			Metadata: json.RawMessage(`{"module":"example.com/svc"}`)}},
	}}
	tsProvider := &stubProvider{name: "@putnami/typescript", result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
		Projects: []wsproto.ProbeProject{{Path: "svc", Tags: []string{"ts"},
			Metadata: json.RawMessage(`{"bundler":"bun"}`)}},
	}}

	forward, err := RunProbe([]ProbeProvider{goProvider, tsProvider}, wsproto.ProbeRequest{}, nil)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	reverse, err := RunProbe([]ProbeProvider{tsProvider, goProvider}, wsproto.ProbeRequest{}, nil)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if forward.Digest != reverse.Digest {
		t.Errorf("aggregate digest depends on provider order: %s vs %s", forward.Digest, reverse.Digest)
	}
	view := forward.Merged["svc"]
	if view.SourceName != "example.com/svc" || len(view.Tags) != 1 {
		t.Errorf("merged view = %+v", view)
	}
	if len(view.Metadata) != 2 {
		t.Errorf("provider metadata was not bucketed per extension: %v", view.Metadata)
	}
}

func TestRunProbe_StampsTheRequest(t *testing.T) {
	var seen wsproto.ProbeRequest
	provider := &stubProvider{name: "@putnami/go", seen: &seen, result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go"}}

	if _, err := RunProbe([]ProbeProvider{provider},
		wsproto.ProbeRequest{Reason: wsproto.ProbeReasonPlan, Paths: []string{"svc"}}, nil); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if seen.Version != wsproto.ProbeProtocolVersion {
		t.Errorf("request version = %d", seen.Version)
	}
	if seen.Extension != "@putnami/go" {
		t.Errorf("request extension = %q", seen.Extension)
	}
	if seen.Reason != wsproto.ProbeReasonPlan || len(seen.Paths) != 1 {
		t.Errorf("request = %+v", seen)
	}
}

func TestRunProbe_TypedFailures(t *testing.T) {
	cases := []struct {
		name     string
		provider ProbeProvider
		want     wsproto.ProbeFailureKind
	}{
		{
			name: "untyped provider error is classified",
			provider: &stubProvider{name: "@putnami/go",
				err: errors.New("exec: no such file")},
			want: wsproto.ProbeFailureTransport,
		},
		{
			name: "typed provider error survives",
			provider: &stubProvider{name: "@putnami/go",
				err: wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, "@putnami/go", "not installed")},
			want: wsproto.ProbeFailureUnavailable,
		},
		{
			name: "misattributed answer",
			provider: &stubProvider{name: "@putnami/go", result: wsproto.ProbeResult{
				Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript"}},
			want: wsproto.ProbeFailureTransport,
		},
		{
			name: "non-conformant answer",
			provider: &stubProvider{name: "@putnami/go", result: wsproto.ProbeResult{
				Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
				Projects: []wsproto.ProbeProject{{Path: "/absolute"}}}},
			want: wsproto.ProbeFailureInvalidResult,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := RunProbe([]ProbeProvider{tc.provider}, wsproto.ProbeRequest{}, nil)
			if outcome != nil {
				t.Error("a failed probe must not yield a partial outcome")
			}
			var failure *wsproto.ProbeFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v (%T), want a typed *wsproto.ProbeFailure", err, err)
			}
			if failure.Kind != tc.want {
				t.Errorf("kind = %q, want %q", failure.Kind, tc.want)
			}
		})
	}
}

func TestRunProbe_ConflictIsATypedFailure(t *testing.T) {
	a := &stubProvider{name: "@putnami/go", result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
		Projects: []wsproto.ProbeProject{{Path: "svc", Type: "library"}}}}
	b := &stubProvider{name: "@putnami/typescript", result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
		Projects: []wsproto.ProbeProject{{Path: "svc", Type: "application"}}}}

	_, err := RunProbe([]ProbeProvider{a, b}, wsproto.ProbeRequest{}, nil)
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureConflict {
		t.Fatalf("err = %v, want a merge-conflict failure", err)
	}
	if len(failure.Diagnostics) == 0 {
		t.Error("a conflict failure must carry the causal diagnostics")
	}

	// Explicit config resolves it, exactly as the merge rules say.
	if _, err := RunProbe([]ProbeProvider{a, b}, wsproto.ProbeRequest{},
		map[string]wsproto.ExplicitProject{"svc": {Type: "application"}}); err != nil {
		t.Errorf("explicit config did not resolve the conflict: %v", err)
	}
}

func TestRunProbe_CarriesAdvisoryDiagnostics(t *testing.T) {
	provider := &stubProvider{name: "@putnami/go", result: wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/go",
		Diagnostics: []diag.Diagnostic{diag.Warningf("slow", "", "took a while")}}}

	outcome, err := RunProbe([]ProbeProvider{provider}, wsproto.ProbeRequest{}, nil)
	if err != nil {
		t.Fatalf("an advisory warning must not fail the probe: %v", err)
	}
	if len(outcome.Diagnostics) != 1 || outcome.Diagnostics[0].Code != "slow" {
		t.Errorf("diagnostics = %v", outcome.Diagnostics)
	}
}

// --- failure policy ---------------------------------------------------------

func TestRequireProbe_GraphCommandsFailAndRecoveryCommandsSurvive(t *testing.T) {
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, "@putnami/go", "not installed")

	blocked := []string{"build", "test", "lint", "publish", "deploy", "projects list", "workspace describe"}
	for _, path := range blocked {
		if err := RequireProbe(path, failure); err == nil {
			t.Errorf("%q ran on a half-known project graph", path)
		}
	}

	// Exactly the recovery set: the user must be able to repair
	// the workspace, and to read help, after a provider breaks.
	survivors := []string{"install", "extensions", "extensions install", "projects sync", "help", "version"}
	for _, path := range survivors {
		if err := RequireProbe(path, failure); err != nil {
			t.Errorf("recovery command %q was blocked by a probe failure: %v", path, err)
		}
	}

	if err := RequireProbe("build", nil); err != nil {
		t.Errorf("a successful probe must not block anything: %v", err)
	}
}

func TestCommandmetaRecoveryAllowlist(t *testing.T) {
	if !commandmeta.IsRecoveryCommand("extensions") {
		t.Error("the bare `extensions` root must resolve through DefaultSub")
	}
	if commandmeta.IsRecoveryCommand("") || commandmeta.IsRecoveryCommand("not-a-command") {
		t.Error("an unknown path is not a recovery command")
	}
	if commandmeta.IsRecoveryCommand("cache gc") {
		t.Error("cache gc is not a recovery command")
	}
	if len(commandmeta.RecoveryCommands()) == 0 {
		t.Fatal("the recovery allowlist is empty; a probe failure would be unrecoverable")
	}
	for _, command := range commandmeta.RecoveryCommands() {
		if !commandmeta.IsRecoveryCommand(command.Path) {
			t.Errorf("%q is marked Recovery but IsRecoveryCommand says otherwise", command.Path)
		}
	}
}
