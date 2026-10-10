package cloudcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	workspaceproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestCloudWorkspaceProbeDeclaresArchiveOwnedByItsPublisherAndPackagedByGo(t *testing.T) {
	root := t.TempDir()
	projectPath := "apps/cli"
	projectRoot := filepath.Join(root, filepath.FromSlash(projectPath))
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudProjectMarker), []byte(`{
		"name":"apps/cli","publish":["archives"],"unrelated":true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudExtensionManifest), []byte(`{
		"name":"@putnami/cloud","commands":{},"unrelated":true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{projectPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v, want one archive publisher", result.Projects)
	}
	project := result.Projects[0]
	if project.Path != projectPath || project.SourceName != "" || project.SourceFile != "" || len(project.Dependencies) != 0 {
		t.Fatalf("Cloud probe claimed core/language identity: %+v", project)
	}
	wantFiles := []string{projectPath + "/putnami.extension.json", projectPath + "/putnami.json"}
	if !reflect.DeepEqual(project.WatchedFiles, wantFiles) {
		t.Fatalf("watchedFiles = %v, want %v", project.WatchedFiles, wantFiles)
	}

	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: project.Metadata})
	if err != nil || !found {
		t.Fatalf("strict release-set metadata = (%+v, %v, %v)", metadata, found, err)
	}
	ecosystem := distributionproto.Ecosystem("archive")
	if got, ok := metadata.PublisherFor(ecosystem, "putnami/cloud"); !ok || got != cloudRuntimeIdentity {
		t.Fatalf("publication owner = %q/%v, want %s", got, ok, cloudRuntimeIdentity)
	}
	if got, ok := metadata.PackagePublisherFor(ecosystem, "putnami/cloud"); !ok || got != cloudArchivePackageOwner {
		t.Fatalf("package owner = %q/%v, want %s", got, ok, cloudArchivePackageOwner)
	}
	if got, ok := metadata.PackageStepFor(ecosystem, "putnami/cloud"); !ok || got != cloudArchivePackageStep {
		t.Fatalf("package step = %q/%v, want %s", got, ok, cloudArchivePackageStep)
	}
	if got, ok := metadata.PublishStepFor(ecosystem, "putnami/cloud"); !ok || got != cloudArchivePublishStep {
		t.Fatalf("publish step = %q/%v, want %s", got, ok, cloudArchivePublishStep)
	}
}

// The framework's source extensions declare archive publication through command
// options and omit the extension manifest name until the package step stamps it.
func TestCloudWorkspaceProbeIncludesSourceExtensionsAndTemplates(t *testing.T) {
	for _, tc := range []struct {
		name, project, manifestFile, manifest, coordinate, owner, step string
	}{
		{
			name:         "source extension",
			project:      `{"name":"@putnami/go","options":{"publish":{"archives":true}}}`,
			manifestFile: "putnami.extension.json", manifest: `{}`, coordinate: "putnami/go",
			owner: "@putnami/go", step: "archives",
		},
		{
			name:         "template",
			project:      `{"name":"go-server","publish":["template-archives"],"options":{"publish":{"archives":true}}}`,
			manifestFile: "putnami.template.json", manifest: `{"name":"go-server"}`, coordinate: "putnami/go-server",
			owner: "@putnami/scaffold", step: "template",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(tc.project), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, tc.manifestFile), []byte(tc.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
			if err != nil || len(result.Projects) != 1 {
				t.Fatalf("probe = %+v, err = %v", result, err)
			}
			metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
			if err != nil || !found || len(metadata.Ecosystems) != 1 {
				t.Fatalf("metadata = %+v, found = %v, err = %v", metadata, found, err)
			}
			member := metadata.Ecosystems[0]
			if member.Coordinate != tc.coordinate || member.PackagePublisher != tc.owner || member.PackageStep != tc.step {
				t.Fatalf("member = %+v", member)
			}
			if !slices.Contains(result.Projects[0].WatchedFiles, tc.manifestFile) {
				t.Fatalf("manifest is not watched: %v", result.Projects[0].WatchedFiles)
			}
		})
	}
}

