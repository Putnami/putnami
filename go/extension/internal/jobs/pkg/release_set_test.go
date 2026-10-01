package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/releaseplan"
	distribution "go.putnami.dev/protocol/distribution"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	alphaModule    = "go.putnami.dev/alpha"
	betaModule     = "go.putnami.dev/beta"
	consumerModule = "go.putnami.dev/consumer"
	alphaOld       = "v1.1.0"
	betaOld        = "v1.7.0"
	consumerOld    = "v1.0.0"
	alphaNext      = "v1.2.0-canary.1"
	consumerNext   = "v1.1.0-canary.1"
)

func TestSparseGoPackageDownstreamOnlyInheritsExactMixedVersions(t *testing.T) {
	workspace := t.TempDir()
	writeGoReleaseProject(t, workspace, "alpha", alphaModule, "")
	writeGoReleaseProject(t, workspace, "beta", betaModule, "")
	writeGoReleaseProject(t, workspace, "consumer", consumerModule, `require (
	go.putnami.dev/alpha v0.0.0
	go.putnami.dev/beta v0.0.0
	example.com/external v1.9.3
)

replace (
	go.putnami.dev/alpha => ../alpha
	go.putnami.dev/beta => ../beta
)
`)
	params := sparseGoPlanParams(t, false)

	status, _, err := Run(goReleaseContext(workspace, "consumer", consumerModule, params), jsonl.New(), []string{"--go"})
	if err != nil || status != "OK" {
		t.Fatalf("package downstream = (%q, %v), want OK", status, err)
	}

	for _, unchanged := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(workspace, ".putnami", "out", unchanged)); !os.IsNotExist(err) {
			t.Errorf("unchanged upstream %s was packaged (stat err=%v)", unchanged, err)
		}
	}
	staged := readStagedGoMod(t, workspace, "consumer")
	for _, exact := range []string{alphaModule + " " + alphaOld, betaModule + " " + betaOld, "example.com/external v1.9.3"} {
		if !strings.Contains(staged, exact) {
			t.Errorf("staged go.mod missing %q:\n%s", exact, staged)
		}
	}
	if strings.Contains(staged, "replace") || strings.Contains(staged, "v0.0.0") {
		t.Errorf("staged go.mod retained a replace or placeholder:\n%s", staged)
	}
	metadata, err := os.ReadFile(filepath.Join(workspace, ".putnami", "out", "consumer", "package", "go", "module.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metadata), `"version": "`+consumerNext+`"`) {
		t.Errorf("module metadata did not use exact candidate version:\n%s", metadata)
	}
}

