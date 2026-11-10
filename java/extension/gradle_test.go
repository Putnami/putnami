package java

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

func TestTheJavaVersionsAGradleBuildAsksForAreRead(t *testing.T) {
	spectest.Proves(t, feature, "gradle-toolchains-are-provided", "the-toolchain-versions-of-the-build-are-read")
	root := t.TempDir()
	stackstest.WriteFile(t, root, "settings.gradle.kts", "include(\":api\", \":legacy\", \":tools\")\n")
	stackstest.WriteFile(t, root, "build.gradle.kts", "subprojects {\n    // jvmToolchain(25)\n}\n")
	stackstest.WriteFile(t, root, "api/build.gradle.kts", "kotlin {\n    jvmToolchain(17)\n}\n")
	stackstest.WriteFile(t, root, "legacy/build.gradle", "java {\n    toolchain {\n        languageVersion = JavaLanguageVersion.of('11')\n    }\n}\n")
	stackstest.WriteFile(t, root, "tools/build.gradle.kts", "java.toolchain.languageVersion.set(JavaLanguageVersion.of(libs.versions.tools.jdk.get().toInt()))\n")
	stackstest.WriteFile(t, root, "gradle/libs.versions.toml", "[versions]\ntools-jdk = \"21\"\nkotlin = \"2.0.21\"\n[libraries]\njdk = \"x:y:9\"\n")
	stackstest.WriteFile(t, root, "gradle/quality.gradle", "javaToolchains.launcherFor { languageVersion = JavaLanguageVersion.of(JavaVersion.VERSION_1_8.majorVersion) }\n")
	stackstest.WriteFile(t, root, "buildSrc/src/main/kotlin/conventions.gradle.kts", "java {\n    toolchain.languageVersion = JavaLanguageVersion.of(\"16\")\n}\n")
	stackstest.WriteFile(t, root, "buildSrc/build/generated/Stale.kt", "JavaLanguageVersion.of(9)\n")
	stackstest.WriteFile(t, root, "build-logic/convention/src/main/java/Conventions.java", "class Conventions { void apply() { JavaLanguageVersion.of(13); } }\n")
	stackstest.WriteFile(t, root, "build-logic/convention/build.gradle.kts", "kotlin { jvmToolchain(javaVersion) }\n")
	stackstest.WriteFile(t, root, "gradle/gradle-daemon-jvm.properties", "toolchainVendor=ADOPTIUM\ntoolchainVersion=24\n")
	if got, want := requestedJavaMajors(root), []int{8, 11, 13, 16, 17, 21, 24}; !slices.Equal(got, want) {
		t.Errorf("the build asks for %v, want %v", got, want)
	}
	if got := requestedJavaMajors(t.TempDir()); got != nil {
		t.Errorf("a build without toolchains asks for none: %v", got)
	}
}

// jdkTarGz is a JDK archive whose javac prints version.
func jdkTarGz(t *testing.T, dir, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho 'javac " + version + "'\n")
	if err := tw.WriteHeader(&tar.Header{Name: dir + "/bin/javac", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveJDKs pins stub JDKs 11 and 17, served by a local server, in place of
// the shipped ones, and answers how often each was downloaded.
func serveJDKs(t *testing.T) map[string]int {
	t.Helper()
	files := map[string][]byte{"/11.tar.gz": jdkTarGz(t, "jdk-11", "11.0.33"), "/17.tar.gz": jdkTarGz(t, "jdk-17", "17.0.21")}
	downloads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads[r.URL.Path]++
		_, _ = w.Write(files[r.URL.Path])
	}))
	t.Cleanup(server.Close)
	pin := func(name, dir string) archive {
		sum := sha256.Sum256(files[name])
		return archive{server.URL + name, wsproto.LockArchiveFormatTarGz, dir + "/bin/javac", hex.EncodeToString(sum[:])}
	}
	oldJDKs, oldPlatform, oldURL := jdks, hostPlatform, downloadURL
	jdks = []jdk{
		{"11.0.33", map[string]archive{"linux/amd64": pin("/11.tar.gz", "jdk-11")}},
		{"17.0.21", map[string]archive{"linux/amd64": pin("/17.tar.gz", "jdk-17")}},
		{"21.0.13", map[string]archive{"linux/amd64": pin("/17.tar.gz", "jdk-17")}},
	}
	hostPlatform = "linux/amd64"
	downloadURL = func(url string) string { return url }
	t.Cleanup(func() { jdks, hostPlatform, downloadURL = oldJDKs, oldPlatform, oldURL })
	return downloads
}

