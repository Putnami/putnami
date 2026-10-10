package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// memFS is an in-memory WriteFS for round-trip tests (no disk).
type memFS map[string][]byte

func (m memFS) WriteFile(rel string, data []byte) error {
	m[rel] = append([]byte(nil), data...)
	return nil
}

var errTestIO = errors.New("test I/O failure")

type failingFS struct{}

func (failingFS) Open(string) (fs.File, error) { return nil, errTestIO }

type readFailFS struct{ fs.FS }

func (f readFailFS) Open(name string) (fs.File, error) {
	if name == "broken.sql" {
		return nil, errTestIO
	}
	return f.FS.Open(name)
}

type failingWriteFS struct{}

func (failingWriteFS) WriteFile(string, []byte) error { return errTestIO }

func sampleBundle() fstest.MapFS {
	return fstest.MapFS{
		"bundle.json": {Data: []byte(`{"protocol":"migration-bundle.v1","appName":"orders"}`)},
		"payload/sql/default/iam/001_init.up.sql":   {Data: []byte("CREATE TABLE a();")},
		"payload/sql/default/iam/001_init.down.sql": {Data: []byte("DROP TABLE a;")},
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	src := sampleBundle()
	tarball, err := Pack(src)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}

	dest := memFS{}
	if err := Unpack(tarball, dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	if len(dest) != len(src) {
		t.Fatalf("round-trip file count = %d, want %d", len(dest), len(src))
	}
	for name, f := range src {
		got, ok := dest[name]
		if !ok {
			t.Fatalf("missing %q after round-trip", name)
		}
		if !bytes.Equal(got, f.Data) {
			t.Fatalf("content mismatch for %q: got %q want %q", name, got, f.Data)
		}
	}
}

func TestPackIsDeterministic(t *testing.T) {
	// Two independently-built maps with the same content must pack identically,
	// so the blob digest is stable across rebuilds (idempotent re-publish).
	a, err := Pack(sampleBundle())
	if err != nil {
		t.Fatalf("pack a: %v", err)
	}
	b, err := Pack(sampleBundle())
	if err != nil {
		t.Fatalf("pack b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Pack is not deterministic: %d vs %d bytes", len(a), len(b))
	}
}

func TestPackPropagatesFilesystemErrors(t *testing.T) {
	if _, err := Pack(failingFS{}); !errors.Is(err, errTestIO) {
		t.Fatalf("walk error = %v, want %v", err, errTestIO)
	}

	broken := readFailFS{FS: fstest.MapFS{
		"broken.sql": {Data: []byte("SELECT 1;")},
	}}
	if _, err := Pack(broken); !errors.Is(err, errTestIO) {
		t.Fatalf("read error = %v, want %v", err, errTestIO)
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.sql", Mode: 0o600, Size: 3})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()

	if err := Unpack(buf.Bytes(), memFS{}); err == nil {
		t.Fatal("Unpack must reject a path-traversal entry")
	}
}

func TestUnpackRejectsAbsolute(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "/etc/passwd", Mode: 0o600, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()

	dest := memFS{}
	if err := Unpack(buf.Bytes(), dest); err != nil {
		t.Fatalf("absolute path should normalize to a safe relative entry, got error: %v", err)
	}
	if _, ok := dest["etc/passwd"]; !ok {
		t.Fatalf("absolute /etc/passwd should land as relative etc/passwd, got %v", keys(dest))
	}
}

func TestUnpackAcceptsDirectoryEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "payload/", Typeflag: tar.TypeDir, Mode: 0o750}); err != nil {
		t.Fatalf("write directory header: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "payload/file.sql", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
		t.Fatalf("write file header: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write file body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	dest := memFS{}
	if err := Unpack(buf.Bytes(), dest); err != nil {
		t.Fatalf("Unpack directory entry: %v", err)
	}
	if got := string(dest["payload/file.sql"]); got != "x" {
		t.Fatalf("unpacked file = %q, want x", got)
	}
}

func TestUnpackRejectsUnsupportedEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "payload/link", Typeflag: tar.TypeSymlink, Linkname: "target"}); err != nil {
		t.Fatalf("write symlink header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := Unpack(buf.Bytes(), memFS{}); err == nil {
		t.Fatal("Unpack must reject non-regular entries")
	}
}

