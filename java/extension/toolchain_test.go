package java

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

func TestATaskInstallsTheJDKItsBuildStates(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-task-installs-the-jdk-its-build-states")
	tc := stack(t).Profile.Toolchain
	for expected, want := range map[string]string{"": "21.0.13", "8": "1.8.0_512", "1.8": "1.8.0_512", "1.8.0": "1.8.0_512", "11": "11.0.33", "17": "17.0.21", "21": "21.0.13", "25": "25.0.5", "25.0.5": "25.0.5"} {
		got, ok := choose(acceptedBy(tc, jdks, stacks.Expected{Version: expected, Source: ".java-version"}), "")
		if !ok || got.version != want {
			t.Errorf(".java-version %q installs %s, want %s", expected, got.version, want)
		}
	}
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{Version: "23", Source: ".java-version"}}, func(string) {}); err == nil || !strings.Contains(err.Error(), "no Temurin JDK") {
		t.Errorf("a version the extension pins no JDK for fails: %v", err)
	}
	for _, r := range jdks {
		if want := map[bool]int{true: 3, false: 5}[r.version == "1.8.0_512"]; len(r.archives) != want {
			t.Errorf("%s ships %d platforms, want %d", r.version, len(r.archives), want)
		}
		major := strconv.Itoa(javaMajor(r.version))
		for platform, a := range r.archives {
			if !strings.HasPrefix(a.url, "https://github.com/adoptium/temurin"+major+"-binaries/releases/download/") || len(a.sha256) != 64 || !strings.HasSuffix(a.executable, "/javac") && !strings.HasSuffix(a.executable, "/javac.exe") {
				t.Errorf("%s %s = %+v", r.version, platform, a)
			}
		}
	}
	if a := jdks[3].archives["linux/amd64"]; a.sha256 != "d7f5d5cf865a2902b70ff549b97586a7d3f234e016f041ab447ea740c7d66d1c" || a.executable != "jdk-21.0.13+9/bin/javac" {
		t.Errorf("the linux JDK 21 = %+v", a)
	}
	if a := jdks[3].archives["darwin/arm64"]; a.executable != "jdk-21.0.13+9/Contents/Home/bin/javac" {
		t.Errorf("a macOS JDK nests its home: %+v", a)
	}

	// A stub JDK archive, with the links a Temurin archive holds.
	data := jdkArchive(t)
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write(data)
	}))
	defer server.Close()
	sum := sha256.Sum256(data)
	oldJDKs, oldPlatform, oldURL := jdks, hostPlatform, downloadURL
	jdks = []jdk{{"21.0.13", map[string]archive{"linux/amd64": {"https://example.test/jdk.tar.gz", "tar.gz", "jdk-21.0.13+9/bin/javac", hex.EncodeToString(sum[:])}}}}
	hostPlatform = "linux/amd64"
	downloadURL = func(string) string { return server.URL + "/jdk.tar.gz" }
	defer func() { jdks, hostPlatform, downloadURL = oldJDKs, oldPlatform, oldURL }()
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	provided, err := Provide(context.Background(), tc, []stacks.Expected{{Version: "21", Source: ".java-version"}}, func(string) {})
	if err != nil || !strings.HasSuffix(provided.Executable, filepath.Join("java-21.0.13", "jdk-21.0.13+9", "bin", "javac")) {
		t.Fatalf("Provide: %+v %v", provided, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(provided.Executable)), "legal", "LICENSE")); !os.IsNotExist(err) {
		t.Errorf("the archive's links are skipped: %v", err)
	}
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{}}, func(string) {}); err != nil || downloads != 1 {
		t.Errorf("a second task reuses the install: %d downloads, %v", downloads, err)
	}
	hostPlatform = "freebsd/amd64"
	if _, err := Provide(context.Background(), tc, []stacks.Expected{{}}, func(string) {}); err == nil {
		t.Error("a platform with no archive fails")
	}
}

