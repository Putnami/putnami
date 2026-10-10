package configcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/config/authoredmember"
)

const authoredTestRevision = "0123456789abcdef0123456789abcdef01234567"

func TestPrepareAuthoredConfigBuildsCanonicalSecretFreeProductionMember(t *testing.T) {
	root := authoredConfigWorkspace(t)
	stubAuthoredGit(t)
	prepared, err := PrepareAuthoredConfig(map[string]any{
		"app": "my-app", "namespace": "workspace-native", "env": "prod",
	}, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Coordinate != "workspace-native/my-app-config" || prepared.Version != "1.2.3" || prepared.ProjectID != "/" {
		t.Fatalf("prepared identity = %+v", prepared)
	}
	descriptor, err := (authoredmember.Publisher{}).Validate(t.Context(), prepared.Member)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.SourceProvenance.Repository != "github.com/acme/storefront" || descriptor.SourceProvenance.Revision != authoredTestRevision {
		t.Fatalf("source provenance = %+v", descriptor.SourceProvenance)
	}
	if bytes.Contains(prepared.Member.Bytes, []byte("PLAINTEXT_SENTINEL")) || !bytes.Contains(prepared.Member.Bytes, []byte("config-secret://server/token")) {
		t.Fatalf("member is not secret-free: %s", prepared.Member.Bytes)
	}
	if !bytes.Contains(prepared.Member.Bytes, []byte(`"server.port":8080`)) || !bytes.Contains(prepared.Member.Bytes, []byte(`"server.options.enabled":true`)) {
		t.Fatalf("member omitted canonical authored values: %s", prepared.Member.Bytes)
	}
}

func TestPackageAndVerifyAuthoredConfigRequireByteExactArtifact(t *testing.T) {
	root := authoredConfigWorkspace(t)
	stubAuthoredGit(t)
	params := map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}
	prepared, err := PackageAuthoredConfig(params, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPackagedAuthoredConfig(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prepared.PackagedMemberPath, append([]byte(nil), `{}`...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPackagedAuthoredConfig(t.Context(), prepared); err == nil {
		t.Fatal("different packaged bytes were accepted")
	}
}

func TestPrepareAuthoredConfigFailsClosedOutsideQualifiedEnvironmentAndOnMixedSensitiveArray(t *testing.T) {
	root := authoredConfigWorkspace(t)
	stubAuthoredGit(t)
	if _, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "staging"}, nil, root, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "env=prod") {
		t.Fatalf("staging error = %v", err)
	}
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "server", "fields": []any{
			map[string]any{"name": "backends", "type": "array", "required": true, "items": map[string]any{"fields": []any{
				map[string]any{"name": "name", "type": "string", "required": true},
				map[string]any{"name": "token", "type": "string", "sensitive": true, "required": true},
			}}},
		}}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("server:\n  backends:\n    - name: primary\n      token: PLAINTEXT_SENTINEL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}, nil, root, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "sensitive descendants") {
		t.Fatalf("mixed array error = %v", err)
	}
}

func TestAuthoredConfigCoordinateUsesLocalCanonicalProjectNaming(t *testing.T) {
	coordinate, err := AuthoredConfigCoordinate("workspace-native", "apps/config-api")
	if err != nil || coordinate != "workspace-native/apps-config-api-config" {
		t.Fatalf("coordinate=%q err=%v", coordinate, err)
	}
	for _, project := range []string{"/absolute", "a//b", "a/../b"} {
		if _, err := AuthoredConfigCoordinate("workspace-native", project); err == nil {
			t.Fatalf("project %q accepted", project)
		}
	}
}

func authoredConfigWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeLinkFile(t, root)
	writeProjectFile(t, root, "my-app")
	for _, dir := range []string{"schema", "conf", ".gen"} {
		mustMkdir(t, filepath.Join(root, dir))
	}
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "server", "fields": []any{
			map[string]any{"name": "port", "type": "int", "required": true},
			map[string]any{"name": "token", "type": "string", "required": true, "sensitive": true},
			map[string]any{"name": "options", "type": "object", "fields": []any{
				map[string]any{"name": "enabled", "type": "bool", "required": true},
			}},
		}}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("server:\n  port: 8080\n  token: PLAINTEXT_SENTINEL\n  options:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(root, ".gen", "version.json"), map[string]any{"version": "1.2.3", "sha": "01234567"})
	return root
}

