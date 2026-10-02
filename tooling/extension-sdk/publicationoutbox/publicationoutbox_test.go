package publicationoutbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

func writeNPMOutbox(t *testing.T) (string, extproto.OutboxMember) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "outbox")
	writer, err := NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	tarball, err := writer.WriteFile("npm/web/web-1.2.3.tgz", []byte("tarball bytes"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "package.json")
	if err := os.WriteFile(source, []byte(`{"name":"@acme/web","version":"1.2.3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := writer.CopyFile("npm/web/package.json", source)
	if err != nil {
		t.Fatal(err)
	}
	member := extproto.OutboxMember{
		Ecosystem: extproto.OutboxEcosystemNPM, Coordinate: "@acme/web", Version: "1.2.3", Project: "/typescript/web",
		NPM: &extproto.OutboxNPM{Registry: "https://npm.acme.dev", Tarball: tarball, Manifest: manifest},
	}
	if err := writer.Add(member); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	return root, member
}

func TestOutboxWriterAndReaderAgree(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "the-engine-uploads-only-bytes-whose-digest-it-verified")
	root, member := writeNPMOutbox(t)
	outbox, err := Read(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(outbox.Descriptor.Members) != 1 || outbox.Descriptor.Members[0].Coordinate != "@acme/web" {
		t.Fatalf("descriptor = %+v", outbox.Descriptor)
	}
	data, err := outbox.ReadFile(member.NPM.Tarball)
	if err != nil || string(data) != "tarball bytes" {
		t.Fatalf("tarball = %q, %v", data, err)
	}
	if member.NPM.Tarball.Size != int64(len("tarball bytes")) || !strings.HasPrefix(member.NPM.Tarball.Digest, "sha256:") {
		t.Fatalf("tarball entry = %+v", member.NPM.Tarball)
	}
	if _, err := outbox.ReadFile(member.NPM.Manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "npm"))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("outbox directory mode = %v, %v; want 0700", info.Mode().Perm(), err)
		}
	}
}

func TestOutboxReaderRefusesATamperedArtifact(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "the-engine-uploads-only-bytes-whose-digest-it-verified")
	cases := map[string]func(t *testing.T, root string){
		"same size, other bytes": func(t *testing.T, root string) {
			writeOver(t, filepath.Join(root, "npm/web/web-1.2.3.tgz"), "tarball byteZ")
		},
		"longer": func(t *testing.T, root string) {
			writeOver(t, filepath.Join(root, "npm/web/web-1.2.3.tgz"), "tarball bytes and more")
		},
		"shorter": func(t *testing.T, root string) {
			writeOver(t, filepath.Join(root, "npm/web/web-1.2.3.tgz"), "tarball")
		},
	}
	if runtime.GOOS != "windows" {
		cases["replaced by a symbolic link"] = func(t *testing.T, root string) {
			outside := filepath.Join(t.TempDir(), "outside.tgz")
			writeOver(t, outside, "tarball bytes")
			target := filepath.Join(root, "npm/web/web-1.2.3.tgz")
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}
		cases["a symbolic link directory"] = func(t *testing.T, root string) {
			outside := t.TempDir()
			writeOver(t, filepath.Join(outside, "web-1.2.3.tgz"), "tarball bytes")
			if err := os.RemoveAll(filepath.Join(root, "npm/web")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "npm/web")); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			root, member := writeNPMOutbox(t)
			outbox, err := Read(root)
			if err != nil {
				t.Fatal(err)
			}
			tamper(t, root)
			if data, err := outbox.ReadFile(member.NPM.Tarball); err == nil {
				t.Fatalf("tampered artifact read as %q", data)
			}
		})
	}
}

func writeOver(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// npmDescriptor is a descriptor of one npm member that names registry and
// whose tarball is at tarball.
func npmDescriptor(registry, tarball string) string {
	return `{"protocolVersion":1,"members":[{"ecosystem":"npm","coordinate":"a","version":"1","project":"/p","npm":{` +
		`"registry":"` + registry + `",` +
		`"tarball":{"path":"` + tarball + `","digest":"sha256:` + strings.Repeat("a", 64) + `","size":1},` +
		`"manifest":{"path":"package.json","digest":"sha256:` + strings.Repeat("b", 64) + `","size":1}}}]}`
}

func TestOutboxReaderRefusesAnInvalidDescriptor(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "the-descriptor-is-strict-and-bounded")
	for name, content := range map[string]string{
		"unknown member":            `{"protocolVersion":1,"members":[],"registry":"https://evil.example"}`,
		"oversized":                 `{"protocolVersion":1,"members":[]}` + strings.Repeat(" ", extproto.MaxPublicationOutboxBytes),
		"escaping path":             npmDescriptor("https://npm.acme.dev", "../a.tgz"),
		"credentialed npm registry": npmDescriptor("https://publisher:secret@npm.acme.dev", "a.tgz"),
		"cleartext npm registry":    npmDescriptor("http://npm.acme.dev", "a.tgz"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeOver(t, filepath.Join(root, extproto.PublicationOutboxDescriptor), content)
			if _, err := Read(root); err == nil {
				t.Fatal("invalid descriptor read")
			}
		})
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Fatal("an outbox without a descriptor read")
	}
}

func TestOutboxWriterRefusesUnwrittenAndOverlappingArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "outbox")
	writer, err := NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	tarball, err := writer.WriteFile("npm/web.tgz", []byte("tgz"))
	if err != nil {
		t.Fatal(err)
	}
	for name, rel := range map[string]string{"same path": "npm/web.tgz", "nested path": "npm/web.tgz/x", "escaping path": "../x", "descriptor": "outbox.json"} {
		if _, err := writer.WriteFile(rel, []byte("x")); err == nil {
			t.Fatalf("%s: wrote %q", name, rel)
		}
	}
	forged := tarball
	forged.Digest = "sha256:" + strings.Repeat("a", 64)
	member := extproto.OutboxMember{
		Ecosystem: extproto.OutboxEcosystemNPM, Coordinate: "web", Version: "1.0.0", Project: "/web",
		NPM: &extproto.OutboxNPM{Registry: "https://npm.acme.dev", Tarball: forged, Manifest: tarball},
	}
	if err := writer.Add(member); err == nil {
		t.Fatal("added a member whose digest the writer did not compute")
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("commit an empty outbox: %v", err)
	}
	if err := writer.Commit(); err == nil {
		t.Fatal("committed twice")
	}
	if _, err := NewWriter(root); err == nil {
		t.Fatal("opened a writer on a committed outbox")
	}
	outbox, err := Read(root)
	if err != nil || len(outbox.Descriptor.Members) != 0 {
		t.Fatalf("empty outbox = %+v, %v", outbox, err)
	}
	if _, err := NewWriter("relative/outbox"); err == nil {
		t.Fatal("opened a writer on a relative path")
	}
}

func TestOutboxWriterPacksAPutMember(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "the-engine-uploads-only-bytes-whose-digest-it-verified")
	root := filepath.Join(t.TempDir(), "outbox")
	writer, err := NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := writer.WriteFile("put/orders/bundle.tar", []byte("bundle bytes"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := writer.WriteFile("put/orders/manifest.json", []byte(`{"blob_digest":"`+bundle.Digest+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	blob := extproto.OutboxPutBlob{Path: bundle.Path, Digest: bundle.Digest, Size: bundle.Size, MediaType: "application/vnd.putnami.migration-bundle.v1.tar"}
	member := extproto.OutboxMember{
		Ecosystem: extproto.OutboxEcosystemPut, Coordinate: "acme/orders-db", Version: "0007", Project: "/data/orders",
		Put: &extproto.OutboxPut{MediaType: "application/vnd.putnami.data.migration.v2+json", Manifest: manifest, Blobs: []extproto.OutboxPutBlob{blob}},
	}
	forged := member
	forgedBlob := blob
	forgedBlob.Size++
	forged.Put = &extproto.OutboxPut{MediaType: member.Put.MediaType, Manifest: manifest, Blobs: []extproto.OutboxPutBlob{forgedBlob}}
	if err := writer.Add(forged); err == nil {
		t.Fatal("added a put member whose blob size the writer did not compute")
	}
	if err := writer.Add(member); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	outbox, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	packed := outbox.Descriptor.Members[0].Put
	if data, err := outbox.ReadFile(packed.Blobs[0].File()); err != nil || string(data) != "bundle bytes" {
		t.Fatalf("blob = %q, %v", data, err)
	}
	if data, err := outbox.ReadFile(packed.Manifest); err != nil || !strings.Contains(string(data), bundle.Digest) {
		t.Fatalf("manifest = %q, %v", data, err)
	}
}

func TestOutboxLayoutRefusesSymbolicLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "every-path-stays-inside-the-outbox")
	source := t.TempDir()
	writeOver(t, filepath.Join(source, "index.json"), "{}")
	if err := os.MkdirAll(filepath.Join(source, "blobs", "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeOver(t, filepath.Join(source, "blobs", "sha256", "abc"), "blob")

	root := filepath.Join(t.TempDir(), "outbox")
	writer, err := NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CopyLayout("oci/app", source); err != nil {
		t.Fatalf("copy layout: %v", err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	outbox, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if dir, err := outbox.Layout("oci/app"); err != nil || dir != filepath.Join(root, "oci", "app") {
		t.Fatalf("layout = %q, %v", dir, err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "oci", "app", "blobs", "sha256", "def")); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Layout("oci/app"); err == nil {
		t.Fatal("a layout holding a symbolic link resolved")
	}

	linked := t.TempDir()
	if err := os.Symlink("/etc/hosts", filepath.Join(linked, "index.json")); err != nil {
		t.Fatal(err)
	}
	other, err := NewWriter(filepath.Join(t.TempDir(), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.CopyLayout("oci/app", linked); err == nil {
		t.Fatal("copied a layout holding a symbolic link")
	}
	if _, err := other.CopyFile("npm/web.tgz", filepath.Join(linked, "index.json")); err == nil {
		t.Fatal("copied a symbolic link")
	}
}

func TestWriterFromEnvNeedsTheOutboxVariable(t *testing.T) {
	t.Setenv(extproto.PublicationOutboxEnv, "")
	if _, err := WriterFromEnv(); err == nil || !strings.Contains(err.Error(), extproto.PublicationOutboxEnv) {
		t.Fatalf("writer without the variable err=%v", err)
	}
	root := filepath.Join(t.TempDir(), "outbox")
	t.Setenv(extproto.PublicationOutboxEnv, root)
	writer, err := WriterFromEnv()
	if err != nil || writer.Root() != root {
		t.Fatalf("writer from env = %v, %v", writer, err)
	}
}
