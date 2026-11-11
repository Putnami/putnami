package java

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// stubSDKManager accepts the licenses when it reads y, under the --sdk_root it
// is given, and records the JAVA_HOME it ran with.
const stubSDKManager = `#!/bin/sh
for a in "$@"; do
  case "$a" in --sdk_root=*) sdk="${a#--sdk_root=}" ;; esac
done
read answer
[ "$answer" = y ] || exit 3
mkdir -p "$sdk/licenses"
echo accepted > "$sdk/licenses/android-sdk-license"
echo "$JAVA_HOME" > "$sdk/java-home"
`

// cmdlineToolsArchive is a stub archive of the command-line tools, laid out
// as Google's: cmdline-tools/bin/sdkmanager.
func cmdlineToolsArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: "cmdline-tools/bin/sdkmanager", Method: zip.Deflate}
	header.SetMode(0o755)
	f, err := w.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(stubSDKManager)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// androidWorkspace writes a Gradle build whose app project applies the Android
// application plugin and whose core project is a plain library.
func androidWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stackstest.WriteFile(t, root, "settings.gradle.kts", "include(\":app\", \":core\")\n")
	stackstest.WriteFile(t, root, "build.gradle.kts", "// plugins { id(\"com.android.application\") }\n")
	stackstest.WriteFile(t, root, "app/build.gradle.kts", "plugins {\n    id(\"com.android.application\")\n}\n")
	stackstest.WriteFile(t, root, "core/build.gradle.kts", "plugins {\n    `java-library`\n}\n")
	return root
}

// realLicenseJava is the JDK sdkmanager runs on, which sdkServer stubs.
var realLicenseJava = licenseJava

// sdkServer serves the stub archive as every platform's pinned archive, after
// delay, and counts the downloads. Another download goes where downloadURL
// sent it before.
func sdkServer(t *testing.T, delay time.Duration) *atomic.Int32 {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub sdkmanager is a shell script")
	}
	data := cmdlineToolsArchive(t)
	downloads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		time.Sleep(delay)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	sum := sha256.Sum256(data)
	oldTools, oldPlatform, oldURL, oldJava := cmdlineTools, hostPlatform, downloadURL, licenseJava
	cmdlineTools = map[string]cmdlineArchive{"linux/amd64": {"commandlinetools-linux-15859902_latest.zip", hex.EncodeToString(sum[:])}}
	hostPlatform = "linux/amd64"
	downloadURL = func(url string) string {
		if name, ok := strings.CutPrefix(url, androidRepository); ok {
			return server.URL + "/" + name
		}
		return oldURL(url)
	}
	licenseJava = func(context.Context, func(string)) (string, error) { return "/pinned/jdk-21", nil }
	t.Cleanup(func() { cmdlineTools, hostPlatform, downloadURL, licenseJava = oldTools, oldPlatform, oldURL, oldJava })
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	return downloads
}

var acceptLicenses = json.RawMessage(`{"android": {"acceptSdkLicenses": true}}`)

func androidRequest(root, variant string, options json.RawMessage, offline bool, env ...string) stacks.EnvRequest {
	component := ":app:"
	if variant == VariantGradle {
		component = ":core:"
	}
	return stacks.EnvRequest{Task: stacks.TaskBuild, Root: root, Meta: stacks.Metadata{Component: component, Variant: variant, Root: "."}, Env: env, Options: options, Offline: offline, Info: func(string) {}}
}

func TestAnAndroidTaskKeepsTheMachineSDK(t *testing.T) {
	spectest.Proves(t, feature, "android-sdk-is-provided", "an-android-task-keeps-the-machine-sdk")
	downloads := sdkServer(t, 0)
	root := androidWorkspace(t)
	if s, err := Stack(); err != nil || s.Env == nil {
		t.Fatalf("the stack sets no Env hook: %v", err)
	}
	for _, env := range []string{"ANDROID_HOME=/opt/android", "ANDROID_SDK_ROOT=/opt/android"} {
		if entries, err := Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, false, "PATH=/bin", env)); err != nil || entries != nil {
			t.Errorf("%s: %v %v", env, entries, err)
		}
	}
	if _, err := Env(context.Background(), androidRequest(root, VariantAndroid, nil, false, "ANDROID_HOME=")); err == nil {
		t.Error("an empty ANDROID_HOME names no SDK")
	}
	stackstest.WriteFile(t, root, "local.properties", "# written by Android Studio\nsdk.dir=/Users/dev/Library/Android/sdk\n")
	if entries, err := Env(context.Background(), androidRequest(root, VariantAndroidCoverage, nil, false)); err != nil || entries != nil {
		t.Errorf("sdk.dir in local.properties: %v %v", entries, err)
	}
	if downloads.Load() != 0 {
		t.Errorf("a machine SDK downloads nothing: %d downloads", downloads.Load())
	}
}

