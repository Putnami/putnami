package datacli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	bundlepkg "go.putnami.dev/protocol/migration/bundle"
	"go.putnami.dev/protocol/migration/bundle/publication"
	put "go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	outboxTestApp        = "apps/example-api"
	outboxTestCoordinate = "workspace-native/apps-example-api"
	outboxTestSQL        = "CREATE TABLE example(id text);"
	// outboxTestSQLHash is the bare hex sha256 of outboxTestSQL.
	outboxTestSQLHash = "198dae9245580d56479483a470a43cc73b97ca942e169b74f87a8ec3cead070a"
)

var outboxTestSelection = MigrationSelection{SourceRevision: "revision", SelectionFingerprint: "fingerprint"}

// writeOutboxTestBundle writes one valid generated bundle for outboxTestApp
// and returns the workspace root. The bundle manifest declares its digest, as
// the build writes it.
func writeOutboxTestBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, filepath.FromSlash(outboxTestApp))
	bundleDir := filepath.Join(appDir, ".gen", "migration-bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(appDir, "putnami.json"), map[string]any{"name": outboxTestApp})
	operations := `[{"kind":"sql","target":"default","namespace":"core","name":"001_example","up":{"path":"payload/001.sql","hash":"` + outboxTestSQLHash + `"},"safety":"safe-online"}]`
	// The bundle digest excludes the digest member, so the digest of the bundle
	// without it is the one the manifest declares.
	undeclared := map[string][]byte{
		"bundle.json":     []byte(`{"protocol":"migration-bundle.v1","appName":"` + outboxTestApp + `","operations":` + operations + `}`),
		"payload/001.sql": []byte(outboxTestSQL),
	}
	probe, err := publication.Pack(publication.Input{
		Application: outboxTestApp, Namespace: "probe", Package: "probe", Version: "1", SourceRevision: "r", SelectionFingerprint: "f", Files: undeclared,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"protocol":"migration-bundle.v1","appName":"` + outboxTestApp + `","version":"1.2.3","digest":"` + probe.BundleDigest + `","operations":` + operations + `}`
	if err := os.WriteFile(filepath.Join(bundleDir, "bundle.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "payload", "001.sql"), []byte(outboxTestSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// outboxTestFiles is a bundle for outboxTestApp of the given literal
// operations, each over the one test payload.
func outboxTestFiles(operations ...string) map[string][]byte {
	return map[string][]byte{
		"bundle.json":     []byte(`{"protocol":"migration-bundle.v1","appName":"` + outboxTestApp + `","operations":[` + strings.Join(operations, ",") + `]}`),
		"payload/001.sql": []byte(outboxTestSQL),
	}
}

func prepareOutboxTestMigration(t *testing.T) *PreparedMigration {
	t.Helper()
	prepared, err := PrepareMigration(map[string]any{"app": outboxTestApp, "namespace": "workspace-native"}, nil, writeOutboxTestBundle(t), clicore.IO{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return prepared
}

// packedOutboxMember is one packed member read back the way the engine reads
// it: the descriptor through the protocol parser and validator, the files
// through the SDK reader, which re-hashes each one.
type packedOutboxMember struct {
	member   extensionproto.OutboxMember
	manifest []byte
	blobs    map[string][]byte
}

func readPackedOutboxMember(t *testing.T, outbox string) packedOutboxMember {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outbox, extensionproto.PublicationOutboxDescriptor))
	if err != nil {
		t.Fatalf("read the outbox descriptor: %v", err)
	}
	descriptor, diagnostics := extensionproto.ParsePublicationOutbox(data)
	if len(diagnostics) != 0 {
		t.Fatalf("outbox descriptor is invalid: %v", diagnostics)
	}
	if diagnostics := extensionproto.ValidatePublicationOutbox(descriptor); len(diagnostics) != 0 {
		t.Fatalf("outbox descriptor is invalid: %v", diagnostics)
	}
	if len(descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want exactly the one planned member", descriptor.Members)
	}
	read, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	member := read.Descriptor.Members[0]
	if member.Put == nil || member.NPM != nil || member.Go != nil || member.OCI != nil {
		t.Fatalf("packed member = %+v, want exactly the put block", member)
	}
	manifest, err := read.ReadFile(member.Put.Manifest)
	if err != nil {
		t.Fatalf("read the packed manifest: %v", err)
	}
	blobs := map[string][]byte{}
	for _, blob := range member.Put.Blobs {
		content, err := read.ReadFile(blob.File())
		if err != nil {
			t.Fatalf("read packed blob %s: %v", blob.Path, err)
		}
		blobs[blob.Digest] = content
	}
	return packedOutboxMember{member: member, manifest: manifest, blobs: blobs}
}

// requireEngineAdmitsMigration applies the engine's publication-v1 rules to the
// packed member: the kind the engine derives from the member's publish step,
// its admission for the put ecosystem, the planned identity, and the
// put-write/v1 upload check the upload node runs before it sends anything.
func requireEngineAdmitsMigration(t *testing.T, packed packedOutboxMember, planned releaseset.PlannedMember) {
	t.Helper()
	// The workspace probe declares the migration member's publish step as
	// cloud-publish-migration; the engine derives the kind from it.
	kind := releaseset.KindFor(planned.Ecosystem, "", "cloud-publish-migration")
	if kind != distributionproto.KindMigration {
		t.Fatalf("cloud-publish-migration maps to kind %q, want %q", kind, distributionproto.KindMigration)
	}
	profile, published := put.ProfileFor(kind)
	if !published || packed.member.Ecosystem == extensionproto.OutboxEcosystemArchive {
		t.Fatalf("publication-v1 admits no %s member of kind %q", packed.member.Ecosystem, kind)
	}
	if packed.member.Ecosystem != string(planned.Ecosystem) || packed.member.Coordinate != planned.Coordinate ||
		packed.member.Version != planned.Version || packed.member.Project != planned.ProjectID {
		t.Fatalf("packed member %+v is not the planned member %+v", packed.member, planned)
	}
	if packed.member.Put.MediaType != profile.ManifestMediaType {
		t.Fatalf("packed media type = %q, want the %s profile's %q", packed.member.Put.MediaType, kind, profile.ManifestMediaType)
	}
	member := putpublish.Member{
		Kind: kind, Coordinate: packed.member.Coordinate, Version: packed.member.Version,
		MediaType: packed.member.Put.MediaType, Manifest: packed.manifest,
	}
	for _, blob := range packed.member.Put.Blobs {
		content := packed.blobs[blob.Digest]
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return content, nil },
		})
	}
	if _, err := putpublish.Check(member); err != nil {
		t.Fatalf("the engine's upload check refuses the packed member: %v", err)
	}
}

// The outbox member must be the member Data's direct path stores: Data's
// publisher packs with publication.Pack from the application, the workspace's
// Put namespace, bundlepkg.PackageName(application), the version, the
// selection and the bundle files, and so does this step. Data accepts a stored
// member only when repacking it reproduces these bytes.
func TestPackPreparedMigrationPacksTheBytesDataStores(t *testing.T) {
	prepared := prepareOutboxTestMigration(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	result, err := PackPreparedMigration(outbox, "/"+outboxTestApp, prepared, outboxTestSelection)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	direct, err := publication.Pack(publication.Input{
		Application: prepared.Application, Namespace: "workspace-native", Package: bundlepkg.PackageName(prepared.Application),
		Version: "1.2.3", SourceRevision: "revision", SelectionFingerprint: "fingerprint", Files: prepared.Files,
	})
	if err != nil {
		t.Fatal(err)
	}

	packed := readPackedOutboxMember(t, outbox)
	if !bytes.Equal(packed.manifest, direct.Canonical) || packed.member.Put.Manifest.Digest != direct.ArtifactDigest {
		t.Fatalf("packed manifest %s (%s), Data stores %s (%s)", packed.manifest, packed.member.Put.Manifest.Digest, direct.Canonical, direct.ArtifactDigest)
	}
	if len(packed.member.Put.Blobs) != 1 {
		t.Fatalf("packed blobs = %+v, want the one bundle blob", packed.member.Put.Blobs)
	}
	blob := packed.member.Put.Blobs[0]
	if blob.Digest != direct.BlobDigest || !bytes.Equal(packed.blobs[blob.Digest], direct.Tarball) || blob.Size != int64(len(direct.Tarball)) ||
		blob.MediaType != publication.BlobMediaType {
		t.Fatalf("packed blob %+v differs from the blob Data stores (%s, %d bytes)", blob, direct.BlobDigest, len(direct.Tarball))
	}
	var manifest publication.Manifest
	if err := json.Unmarshal(packed.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.BlobRef != direct.BlobRef || manifest.BundleDigest != prepared.BundleDigest || manifest.Application != outboxTestApp {
		t.Fatalf("packed manifest = %+v", manifest)
	}
	want := PackedMigration{
		Status: "packed", Coordinate: outboxTestCoordinate, Version: "1.2.3",
		BundleDigest: direct.BundleDigest, BlobDigest: direct.BlobDigest, ArtifactDigest: direct.ArtifactDigest,
	}
	if *result != want {
		t.Fatalf("result = %+v, want %+v", *result, want)
	}
	requireEngineAdmitsMigration(t, packed, releaseset.PlannedMember{
		Ecosystem: "put", Coordinate: outboxTestCoordinate, Version: "1.2.3", ProjectID: "/" + outboxTestApp,
	})

	var output []string
	ReportPackedMigration(map[string]any{}, clicore.IO{Stdout: func(line string) { output = append(output, line) }}, result)
	if len(output) != 1 || !strings.Contains(output[0], outboxTestCoordinate+"@1.2.3") {
		t.Fatalf("report = %q", output)
	}
}

func TestPackPreparedMigrationRefusesBeforeTouchingTheOutbox(t *testing.T) {
	prepared := prepareOutboxTestMigration(t)
	cases := map[string]struct {
		outbox    func(string) string
		projectID string
		prepared  func() *PreparedMigration
		selection MigrationSelection
		message   string
	}{
		"no prepared migration": {prepared: func() *PreparedMigration { return nil }, message: "was not prepared"},
		"no provenance":         {selection: MigrationSelection{SourceRevision: "revision"}, message: "exact release provenance"},
		"relative outbox":       {outbox: func(string) string { return "outbox" }, message: "absolute directory"},
		"no project id":         {projectID: "-", message: "plan names no project id"},
		"no coordinate": {prepared: func() *PreparedMigration {
			copy := *prepared
			copy.ExpectedCoordinate = "workspace-native"
			return &copy
		}, message: "no native Put coordinate"},
		"file outside the bundle manifest": {prepared: func() *PreparedMigration {
			copy := *prepared
			copy.Files = map[string][]byte{"unreferenced.secret": []byte("x")}
			for name, data := range prepared.Files {
				copy.Files[name] = data
			}
			return &copy
		}, message: "outside the strict migration manifest"},
		"declared bundle digest": {prepared: func() *PreparedMigration {
			copy := *prepared
			copy.BundleDigest = strings.Repeat("0", 64)
			return &copy
		}, message: "differs from the digest its manifest declares"},
		// The protocol accepts the next two bundles. Data's publisher and its
		// acceptance at deploy refuse them, so the outbox step refuses them too.
		"an operation that is not sql": {prepared: func() *PreparedMigration {
			copy := *prepared
			copy.Files = outboxTestFiles(`{"kind":"document","target":"default","namespace":"core","name":"001_example","up":{"path":"payload/001.sql","hash":"` + outboxTestSQLHash + `"},"safety":"safe-online"}`)
			return &copy
		}, message: "a migration publication accepts only sql operations"},
		"one canonical identity twice": {prepared: func() *PreparedMigration {
			copy := *prepared
			copy.Files = outboxTestFiles(
				`{"kind":"sql","target":"default","namespace":"core","name":"001_example","up":{"path":"payload/001.sql","hash":"`+outboxTestSQLHash+`"},"safety":"safe-online"}`,
				`{"kind":"sql","target":"default","name":"core/001_example","up":{"path":"payload/001.sql","hash":"`+outboxTestSQLHash+`"},"safety":"safe-online"}`)
			return &copy
		}, message: "appears more than once"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			outbox := filepath.Join(t.TempDir(), "outbox")
			if tc.outbox != nil {
				outbox = tc.outbox(outbox)
			}
			projectID := "/" + outboxTestApp
			if tc.projectID == "-" {
				projectID = ""
			}
			candidate := prepared
			if tc.prepared != nil {
				candidate = tc.prepared()
			}
			selection := outboxTestSelection
			if tc.selection != (MigrationSelection{}) {
				selection = tc.selection
			}
			if _, err := PackPreparedMigration(outbox, projectID, candidate, selection); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("PackPreparedMigration error = %v, want %q", err, tc.message)
			}
			if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
				t.Fatalf("a refused pack touched the outbox: %v", statErr)
			}
		})
	}
}

// An outbox holds one job's members once: a second pack into a committed
// outbox is refused rather than appended.
func TestPackPreparedMigrationRefusesACommittedOutbox(t *testing.T) {
	prepared := prepareOutboxTestMigration(t)
	outbox := filepath.Join(t.TempDir(), "outbox")
	if _, err := PackPreparedMigration(outbox, "/"+outboxTestApp, prepared, outboxTestSelection); err != nil {
		t.Fatal(err)
	}
	if _, err := PackPreparedMigration(outbox, "/"+outboxTestApp, prepared, outboxTestSelection); err == nil || !strings.Contains(err.Error(), "already holds a descriptor") {
		t.Fatalf("second pack error = %v", err)
	}
	readPackedOutboxMember(t, outbox)
}
