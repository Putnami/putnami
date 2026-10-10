package publication_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	protocolmigration "go.putnami.dev/protocol/migration"
	"go.putnami.dev/protocol/migration/bundle"
	"go.putnami.dev/protocol/migration/bundle/publication"
)

// sampleSQL hashes to samplePayloadHash, the bare hex sha256 the bundle names.
const (
	sampleSQL         = "CREATE TABLE example(id text);"
	samplePayloadHash = "198dae9245580d56479483a470a43cc73b97ca942e169b74f87a8ec3cead070a"
)

// sampleOperation is the one operation of sampleFiles, over its one payload.
const sampleOperation = `{"kind":"sql","target":"default","namespace":"auth","name":"001_example","up":{"path":"payload/001.sql","hash":"` + samplePayloadHash + `"},"safety":"safe-online"}`

// sampleFiles is one valid bundle written as literal bytes, so the packed
// bytes do not depend on how a protocol version encodes a bundle.
func sampleFiles(appName string) map[string][]byte {
	return bundleWith(appName, sampleOperation)
}

// bundleWith writes a bundle of the given literal operations, each over the
// one sample payload.
func bundleWith(appName string, operations ...string) map[string][]byte {
	manifest := `{"protocol":"migration-bundle.v1","appName":"` + appName + `","operations":[` + strings.Join(operations, ",") + `]}`
	return map[string][]byte{"bundle.json": []byte(manifest), "payload/001.sql": []byte(sampleSQL)}
}

func sampleInput() publication.Input {
	return publication.Input{
		Application: "billing/workloads/auth-server", Namespace: "workspace", Package: "billing-workloads-auth-server",
		Version: "1.0.0", SourceRevision: "abc123", SelectionFingerprint: "sel_1", Files: sampleFiles("auth-server"),
	}
}

type memFS map[string][]byte

func (m memFS) WriteFile(name string, data []byte) error {
	m[name] = append([]byte(nil), data...)
	return nil
}

// The packed bytes are content-addressed on every publication path and a
// receiver repacks a stored publication to accept it, so they must never depend
// on map order, the process, or the build. The blob digest is pinned: a change
// to it would make every publication packed before the change unacceptable.
func TestPackPinsTheStoredBytes(t *testing.T) {
	first, err := publication.Pack(sampleInput())
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	again, err := publication.Pack(sampleInput())
	if err != nil {
		t.Fatalf("pack again: %v", err)
	}
	if !bytes.Equal(first.Tarball, again.Tarball) || !bytes.Equal(first.Canonical, again.Canonical) || first.ArtifactDigest != again.ArtifactDigest {
		t.Fatal("the same publication packed to different bytes")
	}

	const wantBlobDigest = "sha256:30470c377bc67be843a9d1bc4b07473ed141f4ee2e0de46bf73b3c0c85e9ceb0"
	if first.BlobDigest != wantBlobDigest || publication.Digest(first.Tarball) != first.BlobDigest {
		t.Fatalf("blob digest = %s (bytes hash to %s), want the pinned %s", first.BlobDigest, publication.Digest(first.Tarball), wantBlobDigest)
	}
	if first.BlobRef != "put/workspace/billing-workloads-auth-server/blobs/"+first.BlobDigest {
		t.Fatalf("blob ref = %q", first.BlobRef)
	}
	if len(first.BundleDigest) != 64 || strings.Trim(first.BundleDigest, "0123456789abcdef") != "" || first.Manifest.BundleDigest != first.BundleDigest {
		t.Fatalf("bundle digest = %q, want the bundle's bare hex digest", first.BundleDigest)
	}

	wantManifest := `{"protocol":"putnami.data.migration.v2","application":"billing/workloads/auth-server","namespace":"workspace",` +
		`"package":"billing-workloads-auth-server","version":"1.0.0","bundle_digest":"` + first.BundleDigest + `",` +
		`"blob_digest":"` + first.BlobDigest + `","blob_ref":"` + first.BlobRef + `","source_revision":"abc123","selection_fingerprint":"sel_1"}`
	if string(first.Canonical) != wantManifest {
		t.Fatalf("manifest payload = %s, want %s", first.Canonical, wantManifest)
	}
	if first.ArtifactDigest != publication.Digest(first.Canonical) ||
		first.ArtifactRef != "put-manifest:workspace/billing-workloads-auth-server@"+first.ArtifactDigest {
		t.Fatalf("artifact digest = %s, ref = %s", first.ArtifactDigest, first.ArtifactRef)
	}

	unpacked := memFS{}
	if err := bundle.Unpack(first.Tarball, unpacked); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	files := sampleFiles("auth-server")
	if len(unpacked) != len(files) {
		t.Fatalf("unpacked %d files, want %d", len(unpacked), len(files))
	}
	for name, data := range files {
		if !bytes.Equal(unpacked[name], data) {
			t.Fatalf("unpacked %s = %q, want %q", name, unpacked[name], data)
		}
	}
}