func TestAnAndroidTaskInstallsTheSDKTheWorkspaceAccepts(t *testing.T) {
	spectest.Proves(t, feature, "android-sdk-is-provided", "an-android-task-installs-the-sdk-the-workspace-accepts")
	if len(cmdlineTools) != 4 {
		t.Errorf("the command-line tools are pinned for %d platforms, want 4", len(cmdlineTools))
	}
	for platform, a := range cmdlineTools {
		if !strings.Contains(a.name, "-15859902_latest.zip") || len(a.sha256) != 64 {
			t.Errorf("%s = %+v", platform, a)
		}
	}
	if a := cmdlineTools["linux/amd64"]; a.sha256 != "4e4c464f145a7512b57d088ac6c278c03c9eea610886b35a5e0804e74eedf583" {
		t.Errorf("the Linux archive = %+v", a)
	}
	downloads := sdkServer(t, 0)
	root := androidWorkspace(t)
	var infos []string
	request := androidRequest(root, VariantAndroid, acceptLicenses, false)
	request.Info = func(line string) { infos = append(infos, line) }
	entries, err := Env(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(os.Getenv("PUTNAMI_HOME"), "toolchains", "android", "sdk")
	if want := []string{"ANDROID_HOME=" + sdk, "ANDROID_SDK_ROOT=" + sdk}; !slices.Equal(entries, want) {
		t.Errorf("entries = %v, want %v", entries, want)
	}
	if got, _ := os.ReadFile(filepath.Join(sdk, "java-home")); strings.TrimSpace(string(got)) != "/pinned/jdk-21" {
		t.Errorf("sdkmanager ran on JAVA_HOME=%q", got)
	}
	if !slices.ContainsFunc(infos, func(line string) bool { return strings.Contains(line, "accepting the Android SDK licenses") }) {
		t.Errorf("the license acceptance is not reported: %v", infos)
	}
	// Another component of the Android build gets the installed SDK, with no
	// second download.
	entries, err = Env(context.Background(), androidRequest(root, VariantGradle, acceptLicenses, false))
	if err != nil || len(entries) != 2 || downloads.Load() != 1 {
		t.Errorf("a second task: %v %v, %d downloads", entries, err, downloads.Load())
	}
	// A plain Gradle build needs no SDK.
	plain := t.TempDir()
	stackstest.WriteFile(t, plain, "settings.gradle.kts", "include(\":core\")\n")
	stackstest.WriteFile(t, plain, "core/build.gradle.kts", "plugins {\n    `java-library`\n}\n")
	if entries, err := Env(context.Background(), androidRequest(plain, VariantGradle, json.RawMessage(`not json`), false)); err != nil || entries != nil {
		t.Errorf("a plain Gradle build: %v %v", entries, err)
	}
	// An interrupted sdkmanager leaves an empty license file: the next online
	// task accepts the licenses again, with no second download.
	if err := os.WriteFile(licenseFile(sdk), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, false)); err != nil || len(entries) != 2 || !licensesAccepted(sdk) || downloads.Load() != 1 {
		t.Errorf("an empty license file: %v %v, %d downloads", entries, err, downloads.Load())
	}
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	hostPlatform = "linux/arm64"
	for _, options := range []json.RawMessage{acceptLicenses, nil} {
		_, err := Env(context.Background(), androidRequest(root, VariantAndroid, options, false))
		if err == nil || !strings.Contains(err.Error(), "pins no Android SDK command-line tools for linux/arm64: set ANDROID_HOME, or sdk.dir") || strings.Contains(err.Error(), "acceptSdkLicenses") {
			t.Errorf("%s: a platform with no pinned archive: %v", options, err)
		}
		if entries, err := Env(context.Background(), androidRequest(root, VariantGradle, options, false)); err != nil || entries != nil {
			t.Errorf("%s: another component on a platform with no pinned archive is left to Gradle: %v %v", options, entries, err)
		}
	}
}

