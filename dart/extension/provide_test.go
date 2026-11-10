package dart

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// publisher is a local stand-in for the dart-archive and the Flutter release
// manifests: the extension's download boundary.
type publisher struct {
	files    map[string][]byte
	requests []string
}

func (p *publisher) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests = append(p.requests, r.URL.Path)
		body, ok := p.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// dartArchive is a Dart SDK zip whose dart prints version.
func dartArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: "dart-sdk/bin/dart", Method: zip.Deflate}
	header.SetMode(0o755)
	f, err := z.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("#!/bin/sh\necho 'Dart SDK version: " + version + " (stable)'\n"))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// flutterArchive is a Flutter SDK tar.gz whose flutter prints version and the
// Dart it bundles.
func flutterArchive(t *testing.T, version, dart string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range map[string]string{
		"flutter/bin/flutter":                "#!/bin/sh\nprintf 'Flutter " + version + " • channel stable\\nTools • Dart " + dart + "\\n'\n",
		"flutter/bin/dart":                   "#!/bin/sh\necho 'Dart SDK version: " + dart + " (stable)'\n",
		"flutter/bin/cache/dart-sdk/version": dart + "\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(content))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// usePublisher points the extension at a local publisher, on linux/amd64,
// with a Putnami home of its own.
func usePublisher(t *testing.T, p *publisher) {
	t.Helper()
	server := p.serve(t)
	saved := []string{dartArchiveURL, dartReleaseListURL, flutterReleasesURL, hostPlatform}
	dartArchiveURL = server.URL + "/dart-archive/channels/stable/release"
	dartReleaseListURL = server.URL + "/list?prefix=x"
	flutterReleasesURL = server.URL + "/flutter"
	hostPlatform = "linux/amd64"
	t.Cleanup(func() {
		dartArchiveURL, dartReleaseListURL, flutterReleasesURL, hostPlatform = saved[0], saved[1], saved[2], saved[3]
	})
	t.Setenv(putnamihome.Env, t.TempDir())
}

func dartPublisher(t *testing.T, versions ...string) *publisher {
	t.Helper()
	p := &publisher{files: map[string][]byte{}}
	var prefixes []string
	for _, v := range versions {
		prefixes = append(prefixes, "channels/stable/release/"+v+"/")
		archive := dartArchive(t, v)
		path := "/dart-archive/channels/stable/release/" + v + "/sdk/dartsdk-linux-x64-release.zip"
		p.files[path] = archive
		p.files[path+".sha256sum"] = []byte(digest(archive) + " *dartsdk-linux-x64-release.zip\n")
	}
	prefixes = append(prefixes, "channels/stable/release/latest/", "channels/stable/release/29803/")
	list, _ := json.Marshal(map[string]any{"prefixes": prefixes})
	p.files["/list"] = list
	return p
}

func dartToolchain(t *testing.T, variant string) stacks.Toolchain {
	t.Helper()
	profile, err := stack(t).Profile.Variant(variant)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Toolchain
}

func TestATaskInstallsTheSDKTheConstraintAdmits(t *testing.T) {
	spectest.Proves(t, feature, "installs-the-sdk", "a-task-installs-the-sdk-the-constraint-admits-when-the-machine-has-none")
	p := dartPublisher(t, "3.6.2", "3.7.2", "3.8.1", "4.0.0")
	usePublisher(t, p)
	tc := dartToolchain(t, VariantDart)
	var said []string
	provided, err := Provide(context.Background(), tc, []stacks.Expected{{Version: "^3.7.0", Source: "pubspec.yaml"}}, func(s string) { said = append(said, s) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provided.Executable, "dart-sdk-3.8.1-linux-x64") || len(said) != 1 {
		t.Fatalf("the newest release in ^3.7.0 is installed: %+v %v", provided, said)
	}
	pinned := tc
	pinned.Command = append([]string{provided.Executable}, tc.Command[1:]...)
	if v, err := pinned.MachineVersion(context.Background(), t.TempDir(), os.Environ()); err != nil || v != "3.8.1" {
		t.Errorf("the installed dart answers %q, %v", v, err)
	}
	p.requests = nil
	again, err := Provide(context.Background(), tc, []stacks.Expected{{Version: ">=3.7.0 <3.9.0", Source: "pubspec.yaml"}}, func(string) {})
	if err != nil || again.Executable != provided.Executable || len(p.requests) != 0 {
		t.Errorf("an earlier install that passes is reused offline: %+v %v %v", again, err, p.requests)
	}
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{Version: "^5.0.0", Source: "pubspec.yaml"}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "no stable Dart SDK") {
		t.Errorf("a constraint no release meets: %v", err)
	}
	broken := dartPublisher(t, "3.9.0")
	broken.files["/dart-archive/channels/stable/release/3.9.0/sdk/dartsdk-linux-x64-release.zip.sha256sum"] = []byte(strings.Repeat("0", 64) + "\n")
	usePublisher(t, broken)
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{Version: "^3.9.0", Source: "pubspec.yaml"}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("an archive whose SHA-256 is not the publisher's is refused: %v", err)
	}
	hostPlatform = "plan9/amd64"
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "put dart on PATH") {
		t.Errorf("a platform with no archive: %v", err)
	}
}