// jdkArchive is a tar.gz with jdk-21.0.13+9/bin/javac and a link under legal/.
func jdkArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho 'javac 21.0.13'\n")
	for _, h := range []*tar.Header{
		{Name: "jdk-21.0.13+9/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "jdk-21.0.13+9/bin/javac", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))},
		{Name: "jdk-21.0.13+9/legal/LICENSE", Typeflag: tar.TypeSymlink, Linkname: "../../LICENSE"},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mavenArchive is a tar.gz that holds apache-maven-<version>/bin/mvn.
func mavenArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho maven " + version + "\n")
	for _, h := range []*tar.Header{
		{Name: "apache-maven-" + version + "/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "apache-maven-" + version + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "apache-maven-" + version + "/bin/mvn", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mavenHost serves Maven archives and the checksums of their host: the
// archive at <name>, its SHA-512 at <name>.sha512 unless noSHA512 names it.
type mavenHost struct {
	files     map[string][]byte
	downloads map[string]int
}

func newMavenHost(t *testing.T) (*mavenHost, *httptest.Server) {
	t.Helper()
	h := &mavenHost{files: map[string][]byte{}, downloads: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := h.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.downloads[r.URL.Path]++
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return h, server
}

func TestAMavenComponentGetsMavenWhenTheMachineHasNone(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-maven-component-gets-maven-when-the-machine-has-none")
	host, server := newMavenHost(t)
	data := mavenArchive(t, mavenVersion)
	host.files["/maven.tar.gz"] = data
	sum := sha256.Sum256(data)
	oldURL, oldSHA := mavenURL, mavenSHA256
	mavenURL, mavenSHA256 = server.URL+"/maven.tar.gz", hex.EncodeToString(sum[:])
	defer func() { mavenURL, mavenSHA256 = oldURL, oldSHA }()
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	empty := []string{"PATH=" + t.TempDir()}
	ctx := context.Background()
	root := t.TempDir()

	mvn, err := mavenProgram(ctx, root, empty, func(string) {})
	if err != nil || !strings.HasSuffix(mvn, filepath.Join("maven-"+mavenVersion, "apache-maven-"+mavenVersion, "bin", "mvn")) {
		t.Fatalf("install: %q %v", mvn, err)
	}
	if again, err := mavenProgram(ctx, root, empty, func(string) {}); err != nil || again != mvn || host.downloads["/maven.tar.gz"] != 1 {
		t.Errorf("a second task reuses the install: %d downloads, %v", host.downloads["/maven.tar.gz"], err)
	}
	if found, err := mavenProgram(ctx, root, []string{"PATH=" + filepath.Dir(mvn)}, func(string) {}); err != nil || found != mvn {
		t.Errorf("a mvn on PATH runs as is: %q %v", found, err)
	}
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	mavenSHA256 = strings.Repeat("0", 64)
	if _, err := mavenProgram(ctx, root, empty, func(string) {}); err == nil {
		t.Error("an archive with another digest is refused")
	}
}

func TestAMavenBuildRunsTheReleaseItsWrapperStates(t *testing.T) {
	spectest.Proves(t, feature, "maven-follows-its-wrapper", "a-maven-build-runs-the-release-its-wrapper-states")
	host, server := newMavenHost(t)
	stated := mavenArchive(t, "3.9.6")
	host.files["/maven2/apache-maven-3.9.6-bin.tar.gz"] = stated
	sum512 := sha512.Sum512(stated)
	host.files["/maven2/apache-maven-3.9.6-bin.tar.gz.sha512"] = []byte(hex.EncodeToString(sum512[:]) + "  apache-maven-3.9.6-bin.tar.gz\n")
	ctx := context.Background()
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	root := t.TempDir()
	// A mvn on PATH does not run in place of the stated release.
	machine := t.TempDir()
	stackstest.WriteFile(t, machine, "mvn", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(machine, "mvn"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + machine}
	stackstest.WriteFile(t, root, mavenWrapperProperties, "wrapperVersion=3.3.2\ndistributionType=only-script\ndistributionUrl="+strings.Replace(server.URL, ":", "\\:", 1)+"/maven2/apache-maven-3.9.6-bin.tar.gz\n")

	mvn, err := mavenProgram(ctx, root, env, func(string) {})
	if err != nil || !strings.HasSuffix(mvn, filepath.Join("maven-3.9.6", "apache-maven-3.9.6", "bin", "mvn")) {
		t.Fatalf("the stated release runs: %q %v", mvn, err)
	}
	if host.downloads["/maven2/apache-maven-3.9.6-bin.tar.gz"] != 1 {
		t.Errorf("the archive is downloaded once, checked against its published SHA-512: %v", host.downloads)
	}
	step, err := Step(ctx, stacks.TaskTest, stacks.Metadata{}, stacks.RootStep{Dir: root, Env: env}, []string{"mvn", "--batch-mode", "verify"}, func(string) {})
	if err != nil || len(step) != 3 || step[0] != mvn || step[2] != "verify" {
		t.Errorf("a step that names mvn runs the stated release: %q %v", step, err)
	}

	// A host whose published SHA-512 differs is refused.
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	host.files["/maven2/apache-maven-3.9.6-bin.tar.gz.sha512"] = []byte(strings.Repeat("0", 128))
	if _, err := mavenProgram(ctx, root, env, func(string) {}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("an archive that differs from its published checksum is refused: %v", err)
	}
	// A host that publishes no checksum is refused, and a stated SHA-256 needs none.
	delete(host.files, "/maven2/apache-maven-3.9.6-bin.tar.gz.sha512")
	if _, err := mavenProgram(ctx, root, env, func(string) {}); err == nil || !strings.Contains(err.Error(), "distributionSha256Sum") {
		t.Errorf("an archive with no published checksum is refused: %v", err)
	}
	sum256 := sha256.Sum256(stated)
	stackstest.WriteFile(t, root, mavenWrapperProperties, "distributionUrl="+server.URL+"/maven2/apache-maven-3.9.6-bin.tar.gz\ndistributionSha256Sum="+hex.EncodeToString(sum256[:])+"\n")
	if mvn, err := mavenProgram(ctx, root, env, func(string) {}); err != nil || !strings.Contains(mvn, "apache-maven-3.9.6") {
		t.Errorf("a stated SHA-256 pins the release: %q %v", mvn, err)
	}
	stackstest.WriteFile(t, root, mavenWrapperProperties, "distributionUrl="+server.URL+"/maven2/apache-maven-3.9.6-bin.tar.gz\ndistributionSha256Sum="+strings.Repeat("1", 64)+"\n")
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	if _, err := mavenProgram(ctx, root, env, func(string) {}); err == nil {
		t.Error("an archive that differs from the stated SHA-256 is refused")
	}

	// Offline, the installed release runs and a missing one fails.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if _, err := mavenProgram(ctx, root, env, func(string) {}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("an offline run installs nothing: %v", err)
	}

	for _, bad := range []string{"distributionUrl=file:///tmp/apache-maven-3.9.6-bin.zip", "distributionUrl=https://user:secret@example.test/apache-maven-3.9.6-bin.zip", "distributionUrl=https://example.test/maven.zip"} {
		stackstest.WriteFile(t, root, mavenWrapperProperties, bad+"\n")
		if _, err := mavenProgram(ctx, root, env, func(string) {}); err == nil {
			t.Errorf("%s is refused", bad)
		}
	}
	if d, ok, err := statedMaven(t.TempDir()); ok || err != nil {
		t.Errorf("a build with no wrapper states no Maven: %+v %v", d, err)
	}
}

func TestJavaVersionsMatchTheirJavac(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "java-8-matches-its-1-8-form")
	for _, c := range []struct {
		expected, machine string
		match             bool
	}{
		{"8", "1.8.0_512", true},
		{"1.8", "1.8.0_512", true},
		{"1.8.0", "1.8.0_512", true},
		{"8.0.512", "1.8.0_512", true},
		{"1.8.0_512", "1.8.0_512", true},
		{"8", "11.0.33", false},
		{"11", "11.0.33", true},
		{"11", "1.8.0_512", false},
		{"21", "21.0.12.1", true},
		{"21.0.13", "21.0.12", false},
		{"17", "17-ea", false},
	} {
		if got := MatchVersion("", stacks.Expected{Version: c.expected}, c.machine); got != c.match {
			t.Errorf("%s against javac %s = %v, want %v", c.expected, c.machine, got, c.match)
		}
	}
	tc := stack(t).Profile.Toolchain
	root := t.TempDir()
	stackstest.WriteFile(t, root, ".java-version", "# the team's JDK\n1.8.0_512\n")
	expected, err := tc.ExpectedVersion(root, ".", ".")
	if err != nil || expected.Version != "1.8.0" {
		t.Errorf(".java-version 1.8.0_512 states %+v, %v", expected, err)
	}
	if v := tc.CompareWith(expected, "1.8.0_392", matchesJava); v.Fail || v.Code != "" {
		t.Errorf("a Java 8 javac satisfies 1.8.0_512 as 1.8.0: %+v", v)
	}
}

func TestTheProvidedJDKIsTheTasksJavaHome(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-pinned-jdk-runs-when-the-machine-has-none")
	g := gradleFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	// The machine's javac is replaced by one outside .java-version; the pinned
	// JDK's javac answers 21.
	stackstest.WriteFile(t, filepath.Dir(log), "javac", "#!/bin/sh\necho 'javac 17.0.9'\n")
	home := t.TempDir()
	pinned := filepath.Join(home, "bin", "javac")
	stackstest.WriteFile(t, home, "bin/javac", "#!/bin/sh\necho 'javac 21.0.13'\n")
	if err := os.Chmod(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_JAVA_EXECUTABLE", pinned)
	t.Setenv("JAVA_HOME", "/elsewhere")
	stackstest.WriteFile(t, g, "gradlew", "#!/bin/sh\necho \"JAVA_HOME=$JAVA_HOME\" >> \""+log+"\"\n")
	if err := os.Chmod(filepath.Join(g, "gradlew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if status, _, err := runJob(t, g, "api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	if !strings.Contains(ran(t, log), "JAVA_HOME="+home+"\n") {
		t.Errorf("the build runs with the pinned JDK's home: %s", ran(t, log))
	}
}