func TestAGradleStepGetsTheJDKsItsToolchainsAskFor(t *testing.T) {
	spectest.Proves(t, feature, "gradle-toolchains-are-provided", "each-gradle-step-gets-the-jdks-the-toolchains-ask-for")
	g := gradleFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	stackstest.WriteFile(t, g, "api/build.gradle.kts", "plugins {\n    alias(libs.plugins.kotlin.jvm)\n    jacoco\n}\nkotlin { jvmToolchain(17) }\ndependencies {\n    implementation(projects.pricing)\n}\n")
	stackstest.WriteFile(t, g, "pricing/build.gradle.kts", "plugins {\n    alias(libs.plugins.kotlin.jvm)\n    jacoco\n}\njava { toolchain { languageVersion = JavaLanguageVersion.of(11) } }\n")
	stackstest.WriteFile(t, g, "libs/model/build.gradle.kts", "plugins {\n    `java-library`\n}\njava { toolchain { languageVersion = JavaLanguageVersion.of(21) } }\nkotlin { jvmToolchain(23) }\n")
	downloads := serveJDKs(t)
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	if status, _, err := runJob(t, g, "bom", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	// 21 runs Gradle; 23 is left to Gradle.
	want := "-Porg.gradle.java.installations.paths=" + filepath.Join(home, "toolchains/java/java-11.0.33/jdk-11") + "," + filepath.Join(home, "toolchains/java/java-17.0.21/jdk-17") + "\n"
	if got := ran(t, log); !strings.Contains(got, ":bom:assemble "+want) {
		t.Errorf("the step of a component that asks for none gets every JDK the build asks for: %s", got)
	}
	if _, _, err := runJob(t, g, "api", stacks.TaskTest); err != nil {
		t.Fatal(err)
	}
	if got := ran(t, log); !strings.Contains(got, ":api:jacocoTestReport "+want) || downloads["/11.tar.gz"] != 1 || downloads["/17.tar.gz"] != 1 {
		t.Errorf("a later task reuses the installed JDKs: %v %s", downloads, got)
	}

	// Offline, a JDK that is not installed is left out.
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if status, _, err := runJob(t, g, "bom", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("offline build: %s %v", status, err)
	}
	if got := ran(t, log); strings.Contains(got, gradleInstallationsProperty) || downloads["/11.tar.gz"] != 1 {
		t.Errorf("an offline run installs nothing: %v %s", downloads, got)
	}
}

// gradleZip is a Gradle distribution whose launcher logs its arguments.
func gradleZip(t *testing.T, version, log string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "gradle-" + version + "/bin/gradle", Method: zip.Deflate}
	h.SetMode(0o755)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\necho \"$PWD: gradle-" + version + " $*\" >> \"" + log + "\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestABuildWithoutAWrapperRunsThePinnedGradle(t *testing.T) {
	spectest.Proves(t, feature, "gradle-without-a-wrapper", "a-build-without-a-wrapper-runs-the-pinned-gradle")
	g := gradleFixture(t)
	log := stubTools(t, g, "21.0.4", junitPass)
	if err := os.Remove(filepath.Join(g, "gradlew")); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"/9.8.2": gradleZip(t, "9.8.2", log), "/8.14.6": gradleZip(t, "8.14.6", log)}
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		_, _ = w.Write(files[r.URL.Path])
	}))
	defer server.Close()
	oldReleases, oldURL := gradleReleases, gradleURL
	defer func() { gradleReleases, gradleURL = oldReleases, oldURL }()
	sum := func(name string) string { s := sha256.Sum256(files[name]); return hex.EncodeToString(s[:]) }
	gradleReleases = []gradleRelease{{"9.8.2", 17, 25, sum("/9.8.2")}, {"8.14.6", 8, 24, sum("/8.14.6")}}
	gradleURL = func(version string) string { return server.URL + "/" + version }
	t.Setenv("PUTNAMI_HOME", t.TempDir())

	if status, _, err := runJob(t, g, "api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	if got := ran(t, log); !strings.Contains(got, "gradle-9.8.2 --console=plain :api:assemble\n") {
		t.Errorf("a JDK 21 runs the newest pinned Gradle: %s", got)
	}
	if status, _, err := runFiltered(t, g, "api", "com.acme.QuotesTest"); status != "OK" || err != nil {
		t.Fatalf("filtered test: %s %v", status, err)
	}
	if got := ran(t, log); !strings.Contains(got, "gradle-9.8.2 --console=plain :api:test :api:jacocoTestReport --init-script ") || downloads != 1 {
		t.Errorf("the pinned Gradle is installed once and takes the test filter: %d downloads, %s", downloads, got)
	}

	// A JDK older than 17 runs the newest pinned Gradle that runs on it.
	stackstest.WriteFile(t, g, ".java-version", "11\n")
	stackstest.WriteFile(t, filepath.Dir(log), "javac", "#!/bin/sh\necho 'javac 11.0.33'\n")
	if status, _, err := runJob(t, g, "api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build on 11: %s %v", status, err)
	}
	if got := ran(t, log); !strings.Contains(got, "gradle-8.14.6 --console=plain :api:assemble\n") {
		t.Errorf("a JDK 11 runs Gradle 8.14.6: %s", got)
	}

	// A step that names gradle runs the one on PATH.
	bin := t.TempDir()
	stackstest.WriteFile(t, bin, "gradle", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(bin, "gradle"), 0o755); err != nil {
		t.Fatal(err)
	}
	step, err := Step(context.Background(), stacks.TaskBuild, stacks.Metadata{}, stacks.RootStep{Dir: g, Env: []string{"PATH=" + bin}}, []string{"gradle", "build"}, func(string) {})
	if err != nil || !slices.Equal(step, []string{"gradle", "build"}) {
		t.Errorf("a gradle on PATH runs as it is: %q %v", step, err)
	}

	// Offline, a Gradle that is not installed fails the task.
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, _, err := runJob(t, g, "api", stacks.TaskBuild); status == "OK" || err == nil {
		t.Errorf("an offline build without a wrapper or an installed Gradle fails: %s", status)
	}
}

func TestGradleRunsOnTheJDKItSupports(t *testing.T) {
	spectest.Proves(t, feature, "gradle-without-a-wrapper", "a-build-without-a-wrapper-runs-the-pinned-gradle")
	for major, want := range map[int]string{8: "8.14.6", 11: "8.14.6", 16: "8.14.6", 17: "9.8.2", 21: "9.8.2", 25: "9.8.2", 26: "9.8.2"} {
		if got := gradleFor(major).version; got != want {
			t.Errorf("JDK %d runs Gradle %s, want %s", major, got, want)
		}
	}
	for _, r := range gradleReleases {
		if len(r.sha256) != 64 || !strings.HasPrefix(gradleURL(r.version), "https://services.gradle.org/distributions/gradle-"+r.version+"-bin.zip") {
			t.Errorf("Gradle %s = %+v", r.version, r)
		}
	}
}
