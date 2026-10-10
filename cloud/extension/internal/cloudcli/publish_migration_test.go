package cloudcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestPublishMigrationDryRunBindsTheSelectedMemberFromACachedBundle(t *testing.T) {
	root := t.TempDir()
	app := "apps/example-api"
	project := filepath.Join(root, filepath.FromSlash(app))
	bundle := filepath.Join(project, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "payload", "sql", "default", "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	manifest := `{"protocol":"migration-bundle.v1","appName":"` + app + `","version":"0.9.0","digest":"` + digest + `"}`
	manifest = strings.ReplaceAll(manifest, "\\\"", "\"")
	projectManifest := `{"name":"` + app + `"}`
	projectManifest = strings.ReplaceAll(projectManifest, "\\\"", "\"")
	if err := os.WriteFile(filepath.Join(project, "putnami.json"), []byte(projectManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "bundle.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "payload", "sql", "default", "core", "001.up.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"app": app, "namespace": "workspace-native", "dry-run": true,
		releaseset.ContextParamName: &releaseset.Plan{
			ProtocolVersion: distributionproto.ProtocolVersion, Namespace: "release", Channels: []string{"stable"},
			Heads: map[string]*distributionproto.ChannelHead{"stable": nil},
			Members: []releaseset.PlannedMember{{
				Ecosystem: "put", Coordinate: "workspace-native/apps-example-api", Version: "1.2.3",
				SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
				Selected: true, ProjectID: "/apps/example-api",
			}, {
				// Config shares the project and Put ecosystem, but has its own
				// coordinate and package fingerprint in the same publication.
				Ecosystem: "put", Coordinate: "workspace-native/apps-example-api-config", Version: "1.2.3",
				SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: "sha256:" + strings.Repeat("d", 64),
				Selected: true, ProjectID: "/apps/example-api",
			}},
		},
	}
	var lines []string
	if err := publishMigration(params, nil, root, nil, IO{Stdout: func(line string) { lines = append(lines, line) }}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "workspace-native/apps-example-api@1.2.3") {
		t.Fatalf("dry-run output = %q", lines)
	}
	if _, mutated := params["bundle-version"]; mutated {
		t.Fatal("publication changed the caller's parameters")
	}
	if raw, err := os.ReadFile(filepath.Join(bundle, "bundle.json")); err != nil || string(raw) != manifest {
		t.Fatalf("publication rewrote the cached bundle: %q, err=%v", raw, err)
	}
	for _, alias := range []string{"bundle-version", "bundleVersion", "version"} {
		params[alias] = "0.9.0"
		if err := publishMigration(params, nil, root, nil, IO{}); err == nil || !strings.Contains(err.Error(), "does not match selected member") {
			t.Fatalf("explicit %s contradicted the plan: err=%v", alias, err)
		}
		delete(params, alias)
	}
}

// unbuiltMigrationProject writes a workload whose build produced no migration
// bundle. An empty namespace leaves the migration namespace undeclared.
func unbuiltMigrationProject(t *testing.T, namespace string) (string, string) {
	t.Helper()
	root := t.TempDir()
	app := "apps/api"
	project := filepath.Join(root, filepath.FromSlash(app))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"name": app}
	if namespace != "" {
		manifest["options"] = map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": namespace}}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "putnami.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, app
}

func selectedPutMember(coordinate, fingerprint string) releaseset.PlannedMember {
	return releaseset.PlannedMember{
		Ecosystem: "put", Coordinate: coordinate, Version: "1.2.3",
		SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: "sha256:" + strings.Repeat(fingerprint, 64),
		Selected: true, ProjectID: "/apps/api",
	}
}

func stablePlan(members ...releaseset.PlannedMember) *releaseset.Plan {
	return &releaseset.Plan{
		ProtocolVersion: distributionproto.ProtocolVersion, Namespace: "release", Channels: []string{"stable"},
		Heads: map[string]*distributionproto.ChannelHead{"stable": nil}, Members: members,
	}
}

func TestPublishMigrationFailsWhenThePlanSelectsAMissingBundle(t *testing.T) {
	root, app := unbuiltMigrationProject(t, "cloud")
	params := map[string]any{
		"app": app, "if-present": true,
		releaseset.ContextParamName: stablePlan(selectedPutMember("cloud/apps-api", "c")),
	}
	var lines []string
	err := publishMigration(params, nil, root, nil, IO{Stdout: func(line string) { lines = append(lines, line) }})
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != ExitUsage {
		t.Fatalf("err=%v, want a usage error", err)
	}
	resolvedRoot, evalErr := filepath.EvalSymlinks(root)
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	bundle := filepath.Join(resolvedRoot, filepath.FromSlash(app), ".gen", "migration-bundle", "bundle.json")
	want := "release-set plan selects migration member cloud/apps-api but " + bundle +
		" does not exist; the build produced no migration bundle for apps/api"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err=%q\nwant=%q", err.Error(), want)
	}
	if len(lines) != 0 {
		t.Fatalf("a refused publication reported a skip: %q", lines)
	}
}

func TestPublishMigrationSkipsAMissingBundleNoPlanSelects(t *testing.T) {
	migration := selectedPutMember("cloud/apps-api", "c")
	config := selectedPutMember("cloud/apps-api-config", "d")
	for name, tc := range map[string]struct {
		namespace string
		plan      *releaseset.Plan
	}{
		"no release-set plan":                     {namespace: "cloud"},
		"plan selects only the config member":     {namespace: "cloud", plan: stablePlan(config)},
		"plan selects another namespace":          {namespace: "other", plan: stablePlan(migration)},
		"project declares no migration namespace": {plan: stablePlan(migration)},
	} {
		t.Run(name, func(t *testing.T) {
			root, app := unbuiltMigrationProject(t, tc.namespace)
			params := map[string]any{"app": app, "if-present": true}
			if tc.plan != nil {
				params[releaseset.ContextParamName] = tc.plan
			}
			var lines []string
			if err := publishMigration(params, nil, root, nil, IO{Stdout: func(line string) { lines = append(lines, line) }}); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "skipping migration publication") {
				t.Fatalf("skip output = %q", lines)
			}
		})
	}
}

