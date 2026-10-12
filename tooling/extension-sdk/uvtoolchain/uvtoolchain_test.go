package uvtoolchain

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The one table of declarations. The CLI pins the lock from this reading and
// the @putnami/python extension installs and selects uv from it, so neither
// keeps a copy of the cases.
func TestReadDeclaration(t *testing.T) {
	cases := []struct {
		name      string
		pyproject string
		want      Declaration
	}{
		{"exact", "[tool.uv]\nrequired-version = \"==0.11.32\"\n",
			Declaration{Present: true, Spec: "==0.11.32", Version: "0.11.32"}},
		{"bare release", "[tool.uv]\nrequired-version = '0.11.32'\n",
			Declaration{Present: true, Spec: "0.11.32", Version: "0.11.32"}},
		{"spaces and comments", "# uv\n[ tool.uv ]  # uv settings\r\n  required-version   =   \"== 0.11.32\"   # pinned\r\n",
			Declaration{Present: true, Spec: "== 0.11.32", Version: "0.11.32"}},
		{"quoted key", "[tool.uv]\n\"required-version\" = \"==0.11.32\"\n",
			Declaration{Present: true, Spec: "==0.11.32", Version: "0.11.32"}},
		{"major minor", "[tool.uv]\nrequired-version = \"==0.12\"\n",
			Declaration{Present: true, Spec: "==0.12", Version: "0.12.0"}},
		{"leading v", "[tool.uv]\nrequired-version = \"v0.11.32\"\n",
			Declaration{Present: true, Spec: "v0.11.32", Version: "0.11.32"}},
		{"after other keys and before a sub-table", "[tool.uv]\nmanaged = true\nrequired-version = \"==0.11.32\"\n[tool.uv.sources]\n",
			Declaration{Present: true, Spec: "==0.11.32", Version: "0.11.32"}},
		{"the first of two", "[tool.uv]\nrequired-version = \"==0.11.32\"\nrequired-version = \"==0.12.0\"\n",
			Declaration{Present: true, Spec: "==0.11.32", Version: "0.11.32"}},
		{"range", "[tool.uv]\nrequired-version = \">=0.5\"\n",
			Declaration{Present: true, Spec: ">=0.5"}},
		{"wildcard", "[tool.uv]\nrequired-version = \"==0.11.*\"\n",
			Declaration{Present: true, Spec: "==0.11.*"}},
		{"arbitrary equality", "[tool.uv]\nrequired-version = \"===0.11.32\"\n",
			Declaration{Present: true, Spec: "===0.11.32"}},
		{"compound", "[tool.uv]\nrequired-version = \"==0.11.32,<1\"\n",
			Declaration{Present: true, Spec: "==0.11.32,<1"}},
		{"prerelease", "[tool.uv]\nrequired-version = \"==0.11.32rc1\"\n",
			Declaration{Present: true, Spec: "==0.11.32rc1"}},
		{"not a string", "[tool.uv]\nrequired-version = 0.11\n",
			Declaration{Present: true}},
		{"unterminated", "[tool.uv]\nrequired-version = \"==0.11.32\n",
			Declaration{Present: true}},
		{"empty", "[tool.uv]\nrequired-version = \"\"\n",
			Declaration{Present: true}},
		{"another table", "[tool.uv.workspace]\nrequired-version = \"==0.11.32\"\n", Declaration{}},
		{"top level", "required-version = \"==0.11.32\"\n[tool.uv]\n", Declaration{}},
		{"array of tables", "[[tool.uv]]\nrequired-version = \"==0.11.32\"\n", Declaration{}},
		{"an index entry after the table", "[tool.uv]\nmanaged = true\n[[tool.uv.index]]\nrequired-version = \"==0.11.32\"\n", Declaration{}},
		{"a word in a comment or another value", "[project]\n# required-version = \"==0.11.32\"\ndescription = \"required-version = ==0.11.32\"\n", Declaration{}},
		{"no declaration", "[project]\nname = \"x\"\n", Declaration{}},
		{"empty document", "", Declaration{}},
		{"dotted keys in the tool table", "[tool]\nuv.managed = true\nuv.required-version = \"==0.11.32\"\n",
			Declaration{Outside: "uv.managed"}},
		{"inline table in the tool table", "[tool]\nuv = { required-version = \"==0.11.32\" }\n",
			Declaration{Outside: "uv"}},
		{"dotted keys at the top level", "tool.uv.required-version = \"==0.11.32\"\n",
			Declaration{Outside: "tool.uv.required-version"}},
		{"inline tool table at the top level", "tool = { uv = { managed = true } }\n",
			Declaration{Outside: "tool"}},
		{"a sub-table after dotted keys", "[tool]\n\"uv\" . managed = true\n\n[tool.uv.sources]\npkg = { workspace = true }\n",
			Declaration{Outside: "uv.managed"}},
		{"other tools as dotted keys", "tool.ruff.line-length = 100\n[tool]\nruff.select = [\"E\"]\n[tool.uv]\nrequired-version = \"==0.11.32\"\n",
			Declaration{Present: true, Spec: "==0.11.32", Version: "0.11.32"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadDeclaration(tc.pyproject); got != tc.want {
				t.Errorf("ReadDeclaration(%q) = %+v, want %+v", tc.pyproject, got, tc.want)
			}
		})
	}
}

