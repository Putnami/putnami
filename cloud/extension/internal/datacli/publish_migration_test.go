package datacli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestPrepareMigrationRequiresExplicitNamespaceForExistingBundle(t *testing.T) {
	root := t.TempDir()
	app := "apps/example-api"
	appDir := filepath.Join(root, filepath.FromSlash(app))
	bundleDir := filepath.Join(appDir, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "payload", "sql", "default", "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{"name": app})
	digest := strings.Repeat("a", 64)
	writeJSONFile(t, filepath.Join(bundleDir, "bundle.json"), map[string]any{
		"protocol": "migration-bundle.v1", "appName": app, "version": "1.2.3", "digest": digest,
	})
	if err := os.WriteFile(filepath.Join(bundleDir, "payload", "sql", "default", "core", "001.up.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PrepareMigration(map[string]any{"app": app}, nil, root, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "explicit canonical --namespace") {
		t.Fatalf("missing namespace err=%v", err)
	}
	prepared, err := PrepareMigration(map[string]any{"app": app, "namespace": "workspace-native"}, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ExpectedCoordinate != "workspace-native/apps-example-api" || prepared.BundleDigest != digest || len(prepared.Files) != 2 {
		t.Fatalf("prepared=%+v files=%d", prepared, len(prepared.Files))
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{
		"name": app, "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "authored-native"}},
	})
	prepared, err = PrepareMigration(map[string]any{"app": app}, nil, root, clicore.IO{})
	if err != nil || prepared.ExpectedCoordinate != "authored-native/apps-example-api" {
		t.Fatalf("authored project namespace: prepared=%+v err=%v", prepared, err)
	}
}

// A plain build writes `"version": null` into the bundle manifest — only a
// selected release fills it in — so the publisher falls back to the version the
// build generated for the project. That fallback must not follow the bundle:
// the staged copy the publisher now prefers sits next to a VERSION file holding
// the WORKSPACE version ("0.0.0"), which would publish every migration under
// one meaningless version.
func TestPrepareMigrationTakesTheProjectVersionWhenTheStagedManifestHasNone(t *testing.T) {
	root := t.TempDir()
	app := "apps/example-api"
	appDir := filepath.Join(root, filepath.FromSlash(app))
	staged := filepath.Join(root, ".putnami", "out", filepath.FromSlash(app), "build", "migration-bundle")
	for _, dir := range []string{filepath.Join(staged, "payload", "sql", "default", "core"), filepath.Join(appDir, ".gen")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{
		"name": app, "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "workspace-native"}},
	})
	writeJSONFile(t, filepath.Join(appDir, ".gen", "version.json"), map[string]any{"version": "0.0.0-20260912065103-5117663b2"})
	writeJSONFile(t, filepath.Join(staged, "bundle.json"), map[string]any{
		"protocol": "migration-bundle.v1", "appName": app, "version": nil, "digest": strings.Repeat("a", 64),
	})
	if err := os.WriteFile(filepath.Join(staged, "payload", "sql", "default", "core", "001.up.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The build's declared output carries the workspace version beside the bundle.
	if err := os.WriteFile(filepath.Join(filepath.Dir(staged), "VERSION"), []byte("0.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareMigration(map[string]any{"app": app}, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Version != "0.0.0-20260912065103-5117663b2" {
		t.Fatalf("version = %q, want the project's generated version, not the workspace VERSION", prepared.Version)
	}
	if prepared.BundleDir != staged {
		t.Fatalf("bundleDir = %q, want the staged copy %q", prepared.BundleDir, staged)
	}
}

func TestMigrationTargetResolvesTheCoordinateBeforeTheBundleExists(t *testing.T) {
	root := t.TempDir()
	app := "apps/example-api"
	appDir := filepath.Join(root, filepath.FromSlash(app))
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{"name": app})
	target, err := ResolveMigrationTarget(map[string]any{"app": app}, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if target.Application != app || target.ProjectPath != "/"+app || !target.BundleMissing() ||
		target.BundleManifestPath() != filepath.Join(target.AppDir, ".gen", "migration-bundle", "bundle.json") {
		t.Fatalf("target=%+v manifest=%s", target, target.BundleManifestPath())
	}
	if coordinate, err := target.Coordinate(map[string]any{}); err != nil || coordinate != "" {
		t.Fatalf("undeclared namespace: coordinate=%q err=%v", coordinate, err)
	}
	if coordinate, err := target.Coordinate(map[string]any{"namespace": "workspace-native"}); err != nil ||
		coordinate != "workspace-native/apps-example-api" {
		t.Fatalf("explicit namespace: coordinate=%q err=%v", coordinate, err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{
		"name": app, "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "Not Canonical"}},
	})
	if _, err := target.Coordinate(map[string]any{}); err == nil {
		t.Fatal("a malformed project namespace produced a coordinate")
	}
	if _, err := target.Prepare(map[string]any{}, clicore.IO{}); err == nil || !strings.Contains(err.Error(), "no migration bundle at") {
		t.Fatalf("missing bundle without --if-present: err=%v", err)
	}
}

func TestPublishPreparedMigrationMakesOneBoundedDataCall(t *testing.T) {
	registryToken := "registry-secret-value"
	workspaceToken := "workspace-secret-value"
	digest := func(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		callCount++
		if request.Method != http.MethodPost || request.URL.Path != "/v1/workspaces/ws-acme/data/migrations/publish" {
			t.Fatalf("request=%s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+workspaceToken || request.Header.Get(migrationRegistryAuthorizationHeader) != "Bearer "+registryToken {
			t.Fatalf("authorization headers were not separated")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["expected_coordinate"] != "workspace-native/apps-example-api" {
			t.Fatalf("body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PublishedMigration{
			MigrationRef: "mig_accepted", BundleDigest: strings.Repeat("a", 64), BlobDigest: digest("b"), ArtifactDigest: digest("c"),
			Member: MigrationMember{Ecosystem: "put", Coordinate: "workspace-native/apps-example-api", Version: "1.2.3", ArtifactDigest: digest("c"), SourceRevision: "revision", SelectionFingerprint: "fingerprint"},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	writeLinkFile(t, root)
	prepared := &PreparedMigration{
		Application: "apps/example-api", ProjectPath: "/apps/example-api", Version: "1.2.3",
		ExpectedCoordinate: "workspace-native/apps-example-api", BundleDigest: strings.Repeat("a", 64), Files: map[string][]byte{"bundle.json": []byte(`{}`)},
	}
	var output []string
	accepted, err := PublishPreparedMigration(
		map[string]any{"control-plane-url": server.URL}, root,
		map[string]string{"PUTNAMI_CLOUD_TOKEN": workspaceToken},
		clicore.IO{Client: server.Client(), Stdout: func(line string) { output = append(output, line) }},
		prepared, MigrationSelection{SourceRevision: "revision", SelectionFingerprint: "fingerprint"}, registryToken,
	)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 || accepted.MigrationRef != "mig_accepted" || len(output) != 1 {
		t.Fatalf("calls=%d accepted=%+v output=%q", callCount, accepted, output)
	}
	joined := strings.Join(output, "\n")
	if strings.Contains(joined, registryToken) || strings.Contains(joined, workspaceToken) {
		t.Fatalf("output leaked a credential: %s", joined)
	}
}

func TestMigrationAcceptancePreservesProtocolDigestEncodings(t *testing.T) {
	bundleDigest := strings.Repeat("a", 64)
	prepared := &PreparedMigration{ExpectedCoordinate: "owner/app", Version: "1", BundleDigest: bundleDigest}
	selection := MigrationSelection{SourceRevision: "revision", SelectionFingerprint: "fingerprint"}
	accepted := PublishedMigration{
		MigrationRef: "mig_accepted", BundleDigest: bundleDigest,
		BlobDigest: "sha256:" + strings.Repeat("b", 64), ArtifactDigest: "sha256:" + strings.Repeat("c", 64),
		Member: MigrationMember{Ecosystem: "put", Coordinate: "owner/app", Version: "1", ArtifactDigest: "sha256:" + strings.Repeat("c", 64), SourceRevision: "revision", SelectionFingerprint: "fingerprint"},
	}
	if err := validateAcceptedMigration(&accepted, prepared, selection); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PublishedMigration){
		"qualified bundle digest":     func(a *PublishedMigration) { a.BundleDigest = "sha256:" + bundleDigest },
		"unqualified blob digest":     func(a *PublishedMigration) { a.BlobDigest = strings.Repeat("b", 64) },
		"blob reuses semantic digest": func(a *PublishedMigration) { a.BlobDigest = "sha256:" + bundleDigest },
		"different bundle":            func(a *PublishedMigration) { a.BundleDigest = strings.Repeat("d", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := accepted
			mutate(&invalid)
			if err := validateAcceptedMigration(&invalid, prepared, selection); err == nil {
				t.Fatal("invalid acceptance was accepted")
			}
		})
	}
}

func TestPublishPreparedMigrationSanitizesRefusal(t *testing.T) {
	secret := "registry-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"`+secret+`"}`)
	}))
	defer server.Close()
	root := t.TempDir()
	writeLinkFile(t, root)
	digest := "sha256:" + strings.Repeat("a", 64)
	_, err := PublishPreparedMigration(
		map[string]any{"control-plane-url": server.URL}, root, map[string]string{"PUTNAMI_CLOUD_TOKEN": "workspace-token"}, clicore.IO{Client: server.Client()},
		&PreparedMigration{Application: "app", Version: "1", ExpectedCoordinate: "owner/app", BundleDigest: digest, Files: map[string][]byte{"bundle.json": []byte(`{}`)}},
		MigrationSelection{SourceRevision: "revision", SelectionFingerprint: "fingerprint"}, secret,
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("sanitized refusal err=%v", err)
	}
}

func TestResolveMigrationTargetPrefersTheStagedBundleOverGen(t *testing.T) {
	root := resolvedTempDir(t)
	appDir := filepath.Join(root, "apps", "data-api")
	staged := filepath.Join(root, ".putnami", "out", "apps/data-api", "build", "migration-bundle")
	for dir, body := range map[string]string{
		filepath.Join(appDir, ".gen", "migration-bundle"): `{"protocol":"migration-bundle.v1","appName":"stale"}`,
		staged: `{"protocol":"migration-bundle.v1","appName":"fresh"}`,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bundle.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(appDir, "putnami.json"), []byte(`{"name":"apps/data-api"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	target, err := ResolveMigrationTarget(map[string]any{"app": "apps/data-api"}, nil, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.BundleDir != staged {
		t.Fatalf("bundle dir = %q, want the staged copy %q", target.BundleDir, staged)
	}
}

func TestResolveMigrationTargetFallsBackToGenWhenNothingIsStaged(t *testing.T) {
	root := resolvedTempDir(t)
	appDir := filepath.Join(root, "apps", "data-api")
	gen := filepath.Join(appDir, ".gen", "migration-bundle")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "bundle.json"), []byte(`{"protocol":"migration-bundle.v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "putnami.json"), []byte(`{"name":"apps/data-api"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	target, err := ResolveMigrationTarget(map[string]any{"app": "apps/data-api"}, nil, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.BundleDir != gen {
		t.Fatalf("bundle dir = %q, want the .gen copy %q", target.BundleDir, gen)
	}
}

func TestResolveMigrationTargetHonorsAnExplicitBundleFrom(t *testing.T) {
	root := resolvedTempDir(t)
	appDir := filepath.Join(root, "apps", "data-api")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "putnami.json"), []byte(`{"name":"apps/data-api"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(root, "elsewhere")

	target, err := ResolveMigrationTarget(map[string]any{"app": "apps/data-api", "bundle-from": explicit}, nil, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.BundleDir != explicit {
		t.Fatalf("bundle dir = %q, want %q", target.BundleDir, explicit)
	}
}

// resolvedTempDir returns a temp dir with symlinks resolved: macOS hands out
// /var/folders/... which resolves to /private/var/..., and the resolver returns
// the resolved form, so an expectation built on the raw path never matches.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// writeMigrationProject writes a project manifest named name at appDir, with an
// authored migration namespace, and a generated bundle whose appName is
// bundleAppName.
func writeMigrationProject(t *testing.T, appDir, name, bundleAppName string) {
	t.Helper()
	bundle := filepath.Join(appDir, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "payload", "sql", "default", "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{
		"name": name, "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "workspace-native"}},
	})
	writeJSONFile(t, filepath.Join(bundle, "bundle.json"), map[string]any{
		"protocol": "migration-bundle.v1", "appName": bundleAppName, "version": "1.2.3", "digest": strings.Repeat("a", 64),
	})
	if err := os.WriteFile(filepath.Join(bundle, "payload", "sql", "default", "core", "001.up.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The publication identity is the project's workspace path; the manifest name
// only labels messages and must equal the bundle's appName. Every
// Cloud project's name is its path, so its output is unchanged. A nested
// project publishes at the path's package, and the workspace-root project
// keeps its name because "/" names no package.
func TestMigrationTargetPublishesUnderTheWorkspacePath(t *testing.T) {
	for _, test := range []struct {
		name, dir, manifestName, application, projectPath, coordinate string
	}{
		{
			name: "name is the path", dir: "apps/example-api", manifestName: "apps/example-api",
			application: "apps/example-api", projectPath: "/apps/example-api", coordinate: "workspace-native/apps-example-api",
		},
		{
			name: "nested project", dir: "sites/putnami.dev", manifestName: "putnami.dev",
			application: "sites/putnami.dev", projectPath: "/sites/putnami.dev", coordinate: "workspace-native/sites-putnami.dev",
		},
		{
			// The plan's project id drops the "(web)" grouping folder; the
			// physical path, which names no Put package, keeps the name's.
			name: "grouping folder", dir: "sites/(web)/example", manifestName: "example",
			application: "example", projectPath: "/sites/example", coordinate: "workspace-native/example",
		},
		{
			name: "workspace root", dir: "", manifestName: "root-app",
			application: "root-app", projectPath: "/", coordinate: "workspace-native/root-app",
		},
		{
			// An uppercase segment maps to no Put package, so the name answers.
			name: "path with no package", dir: "Sites/putnami.dev", manifestName: "putnami.dev",
			application: "putnami.dev", projectPath: "/Sites/putnami.dev", coordinate: "workspace-native/putnami.dev",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := resolvedTempDir(t)
			writeMigrationProject(t, filepath.Join(root, filepath.FromSlash(test.dir)), test.manifestName, test.manifestName)
			target, err := ResolveMigrationTarget(map[string]any{"app": test.manifestName}, nil, root)
			if err != nil {
				t.Fatal(err)
			}
			if target.Name != test.manifestName || target.Application != test.application || target.ProjectPath != test.projectPath {
				t.Fatalf("target name=%q application=%q path=%q, want %q %q %q",
					target.Name, target.Application, target.ProjectPath, test.manifestName, test.application, test.projectPath)
			}
			coordinate, err := target.Coordinate(map[string]any{})
			if err != nil || coordinate != test.coordinate {
				t.Fatalf("coordinate=%q err=%v, want %q", coordinate, err, test.coordinate)
			}
			prepared, err := target.Prepare(map[string]any{}, clicore.IO{})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Application != test.application || prepared.ProjectPath != test.projectPath || prepared.ExpectedCoordinate != test.coordinate {
				t.Fatalf("prepared=%+v, want application %q at %q", prepared, test.application, test.coordinate)
			}
		})
	}
}

// MigrationApplication is the rule the workspace probe shares, so its member
// and the CLI's publication name one package.
func TestMigrationApplicationFallsBackToTheNameWhenThePathNamesNoPackage(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"sites/putnami.dev", "sites/putnami.dev"},
		{"apps/api", "apps/api"},
		{"", "putnami.dev"},
		{"Sites/putnami.dev", "putnami.dev"},
		{"sites/putnami dev", "putnami.dev"},
		{"sites/@scope", "putnami.dev"},
		{"/sites/putnami.dev", "putnami.dev"},
		{"sites/", "putnami.dev"},
		{"sites//putnami.dev", "putnami.dev"},
		{"sites/../x", "putnami.dev"},
		{" sites/putnami.dev", "putnami.dev"},
		{"-sites/putnami.dev", "putnami.dev"},
		{strings.Repeat("a", 256), "putnami.dev"},
	} {
		if got := MigrationApplication(test.path, "putnami.dev"); got != test.want {
			t.Errorf("MigrationApplication(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

// The bundle's appName stays checked locally against the manifest name, so a
// nested project's bundle that carries its path is refused before any call.
func TestPrepareMigrationChecksANestedBundleAgainstTheManifestName(t *testing.T) {
	root := resolvedTempDir(t)
	writeMigrationProject(t, filepath.Join(root, "sites", "putnami.dev"), "putnami.dev", "sites/putnami.dev")
	_, err := PrepareMigration(map[string]any{"app": "putnami.dev"}, nil, root, clicore.IO{})
	if err == nil || !strings.Contains(err.Error(), `appName "sites/putnami.dev", want "putnami.dev"`) {
		t.Fatalf("err=%v, want the bundle appName checked against the manifest name", err)
	}
}

// A nested project sends its path as the application, and the member Data
// returns must sit at the path's coordinate: that member is what the CLI emits
// to the release set, which selected the same coordinate.
func TestPublishNestedMigrationSendsThePathAndKeepsThePathMember(t *testing.T) {
	digest := func(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }
	for _, test := range []struct {
		name, returned string
		wantErr        bool
	}{
		{name: "path member", returned: "workspace-native/sites-putnami.dev"},
		{name: "name member", returned: "workspace-native/putnami.dev", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["application"] != "sites/putnami.dev" || body["expected_coordinate"] != "workspace-native/sites-putnami.dev" {
					t.Fatalf("application=%v coordinate=%v, want the path identity", body["application"], body["expected_coordinate"])
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(PublishedMigration{
					MigrationRef: "mig_accepted", BundleDigest: strings.Repeat("a", 64), BlobDigest: digest("b"), ArtifactDigest: digest("c"),
					Member: MigrationMember{Ecosystem: "put", Coordinate: test.returned, Version: "1.2.3", ArtifactDigest: digest("c"), SourceRevision: "revision", SelectionFingerprint: "fingerprint"},
				})
			}))
			defer server.Close()
			root := resolvedTempDir(t)
			writeLinkFile(t, root)
			writeMigrationProject(t, filepath.Join(root, "sites", "putnami.dev"), "putnami.dev", "putnami.dev")
			prepared, err := PrepareMigration(map[string]any{"app": "putnami.dev"}, nil, root, clicore.IO{})
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := PublishPreparedMigration(
				map[string]any{"control-plane-url": server.URL}, root,
				map[string]string{"PUTNAMI_CLOUD_TOKEN": "workspace-token"}, clicore.IO{Client: server.Client(), Stdout: func(string) {}},
				prepared, MigrationSelection{SourceRevision: "revision", SelectionFingerprint: "fingerprint"}, "registry-token",
			)
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted a member at %s for a project published at its path", test.returned)
				}
				return
			}
			if err != nil || accepted.Member.Coordinate != "workspace-native/sites-putnami.dev" {
				t.Fatalf("accepted=%+v err=%v", accepted, err)
			}
		})
	}
}

func TestPrepareMigrationNamesTheManifestFieldThatDisagrees(t *testing.T) {
	root := t.TempDir()
	app := "apps/auth-server"
	appDir := filepath.Join(root, filepath.FromSlash(app))
	bundle := filepath.Join(appDir, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "payload", "sql", "default", "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{
		"name": app, "options": map[string]any{"@putnami/cloud:publish-migration": map[string]any{"namespace": "cloud"}},
	})
	if err := os.WriteFile(filepath.Join(bundle, "payload", "sql", "default", "core", "001.up.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A writer that fell back to the package.json name instead of the project identity.
	writeJSONFile(t, filepath.Join(bundle, "bundle.json"), map[string]any{
		"protocol": "migration-bundle.v1", "appName": "auth.putnami.cloud", "version": "1.0.0", "digest": strings.Repeat("a", 64),
	})
	_, err := PrepareMigration(map[string]any{"app": app}, nil, root, clicore.IO{})
	if err == nil || !strings.Contains(err.Error(), `appName "auth.putnami.cloud", want "apps/auth-server"`) {
		t.Fatalf("appName mismatch error = %v, want the observed and expected names", err)
	}
	writeJSONFile(t, filepath.Join(bundle, "bundle.json"), map[string]any{
		"protocol": "migration-bundle.v1", "appName": app, "version": "1.0.0", "digest": "sha256:" + strings.Repeat("a", 64),
	})
	_, err = PrepareMigration(map[string]any{"app": app}, nil, root, clicore.IO{})
	if err == nil || !strings.Contains(err.Error(), `digest "sha256:`) {
		t.Fatalf("digest mismatch error = %v, want the observed digest", err)
	}
}

func TestMigrationBundleBuiltReadsBothBundleLocations(t *testing.T) {
	root := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".putnami", "out", "apps", "staged-api", "build", "migration-bundle", "bundle.json"))
	write(filepath.Join(root, "apps", "gen-api", ".gen", "migration-bundle", "bundle.json"))
	if err := os.MkdirAll(filepath.Join(root, "apps", "dir-api", ".gen", "migration-bundle", "bundle.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	for project, want := range map[string]bool{
		"apps/staged-api": true,
		"apps/gen-api":    true,
		"apps/plain-api":  false,
		"apps/dir-api":    false,
	} {
		if got := MigrationBundleBuilt(root, project); got != want {
			t.Errorf("MigrationBundleBuilt(%s) = %v, want %v", project, got, want)
		}
	}
}
