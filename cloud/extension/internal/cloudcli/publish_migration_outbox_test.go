package cloudcli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	bundlepkg "go.putnami.dev/protocol/migration/bundle"
	"go.putnami.dev/protocol/migration/bundle/publication"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/releaseset"
)

const outboxMigrationApp = "apps/example-api"

// validMigrationBundle writes a generated bundle Data accepts for
// outboxMigrationApp and returns the workspace root and the bundle files.
func validMigrationBundle(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, filepath.FromSlash(outboxMigrationApp))
	bundle := filepath.Join(project, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "putnami.json"), []byte(`{"name":"`+outboxMigrationApp+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sql := []byte("CREATE TABLE example(id text);")
	operations := `[{"kind":"sql","target":"default","namespace":"core","name":"001_example","up":{"path":"payload/001.sql","hash":"198dae9245580d56479483a470a43cc73b97ca942e169b74f87a8ec3cead070a"},"safety":"safe-online"}]`
	// The bundle digest excludes the digest member, so packing the bundle
	// without it yields the digest the build declares.
	probe, err := publication.Pack(publication.Input{
		Application: outboxMigrationApp, Namespace: "probe", Package: "probe", Version: "1", SourceRevision: "r", SelectionFingerprint: "f",
		Files: map[string][]byte{
			"bundle.json":     []byte(`{"protocol":"migration-bundle.v1","appName":"` + outboxMigrationApp + `","operations":` + operations + `}`),
			"payload/001.sql": sql,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"bundle.json":     []byte(`{"protocol":"migration-bundle.v1","appName":"` + outboxMigrationApp + `","version":null,"digest":"` + probe.BundleDigest + `","operations":` + operations + `}`),
		"payload/001.sql": sql,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(bundle, filepath.FromSlash(name)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, files
}

func migrationOutboxPlan() *releaseset.Plan {
	return stablePlan(releaseset.PlannedMember{
		Ecosystem: "put", Coordinate: "workspace-native/apps-example-api", Version: "1.2.3",
		SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
		Selected: true, ProjectID: "/" + outboxMigrationApp,
	})
}

// The migration member's publish step is what makes the engine admit the
// packed member as a migration, whichever language produced the bundle.
func TestCloudMigrationPublishStepMapsToTheMigrationKind(t *testing.T) {
	for _, packageStep := range []string{"describe", "generate"} {
		if got := releaseset.KindFor(cloudMigrationEcosystem, packageStep, cloudMigrationPublishStep); got != distributionproto.KindMigration {
			t.Errorf("KindFor(%s, %s, %s) = %q, want %q", cloudMigrationEcosystem, packageStep, cloudMigrationPublishStep, got, distributionproto.KindMigration)
		}
	}
}

// Under publication-v1 the migration step packs the planned member the bytes
// Data's publisher stores, and calls no registry or Data endpoint, resolves no
// credential and emits no published member.
func TestManagedMigrationPublishPacksIntoThePublicationOutbox(t *testing.T) {
	root, files := validMigrationBundle(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	params := map[string]any{"app": outboxMigrationApp, "namespace": "workspace-native", releaseset.ContextParamName: migrationOutboxPlan()}
	var stdout []string
	err := publishMigration(params, nil, root, map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}, IO{
		Client: refusingOutboxClient(t), Artifact: refusingArtifact(t),
		Stdout: func(line string) { stdout = append(stdout, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	packed, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	if len(packed.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want exactly the planned migration member", packed.Descriptor.Members)
	}
	member := packed.Descriptor.Members[0]
	if member.Ecosystem != extensionproto.OutboxEcosystemPut || member.Coordinate != "workspace-native/apps-example-api" ||
		member.Version != "1.2.3" || member.Project != "/"+outboxMigrationApp || member.Put == nil ||
		member.Put.MediaType != publication.ManifestMediaType || len(member.Put.Blobs) != 1 {
		t.Fatalf("packed member = %+v, want the planned migration member with its bundle blob", member)
	}
	want, err := publication.Pack(publication.Input{
		Application: outboxMigrationApp, Namespace: "workspace-native", Package: bundlepkg.PackageName(outboxMigrationApp), Version: "1.2.3",
		SourceRevision: strings.Repeat("b", 40), SelectionFingerprint: "sha256:" + strings.Repeat("c", 64), Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := packed.ReadFile(member.Put.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := packed.ReadFile(member.Put.Blobs[0].File())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, want.Canonical) || !bytes.Equal(blob, want.Tarball) {
		t.Fatalf("packed manifest %s, want the publication Data stores, %s", manifest, want.Canonical)
	}
	if got := strings.Join(stdout, "\n"); !strings.Contains(got, "Packed Data migration workspace-native/apps-example-api@1.2.3 into the publication outbox") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestMigrationPublishUnderThePublicationOutboxLeavesItUntouched(t *testing.T) {
	root, _ := validMigrationBundle(t)
	for name, tc := range map[string]struct {
		params  map[string]any
		message string
	}{
		"no release-set plan": {
			params:  map[string]any{"app": outboxMigrationApp, "namespace": "workspace-native", "source-revision": "r", "selection-fingerprint": "f"},
			message: "the job carries no plan",
		},
		"dry run": {
			params: map[string]any{"app": outboxMigrationApp, "namespace": "workspace-native", "dry-run": true, releaseset.ContextParamName: migrationOutboxPlan()},
		},
	} {
		t.Run(name, func(t *testing.T) {
			outbox := filepath.Join(t.TempDir(), "outbox")
			var stdout []string
			err := publishMigration(tc.params, nil, root, map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}, IO{
				Client: refusingOutboxClient(t), Artifact: refusingArtifact(t),
				Stdout: func(line string) { stdout = append(stdout, line) },
			})
			if tc.message == "" {
				if err != nil || len(stdout) != 1 || !strings.Contains(stdout[0], "Dry run") {
					t.Fatalf("dry run = %q, %v", stdout, err)
				}
			} else {
				var cliErr *cliError
				if !errors.As(err, &cliErr) || cliErr.Code != ExitUsage || !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("publishMigration error = %v, want a usage refusal %q", err, tc.message)
				}
			}
			if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
				t.Fatalf("the migration step touched the outbox: %v", statErr)
			}
		})
	}
}
