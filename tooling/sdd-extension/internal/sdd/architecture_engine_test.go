package sdd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func TestDiscoverFindsExactContainedAuthorityFilesAndSkipsOnlyNonAuthorityTrees(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "contained-read-only-tooling", "discovery-finds-only-exact-contained-authority-files")
	root := t.TempDir()
	writeArchitectureTestFile(t, filepath.Join(root, "z-consumer", archproto.ManifestFilename), architectureConsumerManifest(true))
	writeArchitectureTestFile(t, filepath.Join(root, "a-producer", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, "fixtures", "declared", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, ".hidden", archproto.ManifestFilename), `{"unknown":true}`)
	writeArchitectureTestFile(t, filepath.Join(root, "node_modules", "ignored", archproto.ManifestFilename), `{"unknown":true}`)

	result := Discover(root)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("discovery diagnostics = %+v", result.Diagnostics)
	}
	if len(result.Sources) != 3 {
		t.Fatalf("sources = %+v, want three", result.Sources)
	}
	want := []string{
		"a-producer/" + archproto.ManifestFilename,
		"fixtures/declared/" + archproto.ManifestFilename,
		"z-consumer/" + archproto.ManifestFilename,
	}
	for index, path := range want {
		if result.Sources[index].Path != path {
			t.Fatalf("source[%d] = %q, want %q", index, result.Sources[index].Path, path)
		}
	}
}

func TestDetectProjectDependenciesReportsOnlyMappedCrossDomainEdges(t *testing.T) {
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer", "unmapped"}},
		{ID: "/unmapped", Name: "unmapped", Path: "unmapped"},
	})
	sources := []archproto.ManifestSource{
		{Path: "producer/" + archproto.ManifestFilename, Manifest: mustParseArchitectureManifest(t, architectureProducerManifest())},
		{Path: "consumer/" + archproto.ManifestFilename, Manifest: mustParseArchitectureManifest(t, architectureConsumerManifest(true))},
	}

	edges := DetectProjectDependencies(ws, sources)
	if len(edges) != 1 {
		t.Fatalf("observed edges = %+v, want one mapped cross-domain edge", edges)
	}
	edge := edges[0]
	if edge.ConsumerDomain != "consumer" || edge.ProducerDomain != "producer" || edge.ConsumerProject != "/consumer" || edge.ProducerProject != "/producer" {
		t.Fatalf("observed edge = %+v", edge)
	}
}

func TestEvaluateWorkspaceUsesOneDeclaredObservedInterpretation(t *testing.T) {
	for _, test := range []struct {
		name         string
		binding      bool
		wantFindings int
		wantBlocking bool
	}{
		{name: "exact binding", binding: true},
		{name: "undeclared dependency", binding: false, wantFindings: 1, wantBlocking: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), architectureProducerManifest())
			writeArchitectureTestFile(t, filepath.Join(root, "consumer", archproto.ManifestFilename), architectureConsumerManifest(test.binding))
			ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
				{ID: "/producer", Name: "producer", Path: "producer"},
				{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer"}},
			})

			result := EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
			if len(result.Diagnostics) != 0 {
				t.Fatalf("structural diagnostics = %+v", result.Diagnostics)
			}
			if result.Snapshot == nil {
				t.Fatal("snapshot is nil")
			}
			if len(result.Snapshot.Findings) != test.wantFindings {
				t.Fatalf("findings = %+v, want %d", result.Snapshot.Findings, test.wantFindings)
			}
			if got := archproto.HasBlockingFindings(result.Snapshot.Findings); got != test.wantBlocking {
				t.Fatalf("blocking = %t, want %t", got, test.wantBlocking)
			}
			if result.Snapshot.Coverage.ProjectDependencies != "enforced-for-mapped-projects" || result.Snapshot.Coverage.HTTP != "not-detected" {
				t.Fatalf("coverage = %+v", result.Snapshot.Coverage)
			}
		})
	}
}

func TestEvaluateWorkspaceReportsEachSemanticDiagnosticOnce(t *testing.T) {
	root := t.TempDir()
	invalid := `{
  "protocolVersion": 1,
  "domain": "producer",
  "owner": "producer-team",
  "projects": ["/producer"],
  "exports": [{
    "id": "producer.reference.v1",
    "version": 1,
    "status": "not-a-status",
    "description": "Stable producer reference.",
    "facts": [{"name":"id","authority":"producer","classification":"internal","personalData":"none"}],
    "modes": ["reference"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": []
}`
	writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), invalid)
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
	})

	result := EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
	if result.Snapshot != nil {
		t.Fatal("invalid repository published a snapshot")
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != archproto.ErrorCodeInvalidStatus {
		t.Fatalf("diagnostics = %+v, want one invalid-status finding", result.Diagnostics)
	}
}