func TestConcurrentAndroidTasksInstallTheSDKOnce(t *testing.T) {
	downloads := sdkServer(t, 200*time.Millisecond)
	root := androidWorkspace(t)
	var wg sync.WaitGroup
	results := make([][]string, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, false))
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !slices.Equal(results[0], results[1]) || len(results[0]) != 2 || downloads.Load() != 1 {
		t.Errorf("two tasks at once: %v %v, %v %v, %d downloads", results[0], errs[0], results[1], errs[1], downloads.Load())
	}
}

func TestNoJavaCommandDeclaresAFlagTheOptionsShadow(t *testing.T) {
	data, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]json.RawMessage `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, command := range manifest.Commands {
		if _, ok := command.Flags["android"]; ok {
			t.Errorf("the %s command declares a flag named android, which options[%q].android would set on every run", name, ExtensionName)
		}
	}
}

func TestAnAndroidTaskWithoutTheOptionNamesIt(t *testing.T) {
	spectest.Proves(t, feature, "android-sdk-is-provided", "an-android-task-without-the-option-names-it")
	downloads := sdkServer(t, 0)
	root := androidWorkspace(t)
	for _, options := range []json.RawMessage{nil, json.RawMessage(`{"android": {}}`), json.RawMessage(`{"android": {"acceptSdkLicenses": false}}`)} {
		_, err := Env(context.Background(), androidRequest(root, VariantAndroid, options, false))
		if err == nil || !strings.Contains(err.Error(), `{"@putnami/java": {"android": {"acceptSdkLicenses": true}}}`) || !strings.Contains(err.Error(), "putnami.workspace.json") {
			t.Errorf("%s: %v", options, err)
		}
	}
	if entries, err := Env(context.Background(), androidRequest(root, VariantGradle, nil, false)); err != nil || entries != nil {
		t.Errorf("another component of the build is left to Gradle: %v %v", entries, err)
	}
	job := androidRequest(root, VariantGradle, nil, false)
	job.Task = ""
	if _, err := Env(context.Background(), job); err == nil || !strings.Contains(err.Error(), "acceptSdkLicenses") {
		t.Errorf("a job at the stack root of the build, such as its install, names the option: %v", err)
	}
	if _, err := Env(context.Background(), androidRequest(root, VariantAndroid, json.RawMessage(`{"android": true}`), false)); err == nil || !strings.Contains(err.Error(), "putnami.workspace.json") {
		t.Errorf("invalid options: %v", err)
	}
	if downloads.Load() != 0 {
		t.Errorf("no option downloads nothing: %d downloads", downloads.Load())
	}
}

func TestAnOfflineAndroidTaskInstallsNothing(t *testing.T) {
	spectest.Proves(t, feature, "android-sdk-is-provided", "an-offline-android-task-installs-nothing")
	downloads := sdkServer(t, 0)
	root := androidWorkspace(t)
	if _, err := Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, true)); err == nil || !strings.Contains(err.Error(), "an offline run installs nothing") {
		t.Errorf("an offline Android task with no SDK: %v", err)
	}
	if entries, err := Env(context.Background(), androidRequest(root, VariantGradle, acceptLicenses, true)); err != nil || entries != nil {
		t.Errorf("another component is left to Gradle: %v %v", entries, err)
	}
	if downloads.Load() != 0 {
		t.Errorf("an offline run downloads nothing: %d downloads", downloads.Load())
	}
	sdk := filepath.Join(os.Getenv("PUTNAMI_HOME"), "toolchains", "android", "sdk")
	stackstest.WriteFile(t, sdk, "licenses/android-sdk-license", "")
	if _, err := Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, true)); err == nil || !strings.Contains(err.Error(), licenseFile(sdk)+" holds no accepted license") {
		t.Errorf("an offline task with an empty license file names it: %v", err)
	}
	stackstest.WriteFile(t, sdk, "licenses/android-sdk-license", "accepted\n")
	if entries, err := Env(context.Background(), androidRequest(root, VariantAndroid, acceptLicenses, true)); err != nil || len(entries) != 2 || entries[0] != "ANDROID_HOME="+sdk {
		t.Errorf("an offline task uses the installed SDK: %v %v", entries, err)
	}
}
