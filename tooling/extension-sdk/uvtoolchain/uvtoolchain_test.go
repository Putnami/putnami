package uvtoolchain

import (
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