func TestAFlutterReleaseMustBundleADartInTheSDKConstraint(t *testing.T) {
	spectest.Proves(t, feature, "installs-the-sdk", "a-task-installs-the-sdk-the-constraint-admits-when-the-machine-has-none")
	p := &publisher{files: map[string][]byte{}}
	var releases []map[string]string
	for _, r := range []struct{ version, dart, channel string }{
		{"3.27.4", "3.6.2", "stable"}, {"3.29.2", "3.7.2", "stable"}, {"3.32.0", "3.8.0", "stable"}, {"3.33.0-0.1.pre", "3.9.0", "beta"},
	} {
		archive := flutterArchive(t, r.version, r.dart)
		name := "stable/linux/flutter_linux_" + r.version + "-stable.tar.gz"
		p.files["/flutter/"+name] = archive
		releases = append(releases, map[string]string{"channel": r.channel, "version": r.version, "dart_sdk_version": r.dart, "dart_sdk_arch": "x64", "archive": name, "sha256": digest(archive)})
	}
	usePublisher(t, p)
	manifest, _ := json.Marshal(map[string]any{"base_url": flutterReleasesURL, "releases": releases})
	p.files["/flutter/releases_linux.json"] = manifest
	tc := dartToolchain(t, VariantFlutter)
	expected := []stacks.Expected{{Version: ">=3.29.0", Source: "apps/mobile/pubspec.yaml"}, {Version: ">=3.7.0 <3.8.0", Source: "apps/mobile/pubspec.yaml"}}
	provided, err := Provide(context.Background(), tc, expected, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(provided.Executable, filepath.Join("flutter-3.29.2-linux-x64", "flutter", "bin", "flutter")) {
		t.Fatalf("the newest Flutter whose Dart is in environment.sdk: %s", provided.Executable)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(provided.Executable), "dart")); err != nil {
		t.Errorf("the Flutter install holds its dart: %v", err)
	}
	p.requests = nil
	if again, err := Provide(context.Background(), tc, expected, func(string) {}); err != nil || again.Executable != provided.Executable || len(p.requests) != 0 {
		t.Errorf("an earlier Flutter install is reused offline: %+v %v %v", again, err, p.requests)
	}
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{Version: ">=3.40.0"}, {}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "no stable Flutter SDK") {
		t.Errorf("a constraint no release meets: %v", err)
	}
	hostPlatform = "linux/arm64"
	if _, err := Provide(context.Background(), tc, expected, func(string) {}); err == nil || !strings.Contains(err.Error(), "put flutter on PATH") {
		t.Errorf("Linux arm64 has no Flutter archive: %v", err)
	}
}

func TestATaskWithNoSDKOnPathRunsTheOneItInstalls(t *testing.T) {
	spectest.Proves(t, feature, "installs-the-sdk", "a-task-installs-the-sdk-the-constraint-admits-when-the-machine-has-none")
	root := fixture(t)
	p := dartPublisher(t, "3.7.2", "3.8.1")
	usePublisher(t, p)
	bin := t.TempDir()
	log := filepath.Join(bin, "tools.log")
	for _, tool := range []string{"tojunit", "format_coverage"} {
		stackstest.WriteFile(t, bin, tool, "#!/bin/sh\necho \"$0 $*\" >> "+log+"\n")
		_ = os.Chmod(filepath.Join(bin, tool), 0o755)
	}
	// No dart on PATH: the task installs one, then runs its steps with it.
	t.Setenv("PATH", bin)
	status, _, _, err := runJob(t, root, "tools/gen", stacks.TaskLint)
	// The installed stub dart writes nothing a lint step checks: lint passes
	// once the steps ran with it.
	if status != "OK" || err != nil {
		t.Fatalf("lint gen with no dart on PATH: %s %v", status, err)
	}
	if !strings.Contains(strings.Join(p.requests, " "), "dartsdk-linux-x64-release.zip") {
		t.Errorf("the task installed the Dart SDK: %v", p.requests)
	}
}