// A content-only extension declares agentContent in its manifest and carries
// no Go module: @putnami/scaffold packages it under its agent-content step.
// Announcing the Go packager instead leaves the member without the package
// step the release-set coordinator requires, so the extension never publishes.
// A Go extension that also declares agentContent is the reverse case: scaffold
// never plans a step for it, and the Go archives step stages the content.
func TestCloudWorkspaceProbeRoutesContentOnlyExtensionsToScaffold(t *testing.T) {
	for _, tc := range []struct {
		name, project, manifest, goModule, coordinate, owner, step string
	}{
		{
			name:       "a Go extension with agent content stays with the Go packager",
			project:    `{"name":"@putnami/intelligence","publish":["archives"],"options":{"package":{"archives":true}}}`,
			manifest:   `{"name":"@putnami/intelligence","cliContract":5,"agentContent":{"path":"content","source":"agent-src"}}`,
			goModule:   "module example.com/intelligence/cli\n",
			coordinate: "putnami/intelligence", owner: "@putnami/go", step: "archives",
		},
		{
			name: "content-only extension",
			project: `{"name":"@putnami/contributor","extensions":["@putnami/scaffold"],
				"options":{"publish":{"archives":true},"agent-artifact":{"requiredSkills":["plan"]}}}`,
			manifest:   `{"name":"@putnami/contributor","cliContract":5,"agentContent":{"path":"content"}}`,
			coordinate: "putnami/contributor", owner: "@putnami/scaffold", step: "agent-content",
		},
		{
			name:       "a null agentContent is no content",
			project:    `{"name":"@putnami/github-collaboration","options":{"publish":{"archives":true}}}`,
			manifest:   `{"name":"@putnami/github-collaboration","agentContent":null}`,
			coordinate: "putnami/github-collaboration", owner: "@putnami/go", step: "archives",
		},
		{
			name:       "an extension without content stays with the Go packager",
			project:    `{"name":"@putnami/memory-store","options":{"publish":{"archives":true}}}`,
			manifest:   `{"name":"@putnami/memory-store","commands":{}}`,
			coordinate: "putnami/memory-store", owner: "@putnami/go", step: "archives",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(tc.project), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, cloudExtensionManifest), []byte(tc.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			want := []string{cloudExtensionManifest, cloudProjectMarker}
			if tc.goModule != "" {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(tc.goModule), 0o600); err != nil {
					t.Fatal(err)
				}
				// The Go module decides the route, so a change to it re-probes.
				want = []string{"go.mod", cloudExtensionManifest, cloudProjectMarker}
			}
			result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
			if err != nil || len(result.Projects) != 1 {
				t.Fatalf("probe = %+v, err = %v", result, err)
			}
			metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
			if err != nil || !found || len(metadata.Ecosystems) != 1 {
				t.Fatalf("metadata = %+v, found = %v, err = %v", metadata, found, err)
			}
			member := metadata.Ecosystems[0]
			if member.Ecosystem != cloudArchiveEcosystem || member.Coordinate != tc.coordinate {
				t.Fatalf("member = %+v, want %s in %s", member, tc.coordinate, cloudArchiveEcosystem)
			}
			// Literals, not the constants the probe emits: the step id is a
			// contract with @putnami/scaffold's package command, and renaming
			// the constant must not be able to keep this test green.
			if member.PackagePublisher != tc.owner || member.PackageStep != tc.step || member.PublishStep != "cloud-publish-archives" {
				t.Fatalf("member route = %+v, want %s/%s/cloud-publish-archives", member, tc.owner, tc.step)
			}
			if !reflect.DeepEqual(result.Projects[0].WatchedFiles, want) {
				t.Fatalf("watched files = %v, want %v", result.Projects[0].WatchedFiles, want)
			}
		})
	}
}

// An archive project with neither a binary name nor an extension manifest has
// no authored coordinate. The agent-artifact option no longer names one: the
// framework installs agent content only through an extension.
func TestCloudWorkspaceProbeDoesNotAnnounceALegacyAgentArtifactProject(t *testing.T) {
	root := t.TempDir()
	project := `{"name":"@putnami/agent-workflows","extensions":["@putnami/scaffold"],"publish":["archives"],
		"options":{"publish":{"archives":true},"agent-artifact":{"forbiddenContent":[],"requiredSkills":[]}}}`
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("Cloud announced an archive without an authored source: %+v", result.Projects)
	}
}