func TestEvaluateWorkspaceFailsClosedOnTypedIncompleteProviderView(t *testing.T) {
	root := t.TempDir()
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, nil)
	ws.AddWarning(workspace.WarningCodeProviderViewUnavailable, "this human wording may change freely")

	result := EvaluateWorkspace(ws, EvaluationOptions{Today: architectureEngineTestDate()})
	if result.Snapshot != nil || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != ErrorCodeIncompleteProjectGraph {
		t.Fatalf("incomplete provider view did not fail closed: %+v", result)
	}
}

func TestEvaluateWorkspaceLoadsPriorBaselineFromImmutableGitObjects(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "contained-read-only-tooling", "the-prior-baseline-loads-from-immutable-git-objects")
	root := t.TempDir()
	writeArchitectureTestFile(t, filepath.Join(root, "producer", archproto.ManifestFilename), architectureProducerManifest())
	writeArchitectureTestFile(t, filepath.Join(root, "consumer", archproto.ManifestFilename), architectureConsumerManifest(false))
	writeArchitectureTestFile(t, filepath.Join(root, archproto.BaselineFilename), `{"protocolVersion":1,"findings":[]}`)
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "architecture-test@putnami.dev"},
		{"config", "user.name", "Architecture Test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-q", "-m", "initial empty architecture baseline"},
	} {
		runArchitectureTestGit(t, root, arguments...)
	}
	base := strings.TrimSpace(runArchitectureTestGit(t, root, "rev-parse", "HEAD"))
	edge := archproto.ObservedEdge{
		Kind:            archproto.BindingProjectDependency,
		ProducerDomain:  "producer",
		ConsumerDomain:  "consumer",
		ProducerProject: "/producer",
		ConsumerProject: "/consumer",
	}
	baseline := &archproto.Baseline{ProtocolVersion: 1, Findings: []archproto.DebtRecord{{
		Finding:    archproto.StableFindingID(archproto.ErrorCodeUndeclaredProjectDependency, edge),
		Owner:      "consumer-team",
		Reason:     "Post-adoption dependency",
		Scope:      "/consumer -> /producer",
		RemoveWhen: "The dependency is removed or receives an active DARC binding",
	}}}
	baselineBytes, err := archproto.MarshalBaseline(baseline)
	if err != nil {
		t.Fatal(err)
	}
	writeArchitectureTestFile(t, filepath.Join(root, archproto.BaselineFilename), string(baselineBytes))
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "architecture-test"}, []*workspace.Project{
		{ID: "/producer", Name: "producer", Path: "producer"},
		{ID: "/consumer", Name: "consumer", Path: "consumer", Dependencies: []string{"producer"}},
	})

	result := EvaluateWorkspace(ws, EvaluationOptions{BaselineRef: base, Today: architectureEngineTestDate()})
	if len(result.Diagnostics) != 0 || result.Snapshot == nil {
		t.Fatalf("evaluation = %+v", result)
	}
	if !result.Baseline.Compared || !result.Baseline.PriorFile || result.Baseline.Commit != "git:"+base {
		t.Fatalf("baseline status = %+v", result.Baseline)
	}
	if result.Snapshot.Ratchet.KnownDebt != 1 || result.Snapshot.Ratchet.BaselineGrowth != 1 || !archproto.HasBlockingFindings(result.Snapshot.Findings) {
		t.Fatalf("ratchet = %+v; findings = %+v", result.Snapshot.Ratchet, result.Snapshot.Findings)
	}
}

func mustParseArchitectureManifest(t *testing.T, input string) *archproto.Manifest {
	t.Helper()
	manifest, diagnostics := archproto.ParseAndValidateManifest([]byte(input))
	if manifest == nil || len(diagnostics) != 0 {
		t.Fatalf("parse architecture manifest: %+v", diagnostics)
	}
	return manifest
}

func writeArchitectureTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runArchitectureTestGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func architectureEngineTestDate() time.Time {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
}