func TestTheTestTaskActivatesTojunitAndFormatCoverageInItsOwnPubCache(t *testing.T) {
	spectest.Proves(t, feature, "installs-the-test-tools", "the-test-task-activates-tojunit-and-format-coverage-in-its-own-pub-cache")
	t.Setenv(putnamihome.Env, t.TempDir())
	bin := t.TempDir()
	log := filepath.Join(bin, "dart.log")
	version := filepath.Join(bin, "version")
	stackstest.WriteFile(t, bin, "version", "3.7.2")
	stackstest.WriteFile(t, bin, "dart", "#!/bin/sh\nif [ \"$1\" = --version ]; then read v < "+version+"; echo \"Dart SDK version: $v (stable)\"; exit 0; fi\necho \"PUB_CACHE=$PUB_CACHE $*\" >> "+log+"\n")
	if err := os.Chmod(filepath.Join(bin, "dart"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin, "PUB_CACHE=/home/me/.pub-cache"}
	if dirs, err := Tools(context.Background(), stacks.TaskLint, stacks.Metadata{}, env, func(string) {}); err != nil || dirs != nil {
		t.Errorf("lint needs no tool: %v %v", dirs, err)
	}
	dirs, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, env, func(string) {})
	if err != nil || len(dirs) != 1 {
		t.Fatalf("Tools = %v, %v", dirs, err)
	}
	ran, _ := os.ReadFile(log)
	cache := filepath.Join(filepath.Dir(dirs[0]), "pub-cache-3.7.2")
	for _, want := range []string{"PUB_CACHE=" + cache + " pub global activate junitreport 2.0.2", "PUB_CACHE=" + cache + " pub global activate coverage ^1.6.0"} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("activations\n%s\nwant %q", ran, want)
		}
	}
	launcher, err := os.ReadFile(filepath.Join(dirs[0], "tojunit"))
	if err != nil || !strings.Contains(string(launcher), "exec '"+filepath.Join(bin, "dart")+"' pub global run junitreport:tojunit") || !strings.Contains(string(launcher), cache) {
		t.Errorf("the tojunit launcher runs from the extension's pub cache: %s %v", launcher, err)
	}
	_ = os.Remove(log)
	if _, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, env, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(log); len(again) != 0 {
		t.Errorf("an activation is not repeated: %s", again)
	}
	if dirs, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, []string{"PATH=" + dirs[0] + string(os.PathListSeparator) + bin}, func(string) {}); err != nil || dirs != nil {
		t.Errorf("tools already on PATH need nothing: %v %v", dirs, err)
	}
	stackstest.WriteFile(t, bin, "version", "3.9.1")
	other, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, env, func(string) {})
	if err != nil || len(other) != 1 || other[0] == dirs[0] {
		t.Fatalf("another SDK version gets its own tools: %v %v", other, err)
	}
	if again, _ := os.ReadFile(log); !strings.Contains(string(again), "pub-cache-3.9.1 pub global activate junitreport") {
		t.Errorf("another SDK version activates its own tools: %s", again)
	}
	if _, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{}, []string{"PATH=" + t.TempDir()}, func(string) {}); err == nil {
		t.Error("no dart to activate the tools with")
	}
}

func TestAFlutterTestTaskActivatesTheToolsWithTheFlutterSDKsDart(t *testing.T) {
	spectest.Proves(t, feature, "installs-the-test-tools", "the-test-task-activates-tojunit-and-format-coverage-in-its-own-pub-cache")
	t.Setenv(putnamihome.Env, t.TempDir())
	// A standalone dart comes first on PATH; the Flutter SDK has its own.
	standalone, sdk := t.TempDir(), t.TempDir()
	log := filepath.Join(sdk, "dart.log")
	for dir, version := range map[string]string{standalone: "3.9.0", sdk: "3.7.2"} {
		stackstest.WriteFile(t, dir, "dart", "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'Dart SDK version: "+version+" (stable)'; exit 0; fi\necho \""+dir+" $PUB_CACHE $*\" >> "+log+"\n")
		if err := os.Chmod(filepath.Join(dir, "dart"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stackstest.WriteFile(t, sdk, "flutter", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(sdk, "flutter"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + standalone + string(os.PathListSeparator) + sdk}
	for _, variant := range []string{VariantFlutter, VariantDartInFlutter} {
		dirs, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{Variant: variant}, env, func(string) {})
		if err != nil || len(dirs) != 1 {
			t.Fatalf("%s: Tools = %v, %v", variant, dirs, err)
		}
		launcher, err := os.ReadFile(filepath.Join(dirs[0], "tojunit"))
		if err != nil || !strings.Contains(string(launcher), "exec '"+filepath.Join(sdk, "dart")+"' pub global run junitreport:tojunit") {
			t.Errorf("%s: the launcher runs the Flutter SDK's dart: %s %v", variant, launcher, err)
		}
	}
	if ran, _ := os.ReadFile(log); strings.Contains(string(ran), standalone) || !strings.Contains(string(ran), "pub-cache-3.7.2 pub global activate junitreport") {
		t.Errorf("the Flutter SDK's dart activates the tools into the pub cache of its version:\n%s", ran)
	}
	if _, err := Tools(context.Background(), stacks.TaskTest, stacks.Metadata{Variant: VariantFlutter}, []string{"PATH=" + standalone}, func(string) {}); err == nil {
		t.Error("no flutter: no dart to activate the tools with")
	}
}
