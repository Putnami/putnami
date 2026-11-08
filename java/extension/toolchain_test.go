package java

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/stacks"
)

func TestATaskInstallsTheJDKItsBuildStates(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-task-installs-the-jdk-its-build-states")
	tc := stack(t).Profile.Toolchain
	for expected, want := range map[string]string{"": "21.0.13", "17": "17.0.21", "21": "21.0.13", "25": "25.0.5", "25.0.5": "25.0.5"} {
		got, ok := choose(acceptedBy(tc, jdks, stacks.Expected{Version: expected, Source: ".java-version"}), "")
		if !ok || got.version != want {
			t.Errorf(".java-version %q installs %s, want %s", expected, got.version, want)
		}
	}
	if _, err := Provide(tc)(context.Background(), stacks.Expected{Version: "11", Source: ".java-version"}, func(string) {}); err == nil || !strings.Contains(err.Error(), "no Temurin JDK") {
		t.Errorf("a version the extension pins no JDK for fails: %v", err)
	}
	for _, r := range jdks {
		if len(r.archives) != 5 {
			t.Errorf("%s ships %d platforms", r.version, len(r.archives))
		}
		major := strings.SplitN(r.version, ".", 2)[0]
		for platform, a := range r.archives {
			if !strings.HasPrefix(a.url, "https://github.com/adoptium/temurin"+major+"-binaries/releases/download/") || len(a.sha256) != 64 || !strings.HasSuffix(a.executable, "/javac") && !strings.HasSuffix(a.executable, "/javac.exe") {
				t.Errorf("%s %s = %+v", r.version, platform, a)
			}
		}
	}
	if a := jdks[1].archives["linux/amd64"]; a.sha256 != "d7f5d5cf865a2902b70ff549b97586a7d3f234e016f041ab447ea740c7d66d1c" || a.executable != "jdk-21.0.13+9/bin/javac" {
		t.Errorf("the linux JDK 21 = %+v", a)
	}
	if a := jdks[1].archives["darwin/arm64"]; a.executable != "jdk-21.0.13+9/Contents/Home/bin/javac" {
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
	provided, err := Provide(tc)(context.Background(), stacks.Expected{Version: "21", Source: ".java-version"}, func(string) {})
	if err != nil || !strings.HasSuffix(provided.Executable, filepath.Join("java-21.0.13", "jdk-21.0.13+9", "bin", "javac")) {
		t.Fatalf("Provide: %+v %v", provided, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(provided.Executable)), "legal", "LICENSE")); !os.IsNotExist(err) {
		t.Errorf("the archive's links are skipped: %v", err)
	}
	if _, err := Provide(tc)(context.Background(), stacks.Expected{}, func(string) {}); err != nil || downloads != 1 {
		t.Errorf("a second task reuses the install: %d downloads, %v", downloads, err)
	}
	hostPlatform = "freebsd/amd64"
	if _, err := Provide(tc)(context.Background(), stacks.Expected{}, func(string) {}); err == nil {
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
func mavenArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho maven\n")
	for _, h := range []*tar.Header{
		{Name: "apache-maven-" + mavenVersion + "/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "apache-maven-" + mavenVersion + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "apache-maven-" + mavenVersion + "/bin/mvn", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))},
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

func TestAMavenComponentGetsMavenWhenTheMachineHasNone(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-maven-component-gets-maven-when-the-machine-has-none")
	data := mavenArchive(t)
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write(data)
	}))
	defer server.Close()
	sum := sha256.Sum256(data)
	oldURL, oldSHA := mavenURL, mavenSHA256
	mavenURL, mavenSHA256 = server.URL+"/maven.tar.gz", hex.EncodeToString(sum[:])
	defer func() { mavenURL, mavenSHA256 = oldURL, oldSHA }()
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	empty := []string{"PATH=" + t.TempDir()}
	ctx := context.Background()

	if dirs, err := Tools(ctx, stacks.TaskTest, stacks.Metadata{Variant: VariantGradle}, empty, func(string) {}); err != nil || dirs != nil {
		t.Errorf("a Gradle component needs no Maven: %v %v", dirs, err)
	}
	dirs, err := Tools(ctx, stacks.TaskTest, stacks.Metadata{Variant: VariantMaven}, empty, func(string) {})
	if err != nil || len(dirs) != 1 {
		t.Fatalf("install: %v %v", dirs, err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "mvn")); err != nil {
		t.Errorf("the installed bin holds mvn: %v", err)
	}
	if _, err := Tools(ctx, stacks.TaskLint, stacks.Metadata{Variant: VariantMaven}, empty, func(string) {}); err != nil || downloads != 1 {
		t.Errorf("a second task reuses the install: %d downloads, %v", downloads, err)
	}
	if dirs, err := Tools(ctx, stacks.TaskTest, stacks.Metadata{Variant: VariantMaven}, []string{"PATH=" + dirs[0]}, func(string) {}); err != nil || dirs != nil {
		t.Errorf("a mvn on PATH runs as is: %v %v", dirs, err)
	}

	mavenSHA256 = strings.Repeat("0", 64)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	if _, err := Tools(ctx, stacks.TaskTest, stacks.Metadata{Variant: VariantMaven}, empty, func(string) {}); err == nil {
		t.Error("an archive with another digest is refused")
	}
}

func TestTheProvidedJDKIsTheTasksJavaHome(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "a-pinned-jdk-runs-when-the-machine-has-none")
	g := gradleFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	// The machine's javac is replaced by one outside .java-version; the pinned
	// JDK's javac answers 21.
	writeFile(t, filepath.Dir(log), "javac", "#!/bin/sh\necho 'javac 17.0.9'\n")
	home := t.TempDir()
	pinned := filepath.Join(home, "bin", "javac")
	writeFile(t, home, "bin/javac", "#!/bin/sh\necho 'javac 21.0.13'\n")
	if err := os.Chmod(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_JAVA_EXECUTABLE", pinned)
	t.Setenv("JAVA_HOME", "/elsewhere")
	writeFile(t, g, "gradlew", "#!/bin/sh\necho \"JAVA_HOME=$JAVA_HOME\" >> \""+log+"\"\n")
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