func TestUnpackPropagatesReadAndWriteErrors(t *testing.T) {
	if err := Unpack([]byte("not a tar archive"), memFS{}); err == nil {
		t.Fatal("Unpack must reject a malformed tar archive")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "bundle.json", Mode: 0o600, Size: 2}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("{}")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := Unpack(buf.Bytes(), failingWriteFS{}); !errors.Is(err, errTestIO) {
		t.Fatalf("write error = %v, want %v", err, errTestIO)
	}
}

func TestSafeRelNormalizesAndRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", ".", "/", "../escape", `..\escape`} {
		if _, err := safeRel(name); err == nil {
			t.Errorf("safeRel(%q) must fail", name)
		}
	}

	for name, want := range map[string]string{
		`payload\sql\001.sql`: "payload/sql/001.sql",
		"/bundle.json":        "bundle.json",
	} {
		got, err := safeRel(name)
		if err != nil {
			t.Errorf("safeRel(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("safeRel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestUnpackToDirRoundTrip(t *testing.T) {
	tarball, err := Pack(sampleBundle())
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	dir := t.TempDir()
	if err := UnpackToDir(tarball, dir); err != nil {
		t.Fatalf("unpack to dir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "payload", "sql", "default", "iam", "001_init.up.sql"))
	if err != nil {
		t.Fatalf("read unpacked file: %v", err)
	}
	if string(got) != "CREATE TABLE a();" {
		t.Fatalf("unpacked content = %q", got)
	}
}

func TestUnpackToDirPropagatesFilesystemErrors(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	contents := []byte("SELECT 1;")
	if err := tw.WriteHeader(&tar.Header{Name: "nested/migration.sql", Mode: 0o600, Size: int64(len(contents))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := UnpackToDir(buf.Bytes(), blocked); err == nil {
		t.Fatal("UnpackToDir must propagate directory creation errors")
	}
}

func keys(m memFS) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPackageNameSlugsPathStyleAppNames(t *testing.T) {
	// Application names are path-style, but a registry package name is a
	// single path segment.
	for app, want := range map[string]string{
		"billing/workloads/api-server": "billing-workloads-api-server",
		"shop/workloads/orders":        "shop-workloads-orders",
		"orders-api":                   "orders-api",
	} {
		if got := PackageName(app); got != want {
			t.Errorf("PackageName(%q) = %q, want %q", app, got, want)
		}
	}
}

func TestFixtureValidBundlePacksDeterministically(t *testing.T) {
	root := filepath.Join("testdata", "valid")
	first, err := Pack(os.DirFS(root))
	if err != nil {
		t.Fatalf("pack fixture: %v", err)
	}
	second, err := Pack(os.DirFS(root))
	if err != nil {
		t.Fatalf("repack fixture: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("fixture packing must be byte-deterministic")
	}

	for rel, want := range map[string]string{
		"payload/sql/platform_migrations/publish-v2/001_fixture.up.sql":   "81e19b2a77797020f0c77525bfb7cd044d505232f6f8c0c67b9f9153c06a010c",
		"payload/sql/platform_migrations/publish-v2/001_fixture.down.sql": "26129ebb9c1fc9954a02d2fadbc8ee3d0560fd9cddf8c07b1b0bcb471fe2fb2c",
	} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		got := sha256.Sum256(contents)
		if actual := hex.EncodeToString(got[:]); actual != want {
			t.Errorf("payload hash for %s = %s, want %s", rel, actual, want)
		}
	}
}

// TestValidNamespaceHoldsOneSegmentBothSidesCanAddress pins the rule a
// publisher and a consumer both check a namespace against. The namespace
// becomes one registry path segment, so a value that adds a segment, climbs out
// of one, or carries upper case is refused rather than normalized.
func TestValidNamespaceHoldsOneSegmentBothSidesCanAddress(t *testing.T) {
	for _, ok := range []string{Namespace, "acme", "acme-corp", "acme.corp", "acme_corp", "a", "0acme", strings.Repeat("a", 128)} {
		if !ValidNamespace(ok) {
			t.Errorf("ValidNamespace(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " ", "Acme", "acme/corp", "../secrets", ".", "..", "-acme", ".acme", "acme corp", "acme%2fcorp", strings.Repeat("a", 129)} {
		if ValidNamespace(bad) {
			t.Errorf("ValidNamespace(%q) = true, want false", bad)
		}
	}
}