func TestSparseGoPackageUpstreamChangeRepackagesDownstream(t *testing.T) {
	workspace := t.TempDir()
	writeGoReleaseProject(t, workspace, "alpha", alphaModule, "")
	writeGoReleaseProject(t, workspace, "beta", betaModule, "")
	writeGoReleaseProject(t, workspace, "consumer", consumerModule,
		"require "+alphaModule+" v0.0.0\n\nrequire "+betaModule+" v0.0.0\n")
	params := sparseGoPlanParams(t, true)

	for _, project := range []struct{ path, module string }{{"alpha", alphaModule}, {"consumer", consumerModule}} {
		status, _, err := Run(goReleaseContext(workspace, project.path, project.module, params), jsonl.New(), []string{"--go"})
		if err != nil || status != "OK" {
			t.Fatalf("package %s = (%q, %v), want OK", project.path, status, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, ".putnami", "out", "alpha", "package", "go")); err != nil {
		t.Fatalf("selected upstream was not packaged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".putnami", "out", "beta")); !os.IsNotExist(err) {
		t.Errorf("unchanged beta upstream was packaged (stat err=%v)", err)
	}
	staged := readStagedGoMod(t, workspace, "consumer")
	if !strings.Contains(staged, alphaModule+" "+alphaNext) {
		t.Errorf("downstream did not reference repackaged upstream %s:\n%s", alphaNext, staged)
	}
	if !strings.Contains(staged, betaModule+" "+betaOld) {
		t.Errorf("downstream did not retain unchanged upstream %s:\n%s", betaOld, staged)
	}
}

func TestRewritePlannedWorkspaceDepsRejectsMissingMalformedAndMismatchedRequirements(t *testing.T) {
	plan := parseGoPlan(t, sparseGoPlanParams(t, false))
	consumer, ok := plan.Member(releaseplan.GoEcosystem, consumerModule)
	if !ok {
		t.Fatal("consumer missing from test plan")
	}

	tests := []struct {
		name    string
		goMod   string
		mutate  func(*releaseset.PlannedMember)
		wantErr string
	}{
		{
			name:    "missing member",
			goMod:   "module " + consumerModule + "\n\ngo 1.25\n\nrequire go.putnami.dev/missing v1.0.0\n",
			wantErr: "absent from release-set plan",
		},
		{
			name:    "malformed version",
			goMod:   "module " + consumerModule + "\n\ngo 1.25\n\nrequire " + alphaModule + " not-a-version\n",
			wantErr: `version "not-a-version" invalid`,
		},
		{
			name:    "mismatched dependency",
			goMod:   "module " + consumerModule + "\n\ngo 1.25\n\nrequire " + alphaModule + " v0.0.0\n",
			mutate:  func(member *releaseset.PlannedMember) { member.Dependencies[0].Version = "v9.9.9" },
			wantErr: "does not match release-set member version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			member := consumer
			member.Dependencies = append([]distribution.ReleaseSetDependency(nil), consumer.Dependencies...)
			if test.mutate != nil {
				test.mutate(&member)
			}
			_, err := rewritePlannedWorkspaceDeps(test.goMod, member, plan)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("rewrite error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSparseGoPackageRejectsMissingOrMalformedCurrentMember(t *testing.T) {
	workspace := t.TempDir()
	writeGoReleaseProject(t, workspace, "consumer", consumerModule, "")

	otherOnly := releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads:           map[string]*distribution.ChannelHead{"canary": nil},
		Members: []releaseset.PlannedMember{{
			Ecosystem: releaseplan.GoEcosystem, Coordinate: alphaModule, Version: alphaNext,
			Dependencies: []distribution.ReleaseSetDependency{}, Selected: true, ProjectID: "/alpha",
			SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("a"),
		}},
	}
	missingRaw, err := json.Marshal(&otherOnly)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		raw     json.RawMessage
		wantErr string
	}{
		{"missing", missingRaw, "absent from release-set plan"},
		{"malformed", json.RawMessage(`{"protocolVersion":2,"namespace":"putnami","channel":"canary","members":[{"ecosystem":"go","coordinate":"NOT A MODULE","version":"v1.0.0","dependencies":[],"sourceRevision":0123456789abcdef0123456789abcdef01234567,"contentFingerprint":"sha256:1111111111111111111111111111111111111111111111111111111111111111","selected":true,"projectId":"/bad"}]}`), "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := goReleaseContext(workspace, "consumer", consumerModule, pctx.Params{releaseset.ContextParamName: test.raw})
			status, _, err := Run(ctx, jsonl.New(), []string{"--go"})
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("package = (%q, %v), want FAILED containing %q", status, err, test.wantErr)
			}
		})
	}
}

func sparseGoPlanParams(t *testing.T, selectAlpha bool) pctx.Params {
	t.Helper()
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	base := &distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: alphaModule, Version: alphaOld, ArtifactDigest: digest("a"), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("1")},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: betaModule, Version: betaOld, ArtifactDigest: digest("b"), Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("2")},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: consumerModule, Version: consumerOld, ArtifactDigest: digest("c"), Dependencies: []distribution.ReleaseSetDependency{
				{Ecosystem: releaseplan.GoEcosystem, Coordinate: alphaModule, Version: alphaOld},
				{Ecosystem: releaseplan.GoEcosystem, Coordinate: betaModule, Version: betaOld},
			}, SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("3")},
		},
	}
	baseRef, diagnostics := distribution.DeriveReleaseSetRef(base)
	if len(diagnostics) > 0 {
		t.Fatalf("derive test base ref: %v", diagnostics)
	}
	alphaVersion := alphaOld
	alphaDigest := digest("a")
	alphaFingerprint := goTestFingerprint("1")
	if selectAlpha {
		alphaVersion = alphaNext
		alphaDigest = ""
		alphaFingerprint = goTestFingerprint("9")
	}
	plan := releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads: map[string]*distribution.ChannelHead{
			"canary": {Ref: baseRef, Generation: 3, ReleaseSet: base},
		},
		Members: []releaseset.PlannedMember{
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: alphaModule, Version: alphaVersion, ArtifactDigest: alphaDigest, Dependencies: []distribution.ReleaseSetDependency{}, Selected: selectAlpha, ProjectID: "/alpha", SourceRevision: goTestRevision, SelectionFingerprint: alphaFingerprint},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: betaModule, Version: betaOld, ArtifactDigest: digest("b"), Dependencies: []distribution.ReleaseSetDependency{}, ProjectID: "/beta", SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("2")},
			{Ecosystem: releaseplan.GoEcosystem, Coordinate: consumerModule, Version: consumerNext, Dependencies: []distribution.ReleaseSetDependency{
				{Ecosystem: releaseplan.GoEcosystem, Coordinate: alphaModule, Version: alphaVersion},
				{Ecosystem: releaseplan.GoEcosystem, Coordinate: betaModule, Version: betaOld},
			}, Selected: true, ProjectID: "/consumer", SourceRevision: goTestRevision, SelectionFingerprint: goTestFingerprint("8")},
		},
	}
	raw, err := json.Marshal(&plan)
	if err != nil {
		t.Fatal(err)
	}
	return pctx.Params{releaseset.ContextParamName: raw}
}

func parseGoPlan(t *testing.T, params pctx.Params) *releaseset.Plan {
	t.Helper()
	plan, err := releaseset.ParseParams(params)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func writeGoReleaseProject(t *testing.T, workspace, path, modulePath, requires string) {
	t.Helper()
	root := filepath.Join(workspace, path)
	mustWrite(t, filepath.Join(root, "go.mod"), "module "+modulePath+"\n\ngo 1.25\n\n"+requires)
	mustWrite(t, filepath.Join(root, path+".go"), "package "+path+"\n")
}

func goReleaseContext(workspace, path, modulePath string, params pctx.Params) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: workspace,
		Workspace:     pctx.Workspace{Version: "9.9.9"},
		Project:       pctx.Project{Name: modulePath, Path: path, FullPath: filepath.Join(workspace, path)},
		Params:        params,
	}
}

func readStagedGoMod(t *testing.T, workspace, project string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, ".putnami", "out", project, "package", "go", "source", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const goTestRevision = "0123456789abcdef0123456789abcdef01234567"

func goTestFingerprint(char string) string { return "sha256:" + strings.Repeat(char, 64) }
