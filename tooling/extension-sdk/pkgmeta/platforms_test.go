package pkgmeta

import (
	"reflect"
	"testing"
)

// The distribution matrix is the registry keys the CLI's installer asks for,
// in publication order, and a caller cannot rewrite it for the others.
func TestArchivePlatformsIsTheDistributionMatrix(t *testing.T) {
	want := []ArchivePlatform{
		{GOOS: "linux", GOARCH: "amd64", Suffix: "linux-x64"},
		{GOOS: "linux", GOARCH: "arm64", Suffix: "linux-arm64"},
		{GOOS: "darwin", GOARCH: "amd64", Suffix: "darwin-x64"},
		{GOOS: "darwin", GOARCH: "arm64", Suffix: "darwin-arm64"},
		{GOOS: "windows", GOARCH: "amd64", Suffix: "windows-x64"},
	}
	got := ArchivePlatforms()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ArchivePlatforms() = %#v, want %#v", got, want)
	}
	if suffixes := ArchivePlatformSuffixes(); !reflect.DeepEqual(suffixes, []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64", "windows-x64"}) {
		t.Fatalf("ArchivePlatformSuffixes() = %v", suffixes)
	}
	got[0].Suffix = "tampered"
	if ArchivePlatforms()[0].Suffix != "linux-x64" {
		t.Fatal("ArchivePlatforms returned the shared matrix; one caller can rewrite every packager's keys")
	}
}

// A Windows archive carries compiled/<name>.exe and every other archive
// compiled/<name>, whatever OS runs the packager or the CLI.
func TestExecutableNameCarriesTheWindowsSuffixOnlyForWindows(t *testing.T) {
	for _, tc := range []struct {
		goos, name, want string
	}{
		{goos: "windows", name: "putnami", want: "putnami.exe"},
		{goos: "windows", name: "compiled/putnami-go", want: "compiled/putnami-go.exe"},
		{goos: "windows", name: "compiled/tool.exe", want: "compiled/tool.exe"},
		{goos: "windows", name: "compiled/TOOL.EXE", want: "compiled/TOOL.EXE"},
		{goos: "windows", name: "putnami-1.2", want: "putnami-1.2.exe"},
		{goos: "linux", name: "compiled/putnami-go", want: "compiled/putnami-go"},
		{goos: "darwin", name: "putnami", want: "putnami"},
		{goos: "", name: "putnami", want: "putnami"},
	} {
		if got := ExecutableName(tc.goos, tc.name); got != tc.want {
			t.Errorf("ExecutableName(%q, %q) = %q, want %q", tc.goos, tc.name, got, tc.want)
		}
	}
	want := map[string]string{
		"linux-x64":    "compiled/putnami",
		"linux-arm64":  "compiled/putnami",
		"darwin-x64":   "compiled/putnami",
		"darwin-arm64": "compiled/putnami",
		"windows-x64":  "compiled/putnami.exe",
	}
	for _, platform := range ArchivePlatforms() {
		if got := ExecutableName(platform.GOOS, "compiled/putnami"); got != want[platform.Suffix] {
			t.Errorf("%s archive executable = %q, want %q", platform.Suffix, got, want[platform.Suffix])
		}
	}
}