// The build writes the project's manifest name into the bundle, which differs
// from the workspace path a nested project publishes under. The appName stays
// a label covered by the bundle digest.
func TestPackKeepsTheBundleAppNameAsALabel(t *testing.T) {
	in := sampleInput()
	in.Application, in.Package, in.Files = "sites/putnami.dev", "sites-putnami.dev", sampleFiles("putnami.dev")
	packed, err := publication.Pack(in)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if packed.Manifest.Application != "sites/putnami.dev" || packed.Bundle.AppName != "putnami.dev" {
		t.Fatalf("manifest application = %q, bundle appName = %q", packed.Manifest.Application, packed.Bundle.AppName)
	}
	other, err := publication.Pack(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if packed.BundleDigest == other.BundleDigest {
		t.Fatal("the bundle digest does not cover the appName")
	}
}

func TestPackRefusesAnInvalidPublication(t *testing.T) {
	cases := map[string]func(*publication.Input){
		"application pattern":    func(in *publication.Input) { in.Application = "Identity" },
		"application traversal":  func(in *publication.Input) { in.Application = "billing/../auth" },
		"application empty path": func(in *publication.Input) { in.Application = "billing//auth" },
		"namespace":              func(in *publication.Input) { in.Namespace = "work/space" },
		"package":                func(in *publication.Input) { in.Package = "" },
		"version":                func(in *publication.Input) { in.Version = " 1.0.0" },
		"long version":           func(in *publication.Input) { in.Version = strings.Repeat("1", 256) },
		"source revision":        func(in *publication.Input) { in.SourceRevision = "" },
		"selection":              func(in *publication.Input) { in.SelectionFingerprint = "sel\x00" },
		"no files":               func(in *publication.Input) { in.Files = nil },
		"invalid path":           func(in *publication.Input) { in.Files["../escape.sql"] = []byte("x") },
		"empty file":             func(in *publication.Input) { in.Files["payload/001.sql"] = nil },
		"unreferenced file":      func(in *publication.Input) { in.Files["unreferenced.secret"] = []byte("x") },
		"no appName":             func(in *publication.Input) { in.Files = sampleFiles("") },
		"payload hash":           func(in *publication.Input) { in.Files["payload/001.sql"] = []byte("DROP TABLE example;") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			mutate(&in)
			if packed, err := publication.Pack(in); !errors.Is(err, publication.ErrInvalid) || packed != nil {
				t.Fatalf("Pack = %v, %v; want ErrInvalid", packed, err)
			}
		})
	}
}

// The migration-bundle protocol accepts both bundles below; the publication
// scope does not. Pack refuses them, so a publisher fails before it uploads a
// bundle every receiver would refuse.
func TestPackRefusesABundleOnlyThePublicationScopeRefuses(t *testing.T) {
	qualified := strings.Replace(sampleOperation, `"namespace":"auth","name":"001_example"`, `"name":"auth/001_example"`, 1)
	for name, tc := range map[string]struct {
		files   map[string][]byte
		message string
	}{
		"document operation": {
			files:   bundleWith("auth-server", strings.Replace(sampleOperation, `"kind":"sql"`, `"kind":"document"`, 1)),
			message: "a migration publication accepts only sql operations",
		},
		"one canonical identity twice": {
			files:   bundleWith("auth-server", sampleOperation, qualified),
			message: "default:auth/001_example appears more than once",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if bundle, _, diagnostics := protocolmigration.LoadBundle(memMapFS(tc.files)); bundle == nil {
				t.Fatalf("the protocol refuses the bundle itself: %v", diagnostics)
			}
			in := sampleInput()
			in.Files = tc.files
			packed, err := publication.Pack(in)
			if !errors.Is(err, publication.ErrInvalid) || packed != nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("Pack packed %t, error %v; want ErrInvalid naming %q", packed != nil, err, tc.message)
			}
		})
	}
}

func TestValidatorsMatchThePublisherRules(t *testing.T) {
	for application, want := range map[string]bool{"sites/putnami.dev": true, "putnami.dev": true, "/sites": false, "a..b": false, "": false} {
		if publication.ValidApplication(application) != want {
			t.Fatalf("ValidApplication(%q) = %v", application, !want)
		}
	}
	for part, want := range map[string]bool{"workspace": true, "sites-putnami.dev": true, "a/b": false, "-a": false} {
		if publication.ValidAddressPart(part) != want {
			t.Fatalf("ValidAddressPart(%q) = %v", part, !want)
		}
	}
	if publication.BlobMediaType != bundle.BlobMediaType || publication.ManifestMediaType != "application/vnd.putnami.data.migration.v2+json" {
		t.Fatal("media types drifted from the registry's migration profile")
	}
}