func TestArchiveIntentFollowsCommandOptionPrecedence(t *testing.T) {
	p := cloudProjectProbe{Publish: []string{"archives"}, Options: map[string]map[string]any{
		"publish": {"archives": true}, cloudRuntimeIdentity + ":publish": {"archives": false},
	}}
	if enabled, err := p.publishesArchives(); err != nil || enabled {
		t.Fatalf("disabled archive intent = %v, %v", enabled, err)
	}
	p.Options[cloudRuntimeIdentity+":publish"]["archives"] = "yes"
	if _, err := p.publishesArchives(); err == nil {
		t.Fatal("invalid archive intent accepted")
	}
}

func TestCloudWorkspaceProbeTransportIsReservedStrictAndBounded(t *testing.T) {
	root := t.TempDir()
	writeArchiveProbeProject(t, root, ".", "@putnami/cloud")
	request := workspaceproto.ProbeRequest{
		Version:   workspaceproto.ProbeProtocolVersion,
		Extension: cloudRuntimeIdentity,
		Paths:     []string{"."},
	}
	result, err := probeCloudWorkspace(root, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, diagnostics := workspaceproto.ParseAndValidateProbeResult(mustJSONBytes(t, result)); len(diagnostics) != 0 {
		t.Fatalf("probe contract diagnostics = %+v", diagnostics)
	}

	var stdin, stdout bytes.Buffer
	if err := workspaceproto.EncodeProbeRequest(&stdin, request); err != nil {
		t.Fatal(err)
	}
	handled, err := handleWorkspaceProbeAt(root, workspaceproto.ProbeControlArgs(), &stdin, &stdout)
	if err != nil || !handled {
		t.Fatalf("HandleWorkspaceProbe = (%v, %v)", handled, err)
	}
	parsed, diagnostics := workspaceproto.ParseAndValidateProbeResult(stdout.Bytes())
	if len(diagnostics) != 0 || parsed.Extension != cloudRuntimeIdentity || len(parsed.Projects) != 1 {
		t.Fatalf("transport result = %+v diagnostics=%+v", parsed, diagnostics)
	}

	var ordinary bytes.Buffer
	if handled, err := HandleWorkspaceProbe([]string{"publish-archives"}, strings.NewReader("{}"), &ordinary); handled || err != nil || ordinary.Len() != 0 {
		t.Fatalf("ordinary command handled=%v err=%v stdout=%q", handled, err, ordinary.String())
	}
}

func TestCloudWorkspaceProbeRejectsArchiveWithoutCanonicalScopedIdentity(t *testing.T) {
	for _, identity := range []string{"", "putnami-cloud", "@Putnami/cloud", " @putnami/cloud"} {
		t.Run(identity, func(t *testing.T) {
			root := t.TempDir()
			writeArchiveProbeProject(t, root, ".", identity)
			_, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
			if err == nil || !strings.Contains(err.Error(), "canonical scoped package") {
				t.Fatalf("error = %v, want canonical scoped identity refusal", err)
			}
		})
	}
}

func TestCloudWorkspaceProbeUsesThePublishArtifactForAPlainCLIProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{
		"name":"@putnami/cli",
		"publish":["archives"],
		"options":{"publish":{"archives":true,"binary-name":"putnami"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 || !reflect.DeepEqual(result.Projects[0].WatchedFiles, []string{cloudProjectMarker}) {
		t.Fatalf("plain CLI archive project = %+v", result.Projects)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
	if err != nil || !found {
		t.Fatalf("release metadata = (%+v, %v, %v)", metadata, found, err)
	}
	if _, ok := metadata.PublisherFor(cloudArchiveEcosystem, "putnami/cli"); !ok {
		t.Fatalf("binary-name putnami did not use the native CLI coordinate: %+v", metadata)
	}
}

func TestCloudWorkspaceProbeDoesNotGuessAPlainArchiveCoordinate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{"publish":["archives"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("Cloud guessed an archive coordinate without an authored source: %+v", result.Projects)
	}
}

func TestCloudWorkspaceProbeDeclaresMigrationFromLanguagePackageProducer(t *testing.T) {
	for _, tc := range []struct{ owner, step string }{{"@putnami/go", "describe"}, {"@putnami/typescript", "generate"}} {
		t.Run(tc.owner, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{
		"name":"telemetry.putnami.dev",
		"extensions":["`+tc.owner+`"],
		"options":{"@putnami/cloud:publish-migration":{"namespace":"putnami"}}
	}`), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Projects) != 1 {
				t.Fatalf("projects = %+v", result.Projects)
			}
			metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
			if err != nil || !found {
				t.Fatalf("metadata = %+v, %v, %v", metadata, found, err)
			}
			coordinate := "putnami/telemetry.putnami.dev"
			if owner, ok := metadata.PackagePublisherFor(cloudMigrationEcosystem, coordinate); !ok || owner != tc.owner {
				t.Fatalf("package owner = %q/%v", owner, ok)
			}
			if step, ok := metadata.PackageStepFor(cloudMigrationEcosystem, coordinate); !ok || step != tc.step {
				t.Fatalf("package step = %q/%v; want %q for %s", step, ok, tc.step, tc.owner)
			}
			if step, ok := metadata.PublishStepFor(cloudMigrationEcosystem, coordinate); !ok || step != cloudMigrationPublishStep {
				t.Fatalf("publish step = %q/%v", step, ok)
			}
		})
	}
}

// The member's package comes from datacli.MigrationApplication, the rule the
// CLI publishes the migration under, so both name one package: the path's
// when it maps to a native Put package, else the manifest name's.
func TestCloudWorkspaceProbeNamesANestedMigrationByItsPath(t *testing.T) {
	for _, test := range []struct{ name, dir, member, other string }{
		{"a path with a package", "sites/putnami.dev", "putnami/sites-putnami.dev", "putnami/putnami.dev"},
		{"a path with no package", "Sites/putnami.dev", "putnami/putnami.dev", "putnami/Sites-putnami.dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, filepath.FromSlash(test.dir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, cloudProjectMarker), []byte(`{
				"name":"putnami.dev",
				"extensions":["@putnami/typescript"],
				"options":{"@putnami/cloud:publish-migration":{"namespace":"putnami"}}
			}`), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{test.dir}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Projects) != 1 {
				t.Fatalf("projects = %+v", result.Projects)
			}
			metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
			if err != nil || !found {
				t.Fatalf("metadata = %+v, %v, %v", metadata, found, err)
			}
			if _, ok := metadata.PublishStepFor(cloudMigrationEcosystem, test.member); !ok {
				t.Fatalf("the migration member is not %s: %+v", test.member, metadata)
			}
			if _, ok := metadata.PublishStepFor(cloudMigrationEcosystem, test.other); ok {
				t.Fatalf("the migration member is still %s", test.other)
			}
		})
	}
}

func TestCloudWorkspaceProbeRejectsMigrationWithoutBundleProducer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{
		"name":"unknown-app",
		"extensions":["@putnami/python"],
		"options":{"@putnami/cloud:publish-migration":{"namespace":"putnami"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}}); err == nil || !strings.Contains(err.Error(), "exactly one migration-bundle language publisher") {
		t.Fatalf("migration without a bundle producer: %v", err)
	}
}

func TestMigrationPackageProducerRefusesAmbiguousLanguages(t *testing.T) {
	project := cloudProjectProbe{Extensions: []string{"@putnami/go", "@putnami/typescript"}}
	if _, _, err := project.migrationPackageProducer(); err == nil {
		t.Fatal("ambiguous migration bundle producers were accepted")
	}
}

func TestCloudWorkspaceProbeDeclaresAuthoredConfigWithExplicitNamespace(t *testing.T) {
	root := t.TempDir()
	projectPath := "apps/example-api"
	projectRoot := filepath.Join(root, filepath.FromSlash(projectPath))
	if err := os.MkdirAll(filepath.Join(projectRoot, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudProjectMarker), []byte(`{
		"name":"apps/example-api",
		"options":{"@putnami/cloud:publish-config":{"namespace":"workspace-native"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "schema", "config.json"), []byte(`{"appName":"apps/example-api","configs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "conf", "env.prod.yaml"), []byte("server:\n  port: 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".gen", "version.json"), []byte(`{"version":"1.2.3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "schema", "config-authored-fields.json"), []byte(`{"fields":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{projectPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v", result.Projects)
	}
	wantFiles := []string{
		projectPath + "/.gen/version.json", projectPath + "/conf/env.prod.yaml",
		projectPath + "/putnami.json", projectPath + "/schema/config-authored-fields.json", projectPath + "/schema/config.json",
	}
	if !reflect.DeepEqual(result.Projects[0].WatchedFiles, wantFiles) {
		t.Fatalf("watched files = %v, want %v", result.Projects[0].WatchedFiles, wantFiles)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
	if err != nil || !found {
		t.Fatalf("metadata = %+v, %v, %v", metadata, found, err)
	}
	coordinate := "workspace-native/apps-example-api-config"
	if owner, ok := metadata.PackagePublisherFor(cloudConfigEcosystem, coordinate); !ok || owner != cloudConfigPackageOwner {
		t.Fatalf("package owner = %q/%v", owner, ok)
	}
	if step, ok := metadata.PackageStepFor(cloudConfigEcosystem, coordinate); !ok || step != cloudConfigPackageStep {
		t.Fatalf("package step = %q/%v", step, ok)
	}
	if step, ok := metadata.PublishStepFor(cloudConfigEcosystem, coordinate); !ok || step != cloudConfigPublishStep {
		t.Fatalf("publish step = %q/%v", step, ok)
	}
}

func TestCloudWorkspaceProbeDeclaresOneSiteContentMemberPerSection(t *testing.T) {
	root := t.TempDir()
	projectPath := "docs/example.test"
	projectRoot := filepath.Join(root, filepath.FromSlash(projectPath))
	for _, section := range []string{"platform", "guides"} {
		if err := os.MkdirAll(filepath.Join(projectRoot, section), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectRoot, section, "index.md"), []byte("# "+section+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudProjectMarker), []byte(`{"name":"docs/example.test","publish":["site-content"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("# guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{projectPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v", result.Projects)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
	if err != nil || !found || len(metadata.Ecosystems) != 2 {
		t.Fatalf("metadata = %+v, %v, %v; want two members", metadata, found, err)
	}
	for _, coordinate := range []string{"cloud/doc-contents-guides", "cloud/doc-contents-platform"} {
		if owner, ok := metadata.PackagePublisherFor(cloudSiteContentEcosystem, coordinate); !ok || owner != cloudSiteContentPackageOwner {
			t.Fatalf("%s package owner = %q/%v", coordinate, owner, ok)
		}
		if step, ok := metadata.PackageStepFor(cloudSiteContentEcosystem, coordinate); !ok || step != cloudSiteContentPackageStep {
			t.Fatalf("%s package step = %q/%v", coordinate, step, ok)
		}
		if step, ok := metadata.PublishStepFor(cloudSiteContentEcosystem, coordinate); !ok || step != cloudSiteContentPublishStep {
			t.Fatalf("%s publish step = %q/%v", coordinate, step, ok)
		}
	}
}

func TestCloudWorkspaceProbeDoesNotDeclareSiteContentWithoutThePublishEntry(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "docs", "example.test")
	if err := os.MkdirAll(filepath.Join(projectRoot, "platform"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudProjectMarker), []byte(`{"name":"docs/example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"docs/example.test"}})
	if err != nil || len(result.Projects) != 0 {
		t.Fatalf("result = %+v, %v; want no declaration", result, err)
	}
}

func TestPublishedSiteContentMemberUsesTheNativeRuntimeArtifactEnvelope(t *testing.T) {
	var output []string
	runtimeIO := putnamiEventIO(IO{Stdout: func(line string) { output = append(output, line) }}, &eventState{}, runtimeproto.MaxKnownProtocolVersion)
	err := emitSiteContentPublishedMember(runtimeIO, distributioncli.SiteContentMemberResult{
		Coordinate: "cloud/doc-contents-platform", Version: "0.0.0-20260911000000-0123456",
		ArtifactDigest: "sha256:" + strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 {
		t.Fatalf("runtime output = %q, want one artifact", output)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(output[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "artifact" || event["kind"] != extensionproto.PublishedMemberEventKind ||
		event["id"] != "site-content-cloud-doc-contents-platform" || event["ecosystem"] != "put" || event["coordinate"] != "cloud/doc-contents-platform" {
		t.Fatalf("artifact envelope = %+v", event)
	}
	if err := emitSiteContentPublishedMember(runtimeIO, distributioncli.SiteContentMemberResult{Coordinate: "cloud/doc-contents-platform"}); err == nil {
		t.Fatal("an incomplete publication result must be refused before emission")
	}
}

func TestCloudWorkspaceProbeDoesNotInventConfigNamespace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{"name":"my-app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schema", "config.json"), []byte(`{"appName":"my-app","configs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("probe invented Config authority: %+v", result.Projects)
	}
}

func TestCloudWorkspaceProbeUsesFrameworkOptionPrecedenceAndRejectsInvalidOverrides(t *testing.T) {
	root := t.TempDir()
	writeArchiveProbeProject(t, root, ".", "@putnami/wrong")
	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{
		"publish":["archives"],
		"options":{
			"publish":{"binary-name":"putnami"},
			"@putnami/cloud":{"binary-name":"putnami-cloud"},
			"@putnami/cloud:publish":{"binary-name":"@acme/final"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
	if err != nil || !found {
		t.Fatalf("release metadata = (%+v, %v, %v)", metadata, found, err)
	}
	if _, ok := metadata.PublisherFor(cloudArchiveEcosystem, "acme/final"); !ok {
		t.Fatalf("extension-command override did not win: %+v", metadata)
	}

	if err := os.WriteFile(filepath.Join(root, cloudProjectMarker), []byte(`{
		"publish":["archives"],"options":{"publish":{"binary-name":" putnami"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{"."}}); err == nil || !strings.Contains(err.Error(), "canonical string") {
		t.Fatalf("invalid binary-name error = %v", err)
	}
}

func TestPublishedArchiveUsesTheNativeRuntimeArtifactEnvelope(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	platformDigest := "sha256:" + strings.Repeat("b", 64)
	var output []string
	runtimeIO := putnamiEventIO(IO{Stdout: func(line string) { output = append(output, line) }}, &eventState{}, runtimeproto.MaxKnownProtocolVersion)
	err := emitArchivePublishedMember(runtimeIO, &distributioncli.ArchivePublishResult{
		Package: "putnami/cloud", Version: "1.2.3", ArtifactDigest: digest,
		Platforms: map[string]string{"linux/amd64": platformDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 {
		t.Fatalf("runtime output = %q, want one artifact", output)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(output[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "artifact" || event["kind"] != extensionproto.PublishedMemberEventKind || event["id"] != "archive-putnami-cloud" || event["name"] != "putnami/cloud" {
		t.Fatalf("artifact envelope = %+v", event)
	}
	platforms, ok := event["platforms"].(map[string]any)
	if !ok {
		t.Fatalf("platforms = %#v", event["platforms"])
	}
	member := extensionproto.PublishedMember{
		Ecosystem: event["ecosystem"].(string), Coordinate: event["coordinate"].(string),
		Version: event["version"].(string), ArtifactDigest: event["artifactDigest"].(string),
		Platforms: map[string]string{"linux/amd64": platforms["linux/amd64"].(string)},
	}
	if diagnostics := extensionproto.ValidatePublishedMember(&member); len(diagnostics) != 0 {
		t.Fatalf("published member diagnostics = %+v", diagnostics)
	}
}

func TestPublishedArchiveSkipsWithoutRuntimeAndPropagatesEmitterFailure(t *testing.T) {
	published := &distributioncli.ArchivePublishResult{
		Package: "putnami/cloud", Version: "1.2.3",
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if err := emitArchivePublishedMember(IO{}, published); err != nil {
		t.Fatal(err)
	}
	want := "artifact sink failed"
	err := emitArchivePublishedMember(IO{Artifact: func(string, string, string, string, map[string]any) error {
		return errors.New(want)
	}}, published)
	if err == nil || err.Error() != want {
		t.Fatalf("emitter error = %v, want %q", err, want)
	}

	called := false
	err = emitArchivePublishedMember(IO{Artifact: func(string, string, string, string, map[string]any) error {
		called = true
		return nil
	}}, &distributioncli.ArchivePublishResult{Package: "putnami/cloud", Version: "1.2.3"})
	if err == nil || called {
		t.Fatalf("invalid publication result error=%v called=%v, want refusal before emission", err, called)
	}
}

func writeArchiveProbeProject(t *testing.T, root, projectPath, identity string) {
	t.Helper()
	projectRoot := root
	if projectPath != "." {
		projectRoot = filepath.Join(root, filepath.FromSlash(projectPath))
	}
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudProjectMarker), []byte(`{"publish":["archives"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, cloudExtensionManifest), []byte(`{"name":`+strconv.Quote(identity)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