func TestExactRelease(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"==0.11.32", "0.11.32"},
		{"\t== 1.2.3\t", "1.2.3"},
		{"0.12", "0.12.0"},
		{"==0", ""},
		{"==1.2.3.4", ""},
		{"~=0.11.32", ""},
		{"!=0.11.32", ""},
		{"==0.11.x", ""},
		{"", ""},
	} {
		if got := ExactRelease(tc.spec); got != tc.want {
			t.Errorf("ExactRelease(%q) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// Every platform a uv is installed on names one archive in the release, and
// the URLs are the release downloads of the uv repository.
func TestArchives(t *testing.T) {
	want := map[string]Archive{
		"darwin/amd64":  {Name: "uv-x86_64-apple-darwin.tar.gz", Dir: "uv-x86_64-apple-darwin"},
		"darwin/arm64":  {Name: "uv-aarch64-apple-darwin.tar.gz", Dir: "uv-aarch64-apple-darwin"},
		"linux/amd64":   {Name: "uv-x86_64-unknown-linux-musl.tar.gz", Dir: "uv-x86_64-unknown-linux-musl"},
		"linux/arm64":   {Name: "uv-aarch64-unknown-linux-musl.tar.gz", Dir: "uv-aarch64-unknown-linux-musl"},
		"windows/amd64": {Name: "uv-x86_64-pc-windows-msvc.zip", Zip: true},
		"windows/arm64": {Name: "uv-aarch64-pc-windows-msvc.zip", Zip: true},
	}
	platforms := Platforms()
	if !slices.IsSorted(platforms) || len(platforms) != len(want) {
		t.Fatalf("Platforms() = %v, want the %d platforms sorted", platforms, len(want))
	}
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		got, ok := ArchiveFor(goos, goarch)
		if !ok || got != want[platform] {
			t.Errorf("ArchiveFor(%s) = %+v, %v; want %+v", platform, got, ok, want[platform])
		}
	}
	if archive, ok := ArchiveFor("freebsd", "amd64"); ok {
		t.Errorf("ArchiveFor(freebsd/amd64) = %+v, want none", archive)
	}
	if got := ReleaseURL("0.11.32"); got != "https://github.com/astral-sh/uv/releases/download/0.11.32/" {
		t.Errorf("ReleaseURL = %q", got)
	}
	if got := ChecksumsURL("0.11.32"); got != "https://github.com/astral-sh/uv/releases/download/0.11.32/sha256.sum" {
		t.Errorf("ChecksumsURL = %q", got)
	}
}

// Only a plain release from MinimumVersion on is supported, and the refusal
// names the minimum.
func TestSupported(t *testing.T) {
	for _, version := range []string{MinimumVersion, "0.11.32", "0.12.0", "1.0.0", "0.100.0", "0.11.10"} {
		if err := Supported(version); err != nil {
			t.Errorf("Supported(%q) = %v, want nil", version, err)
		}
	}
	for _, version := range []string{"0.11.7", "0.10.99", "0.4.30", "0.0.0", "00.011.0007"} {
		err := Supported(version)
		if err == nil || !strings.Contains(err.Error(), "older than uv "+MinimumVersion) {
			t.Errorf("Supported(%q) = %v, want a refusal that names %s", version, err, MinimumVersion)
		}
	}
	for _, version := range []string{"", "0.11", "0.11.32rc1", "../0.11.32", "0.11.32/", "v0.11.32"} {
		if err := Supported(version); err == nil || !strings.Contains(err.Error(), "names no uv release") {
			t.Errorf("Supported(%q) = %v, want a refusal", version, err)
		}
	}
}

// The digests Putnami ships for DefaultVersion are the ones the release lists
// in its sha256.sum. testdata/<version>/sha256.sum is a copy of that file, so
// a new DefaultVersion needs the copy of its own.
func TestDefaultIntegritiesMatchTheReleaseChecksums(t *testing.T) {
	path := filepath.Join("testdata", DefaultVersion, ChecksumsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, a copy of %s: %v", path, ChecksumsURL(DefaultVersion), err)
	}
	published := PlatformDigests(string(data))
	shipped := DefaultIntegrities()
	if !maps.Equal(shipped, published) {
		t.Fatalf("uv %s ships digests %v, its %s lists %v", DefaultVersion, shipped, ChecksumsFile, published)
	}
	if len(shipped) != len(Platforms()) {
		t.Fatalf("uv %s ships %d digests for the %d platforms %v", DefaultVersion, len(shipped), len(Platforms()), Platforms())
	}
	if err := Supported(DefaultVersion); err != nil {
		t.Fatalf("the default release is one Putnami cannot run: %v", err)
	}
	shipped["darwin/arm64"] = "changed"
	if DefaultIntegrities()["darwin/arm64"] == "changed" {
		t.Fatal("DefaultIntegrities returned the shipped map, not a copy")
	}
}

// Only a "<sha256> *<archive>" line of an archive a platform installs gives a
// digest, lower-cased. The star is optional, as sha256sum writes it only for
// binary mode.
func TestPlatformDigests(t *testing.T) {
	checksums := strings.Repeat("A", 64) + " *uv-x86_64-apple-darwin.tar.gz\r\n" +
		"not-a-digest *uv-aarch64-apple-darwin.tar.gz\n" +
		strings.Repeat("b", 64) + " *uv-x86_64-unknown-linux-gnu.tar.gz\n" +
		strings.Repeat("c", 64) + "  uv-aarch64-pc-windows-msvc.zip\n" +
		strings.Repeat("d", 63) + " *uv-x86_64-pc-windows-msvc.zip\n" +
		strings.Repeat("e", 64) + " *uv-aarch64-unknown-linux-musl.tar.gz trailing\n" +
		"\n"
	want := map[string]string{
		"darwin/amd64":  strings.Repeat("a", 64),
		"windows/arm64": strings.Repeat("c", 64),
	}
	if got := PlatformDigests(checksums); !maps.Equal(got, want) {
		t.Fatalf("PlatformDigests = %v, want %v", got, want)
	}
}

// Problem names what is wrong with a declaration and the one to write, and
// nothing for a declaration Putnami pins or for none.
func TestDeclarationProblem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pyproject string
		want      []string
	}{
		{"no declaration", "[project]\nname = \"ws\"\n", nil},
		{"an exact release", "[tool.uv]\nrequired-version = \"==0.11.32\"\n", nil},
		{"a range", "[tool.uv]\nrequired-version = \">=0.5\"\n", []string{
			`pyproject.toml [tool.uv] required-version is ">=0.5", which names no single uv release`,
			`required-version = "==` + DefaultVersion + `", so Putnami can pin it`,
		}},
		{"not a string", "[tool.uv]\nrequired-version = 11\n", []string{
			"required-version is empty or not a one-line string, which names no single uv release",
		}},
		{"an old release", "[tool.uv]\nrequired-version = \"==0.10.4\"\n", []string{
			"pyproject.toml [tool.uv] required-version declares a release Putnami cannot run: uv 0.10.4 is older than uv " + MinimumVersion,
			`required-version = "==` + MinimumVersion + `" or a later release`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ReadDeclaration(tc.pyproject).Problem()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Problem = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Problem = nil, want one")
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("Problem = %q, want it to name %q", err, part)
				}
			}
		})
	}
}

// The root pyproject.toml of a workspace is read with ReadDeclaration; a
// workspace without one declares nothing, and one that cannot be read is an
// error.
func TestReadWorkspaceDeclaration(t *testing.T) {
	root := t.TempDir()
	if got, err := ReadWorkspaceDeclaration(root); err != nil || got != (Declaration{}) {
		t.Fatalf("ReadWorkspaceDeclaration without a pyproject.toml = %+v, %v; want nothing", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, Pyproject), []byte("[tool.uv]\nrequired-version = \"==0.11.32\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadWorkspaceDeclaration(root); err != nil || got.Version != "0.11.32" {
		t.Fatalf("ReadWorkspaceDeclaration = %+v, %v; want 0.11.32", got, err)
	}
	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, Pyproject), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorkspaceDeclaration(unreadable); err == nil || !strings.Contains(err.Error(), "read pyproject.toml") {
		t.Fatalf("ReadWorkspaceDeclaration error = %v, want the unreadable pyproject.toml reported", err)
	}
}
