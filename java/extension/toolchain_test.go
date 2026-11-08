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
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks"
)

func resolveJava(t *testing.T, root, committed, platform string) (wsproto.ToolchainResolution, bool) {
	t.Helper()
	request := wsproto.ToolchainResolveRequest{Version: wsproto.ToolchainResolveProtocolVersion, Extension: ExtensionName, Identities: []string{LockIdentity}, Platform: platform}
	if committed != "" {
		request.Locked = map[string]wsproto.LockToolchainEntry{LockIdentity: {Version: committed}}
	}
	got, ok := answerToolchainResolve(root, request, stack(t).Profile).Toolchains[LockIdentity]
	return got, ok
}

func TestInstallPinsTheJDKTheBuildsState(t *testing.T) {
	spectest.Proves(t, feature, "toolchain-is-provided", "install-pins-the-jdk-the-builds-state")
	g := gradleFixture(t)
	if got, ok := resolveJava(t, g, "", "linux/amd64"); !ok || got.Pin.Version != "21.0.13" {
		t.Fatalf(".java-version 21 pins 21.0.13: %+v", got)
	} else {
		a := got.Pin.Archives["linux/amd64"]
		if !strings.HasPrefix(a.URL, "https://github.com/adoptium/temurin21-binaries/") || a.Executable != "jdk-21.0.13+9/bin/javac" || got.Pin.Integrities["linux/amd64"] != "d7f5d5cf865a2902b70ff549b97586a7d3f234e016f041ab447ea740c7d66d1c" {
			t.Errorf("linux archive = %+v %v", a, got.Pin.Integrities)
		}
		if mac := got.Pin.Archives["darwin/arm64"]; mac.Executable != "jdk-21.0.13+9/Contents/Home/bin/javac" {
			t.Errorf("a macOS JDK nests its home: %+v", mac)
		}
		if win := got.Pin.Archives["windows/amd64"]; win.Format != wsproto.LockArchiveFormatZip || !strings.HasSuffix(win.Executable, "javac.exe") {
			t.Errorf("a Windows JDK is a zip: %+v", win)
		}
	}
	writeFile(t, g, ".java-version", "17\n")
	if got, _ := resolveJava(t, g, "", "linux/amd64"); got.Pin == nil || got.Pin.Version != "17.0.21" {
		t.Errorf(".java-version 17 pins 17.0.21: %+v", got)
	}
	writeFile(t, g, "api/.java-version", "25\n")
	if got, _ := resolveJava(t, g, "", "linux/amd64"); got.Pin == nil || got.Pin.Version != "17.0.21" {
		t.Errorf("when no release suits every build, the root's statement wins: %+v", got)
	}
	if err := os.Remove(filepath.Join(g, ".java-version")); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveJava(t, g, "", "linux/amd64"); got.Pin == nil || got.Pin.Version != "25.0.5" {
		t.Errorf("a component's statement narrows the pin: %+v", got)
	}
	if err := os.Remove(filepath.Join(g, "api/.java-version")); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveJava(t, g, "17.0.21", "linux/amd64"); got.Pin == nil || got.Pin.Version != "17.0.21" {
		t.Errorf("an accepted committed pin is kept: %+v", got)
	}
	if _, ok := resolveJava(t, g, "", "freebsd/amd64"); ok {
		t.Error("a platform with no archive pins nothing")
	}
	if _, ok := resolveJava(t, t.TempDir(), "", "linux/amd64"); ok {
		t.Error("a workspace with no Gradle or Maven build pins nothing")
	}
	request := wsproto.ToolchainResolveRequest{Version: wsproto.ToolchainResolveProtocolVersion, Extension: ExtensionName, Identities: []string{"maven"}, Platform: "linux/amd64"}
	if got := answerToolchainResolve(g, request, stack(t).Profile).Toolchains["maven"]; got.Problem == nil {
		t.Errorf("another identity is a problem: %+v", got)
	}
	for _, r := range jdks {
		if len(r.archives) != 5 {
			t.Errorf("%s ships %d platforms", r.version, len(r.archives))
		}
	}
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