func TestMigrationSelectionFailsClosedOnMissingOrMismatchedPlan(t *testing.T) {
	prepared := &datacli.PreparedMigration{ProjectPath: "/app", ExpectedCoordinate: "owner/app", Version: "1"}
	if _, err := migrationSelection(map[string]any{}, prepared); err == nil {
		t.Fatal("missing provenance accepted")
	}
	direct := map[string]any{"source-revision": "revision", "selection-fingerprint": "sha256:" + strings.Repeat("d", 64)}
	if got, err := migrationSelection(direct, prepared); err != nil || got.SourceRevision != "revision" {
		t.Fatalf("direct=%+v err=%v", got, err)
	}
	direct["channel"] = "stable"
	if _, err := migrationSelection(direct, prepared); err == nil {
		t.Fatal("direct channel accepted")
	}
	plan := &releaseset.Plan{
		ProtocolVersion: distributionproto.ProtocolVersion, Namespace: "release", Channels: []string{"stable"}, Heads: map[string]*distributionproto.ChannelHead{"stable": nil},
		Members: []releaseset.PlannedMember{{Ecosystem: "put", Coordinate: "other/app", Version: "1", SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("e", 64), Selected: true, ProjectID: "/app"}},
	}
	if _, err := migrationSelection(map[string]any{releaseset.ContextParamName: plan}, prepared); err == nil || !strings.Contains(err.Error(), "does not select") {
		t.Fatalf("coordinate mismatch err=%v", err)
	}
	plan.Members[0].Coordinate = prepared.ExpectedCoordinate
	plan.Members[0].Version = "2"
	if _, err := migrationSelection(map[string]any{releaseset.ContextParamName: plan}, prepared); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch err=%v", err)
	}
}

func TestEmitMigrationPublishedMemberUsesNativeArtifactEnvelope(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	accepted := &datacli.PublishedMigration{Member: datacli.MigrationMember{Ecosystem: "put", Coordinate: "owner/app", Version: "1", ArtifactDigest: digest}}
	var event map[string]any
	err := emitMigrationPublishedMember(IO{Artifact: func(id, name, kind, _ string, data map[string]any) error {
		if id != "migration-owner-app" || name != "owner/app" || kind != extensionproto.PublishedMemberEventKind {
			t.Fatalf("envelope=%s/%s/%s", id, name, kind)
		}
		event = data
		return nil
	}}, accepted)
	if err != nil || event["artifactDigest"] != digest {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	requireDeclaredMemberProfile(t, stringValue(event["ecosystem"]), stringValue(event["coordinate"]), stringValue(event["version"]))
	accepted.Member.ArtifactDigest = "bad"
	if err := emitMigrationPublishedMember(IO{Artifact: func(string, string, string, string, map[string]any) error { return errors.New("must not run") }}, accepted); err == nil {
		t.Fatal("invalid member emitted")
	}
	if err := emitMigrationPublishedMember(IO{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPlanJSONValueIsStrictlyParsed(t *testing.T) {
	prepared := &datacli.PreparedMigration{ProjectPath: "/app", ExpectedCoordinate: "owner/app", Version: "1"}
	raw := map[string]any{"protocolVersion": distributionproto.ProtocolVersion, "unknown": true}
	if _, err := migrationSelection(map[string]any{releaseset.ContextParamName: raw}, prepared); err == nil {
		t.Fatal("unknown plan field accepted")
	}
}