func stubAuthoredGit(t *testing.T) {
	t.Helper()
	previous := authoredGitOutput
	authoredGitOutput = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			return []byte(authoredTestRevision + "\n"), nil
		case "remote get-url origin":
			return []byte("git@github.com:acme/storefront.git\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	t.Cleanup(func() { authoredGitOutput = previous })
}

func TestPrepareAuthoredConfigFollowsTheProjectNamespaceDeclaration(t *testing.T) {
	root := authoredConfigWorkspace(t)
	stubAuthoredGit(t)

	// Undeclared namespace: the package step's --if-present skips (no member
	// exists, exactly as the workspace probe announces none); an explicit
	// request without --namespace is refused with guidance.
	prepared, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod", "if-present": true}, nil, root, clicore.IO{})
	if err != nil || prepared != nil {
		t.Fatalf("undeclared namespace should skip: prepared=%+v err=%v", prepared, err)
	}
	if _, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod"}, nil, root, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "declares no native Config namespace") {
		t.Fatalf("undeclared namespace accepted: %v", err)
	}

	// Declared in the manifest: no flag is needed and the command layer wins
	// over the extension layer, mirroring the probe's precedence.
	writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "my-app", "options": map[string]any{
		"@putnami/cloud":                map[string]any{"namespace": "workspace-native"},
		"@putnami/cloud:publish-config": map[string]any{"namespace": "config-native"},
	}})
	prepared, err = PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod", "if-present": true}, nil, root, clicore.IO{})
	if err != nil || prepared == nil || prepared.Coordinate != "config-native/my-app-config" || prepared.Namespace != "config-native" || prepared.Package != "my-app-config" {
		t.Fatalf("declared namespace not honored: prepared=%+v err=%v", prepared, err)
	}

	// An explicit flag still overrides the declaration.
	prepared, err = PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod", "namespace": "explicit"}, nil, root, clicore.IO{})
	if err != nil || prepared == nil || prepared.Coordinate != "explicit/my-app-config" {
		t.Fatalf("explicit namespace not honored: prepared=%+v err=%v", prepared, err)
	}

	// A malformed declaration fails closed instead of being treated as absent.
	writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "my-app", "options": map[string]any{
		"publish": map[string]any{"namespace": "Not A Segment"},
	}})
	if _, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod", "if-present": true}, nil, root, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "canonical native path segment") {
		t.Fatalf("malformed declaration accepted: %v", err)
	}
}

// TestPrepareAuthoredConfigUsesTheWorkspaceProjectPath pins the member's
// project id to the id the framework's release-set plan carries: "/" + the
// project's workspace-relative path, not "/" + its manifest name. Framework
// main run 12669584028f failed both sites' publish~cloud-publish-config with
// "selected Config member belongs to a different project" because the step
// compared the plan's "/sites/putnami.dev" against "/putnami.dev".
//
// A grouping folder such as "(web)" is not part of the project id, as in the
// framework's ProjectIDFromPath.
func TestPrepareAuthoredConfigUsesTheWorkspaceProjectPath(t *testing.T) {
	for _, path := range []string{"sites/putnami.dev", "sites/(web)/putnami.dev"} {
		t.Run(path, func(t *testing.T) { testPrepareAuthoredConfigProjectPath(t, path) })
	}
}

func testPrepareAuthoredConfigProjectPath(t *testing.T, path string) {
	root := t.TempDir()
	writeLinkFile(t, root)
	projectDir := filepath.Join(root, filepath.FromSlash(path))
	for _, dir := range []string{"schema", "conf", ".gen"} {
		mustMkdir(t, filepath.Join(projectDir, dir))
	}
	writeJSONFile(t, filepath.Join(projectDir, "putnami.json"), map[string]any{"name": "putnami.dev", "options": map[string]any{
		"@putnami/cloud:publish-config": map[string]any{"namespace": "putnami"},
	}})
	writeJSONFile(t, filepath.Join(projectDir, "schema", "config.json"), map[string]any{
		"appName": "putnami.dev",
		"configs": []any{map[string]any{"path": "server", "fields": []any{
			map[string]any{"name": "port", "type": "int", "required": true},
		}}},
	})
	if err := os.WriteFile(filepath.Join(projectDir, "conf", "env.prod.yaml"), []byte("server:\n  port: 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(projectDir, ".gen", "version.json"), map[string]any{"version": "1.2.3"})
	stubAuthoredGit(t)

	prepared, err := PrepareAuthoredConfig(map[string]any{"app": "putnami.dev", "env": "prod"}, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ProjectID != "/sites/putnami.dev" || prepared.ProjectPath != "/sites/putnami.dev" {
		t.Fatalf("project id = %q path = %q, want the workspace path /sites/putnami.dev", prepared.ProjectID, prepared.ProjectPath)
	}
	if prepared.Coordinate != "putnami/putnami.dev-config" {
		t.Fatalf("coordinate = %q, want the name-derived putnami/putnami.dev-config", prepared.Coordinate)
	}
	// Control prepares the member for "sites/putnami.dev"; framework main run
	// 752ac0e7a0f3 deployed nothing because Config refused a member naming
	// "putnami.dev" with "invalid prepared configuration request: authored member".
	if got := prepared.Member.Descriptor.Project; got != "sites/putnami.dev" {
		t.Fatalf("member project = %q, want the workspace path sites/putnami.dev", got)
	}
}

func TestMemberProjectKeepsTheRootProjectName(t *testing.T) {
	if got := memberProject("/", "site"); got != "site" {
		t.Fatalf("root member project = %q, want the manifest name", got)
	}
	if got := memberProject("/site", "site"); got != "site" {
		t.Fatalf("top-level member project = %q, want site", got)
	}
}

func TestConfigNamespaceFromOptionsPrecedence(t *testing.T) {
	if _, declared, err := ConfigNamespaceFromOptions(nil); err != nil || declared {
		t.Fatalf("nil options = declared %v err %v", declared, err)
	}
	namespace, declared, err := ConfigNamespaceFromOptions(map[string]map[string]any{
		"publish":                {"namespace": "a"},
		"@putnami/cloud:publish": {"namespace": "b"},
	})
	if err != nil || !declared || namespace != "b" {
		t.Fatalf("precedence = %q %v %v", namespace, declared, err)
	}
	if _, _, err := ConfigNamespaceFromOptions(map[string]map[string]any{"@putnami/cloud": {"namespace": 7}}); err == nil {
		t.Fatal("non-string namespace accepted")
	}
}