// CheckScope refuses what the protocol already refuses too, so a bundle built
// in memory, never loaded from files, gets the same answer.
func TestCheckScopeRefusesABundleOutsideThePublicationScope(t *testing.T) {
	valid := protocolmigration.BundleOperation{
		Kind: protocolmigration.KindSQL, Target: "default", Namespace: "auth", Name: "001_example",
		Up: protocolmigration.PayloadRef{Path: "payload/001.sql", Hash: samplePayloadHash}, Safety: protocolmigration.SafetySafeOnline,
	}
	bundleOf := func(operations ...protocolmigration.BundleOperation) protocolmigration.Bundle {
		return protocolmigration.Bundle{Protocol: protocolmigration.BundleProtocol, AppName: "auth-server", Operations: operations}
	}
	if err := publication.CheckScope(bundleOf(valid)); err != nil {
		t.Fatalf("valid bundle refused: %v", err)
	}
	events, unsafe, unhashed, qualified := valid, valid, valid, valid
	events.Kind = protocolmigration.KindEvents
	unsafe.Safety = "probably-safe"
	unhashed.Up.Hash = ""
	qualified.Namespace, qualified.Name = "", "auth/001_example"
	for name, candidate := range map[string]protocolmigration.Bundle{
		"no operation":                 bundleOf(),
		"events operation":             bundleOf(events),
		"unknown safety marker":        bundleOf(unsafe),
		"no up payload hash":           bundleOf(unhashed),
		"one canonical identity twice": bundleOf(valid, qualified),
	} {
		if err := publication.CheckScope(candidate); !errors.Is(err, publication.ErrInvalid) {
			t.Errorf("%s: CheckScope = %v, want ErrInvalid", name, err)
		}
	}
}

func TestFrameworkNameStampsOnlyABareName(t *testing.T) {
	for _, tc := range []struct{ namespace, name, want string }{
		{"auth", "001_example", "auth/001_example"},
		{"", "001_example", protocolmigration.DefaultDatasource + "/001_example"},
		{"auth", "billing/001_example", "billing/001_example"},
		{"auth", "", ""},
	} {
		if got := publication.FrameworkName(tc.namespace, tc.name); got != tc.want {
			t.Errorf("FrameworkName(%q, %q) = %q, want %q", tc.namespace, tc.name, got, tc.want)
		}
	}
}

// The checked-in fixture bundle packs to pinned digests. A registry stores
// these bytes and a receiver repacks a stored publication to accept it, so a
// packer change that moves any digest makes every publication packed before it
// unacceptable. The bundle digest is the one testdata/README.md documents.
func TestPackPinsTheFixtureCorpusDigests(t *testing.T) {
	root := filepath.Join("..", "testdata", "valid")
	files := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(root), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		files[name] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	packed, err := publication.Pack(publication.Input{
		Application: "shop/workloads/migration-fixture", Namespace: "workspace", Package: bundle.PackageName("shop/workloads/migration-fixture"),
		Version: "1.0.0", SourceRevision: "0123456789abcdef0123456789abcdef01234567", SelectionFingerprint: "sel_fixture", Files: files,
	})
	if err != nil {
		t.Fatalf("pack the fixture: %v", err)
	}
	const (
		wantBundleDigest   = "4f12fb7c002958eebb90da5d38679d8b794e2522d6d89202f399b4ef955c1955"
		wantBlobDigest     = "sha256:0c5a4a88a758035dcb5b46bc35b74847e8844bbe6156a08d68623e779ba787a4"
		wantArtifactDigest = "sha256:ef19b7d939b579b4b2a70fef250acf8b97fcc8c04a0667a178121716b648dafb"
		wantManifest       = `{"protocol":"putnami.data.migration.v2","application":"shop/workloads/migration-fixture","namespace":"workspace",` +
			`"package":"shop-workloads-migration-fixture","version":"1.0.0","bundle_digest":"` + wantBundleDigest + `","blob_digest":"` + wantBlobDigest + `",` +
			`"blob_ref":"put/workspace/shop-workloads-migration-fixture/blobs/` + wantBlobDigest + `",` +
			`"source_revision":"0123456789abcdef0123456789abcdef01234567","selection_fingerprint":"sel_fixture"}`
	)
	if packed.BundleDigest != wantBundleDigest || packed.BlobDigest != wantBlobDigest || packed.ArtifactDigest != wantArtifactDigest {
		t.Fatalf("fixture packed to bundle %s, blob %s, artifact %s; want the pinned %s, %s, %s",
			packed.BundleDigest, packed.BlobDigest, packed.ArtifactDigest, wantBundleDigest, wantBlobDigest, wantArtifactDigest)
	}
	if string(packed.Canonical) != wantManifest || publication.Digest(packed.Tarball) != wantBlobDigest {
		t.Fatalf("fixture manifest = %s, want %s", packed.Canonical, wantManifest)
	}
}

// memMapFS serves files as a file system, the way Pack hands them to the
// protocol's loader.
func memMapFS(files map[string][]byte) fstest.MapFS {
	mapfs := fstest.MapFS{}
	for name, data := range files {
		mapfs[name] = &fstest.MapFile{Data: data, Mode: 0o600}
	}
	return mapfs
}
