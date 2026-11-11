package java

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/observe/detect/gradle"
	"go.putnami.dev/tooling/stacks"
)

// cmdlineToolsVersion is the release of the Android SDK command-line tools
// the extension installs, build 15859902: the newest whose sdkmanager accepts
// the licenses. From 23.0, sdkmanager hands over to the Android CLI, which
// downloads itself into ~/.android unchecked, collects usage metrics, and
// writes no license for --licenses, where the Android Gradle plugin still
// reads them from the SDK's licenses directory.
const cmdlineToolsVersion = "22.0"

// androidRepository is where Google publishes the Android SDK archives.
const androidRepository = "https://dl.google.com/android/repository/"

// Environment variables the Android Gradle plugin reads the SDK from.
const (
	envAndroidHome    = "ANDROID_HOME"
	envAndroidSDKRoot = "ANDROID_SDK_ROOT"
)

// cmdlineArchive is one platform's archive of the command-line tools.
type cmdlineArchive struct {
	name   string
	sha256 string
}

// cmdlineTools are the archives of the pinned command-line tools by platform,
// with the SHA-256 of each, computed from the archive Google serves, whose
// SHA-1 is the one Google's repository index (repository2-3.xml) states.
// Google publishes the SDK's build tools for Linux on x64 only.
var cmdlineTools = map[string]cmdlineArchive{
	"linux/amd64":   {"commandlinetools-linux-15859902_latest.zip", "4e4c464f145a7512b57d088ac6c278c03c9eea610886b35a5e0804e74eedf583"},
	"darwin/amd64":  {"commandlinetools-mac_x86_64-15859902_latest.zip", "c5a6378ab5cf7e0d5701921405115befff13e9ff7417fb588389338f8bd050f3"},
	"darwin/arm64":  {"commandlinetools-mac_arm64-15859902_latest.zip", "835b62a26162b229b441d1f6d4680383815a270809eb33522c0d480fa5002c4e"},
	"windows/amd64": {"commandlinetools-win-15859902_latest.zip", "90ae805d20434428bffcb699c290860f19bb5f66a67e6b330067e3de801fb04a"},
}

// androidOptions is the extension's block of the workspace's options.
type androidOptions struct {
	Android struct {
		// AcceptSdkLicenses lets the extension install the Android SDK and
		// accept its licenses on the team's behalf.
		AcceptSdkLicenses bool `json:"acceptSdkLicenses"`
	} `json:"android"`
}

// licenseJava answers the home of the JDK sdkmanager runs on: the pinned
// JDK 21, since the command-line tools need Java 17 or newer. Tests replace it.
var licenseJava = func(ctx context.Context, info func(string)) (string, error) {
	r, ok := jdkOfMajor(21)
	if !ok {
		return "", fmt.Errorf("the extension pins no JDK 21 to run sdkmanager on")
	}
	javac, err := installJDK(ctx, r, info)
	if err != nil {
		return "", err
	}
	return javaHome(javac), nil
}

// Env sets the Android SDK of a Gradle build that applies the Android plugin:
// none when the machine has one, through ANDROID_HOME, ANDROID_SDK_ROOT or
// sdk.dir in the stack root's local.properties; else, when the workspace's
// options accept the SDK's licenses, the SDK the extension installs under the
// Putnami home, as ANDROID_HOME and ANDROID_SDK_ROOT. Without either, the task
// of an Android component fails with a message that names the option, and the
// task of another component of the build is left to Gradle. Offline, it
// installs nothing.
func Env(ctx context.Context, request stacks.EnvRequest) ([]string, error) {
	root := filepath.Join(request.Root, filepath.FromSlash(request.Meta.Root))
	component := request.Meta.Variant == VariantAndroid || request.Meta.Variant == VariantAndroidCoverage
	if !component && !androidBuild(root) {
		return nil, nil
	}
	if machineSDK(root, request.Env) {
		return nil, nil
	}
	var options androidOptions
	if len(request.Options) > 0 {
		if err := json.Unmarshal(request.Options, &options); err != nil {
			return nil, fmt.Errorf("the options of %s in putnami.workspace.json: %w", ExtensionName, err)
		}
	}
	if !options.Android.AcceptSdkLicenses {
		if !component {
			return nil, nil
		}
		return nil, fmt.Errorf(`the Android build at %s needs an Android SDK: set ANDROID_HOME, or sdk.dir in its local.properties, or let %s install one, which accepts the Android SDK licenses on the team's behalf, with "options": {%q: {"android": {"acceptSdkLicenses": true}}} in putnami.workspace.json`, root, ExtensionName, ExtensionName)
	}
	sdk, installed, err := androidSDK(ctx, request.Offline, request.Info)
	if err != nil {
		return nil, err
	}
	if !installed {
		if !component {
			return nil, nil
		}
		return nil, fmt.Errorf("the Android build at %s needs the Android SDK, which is not installed under %s: an offline run installs nothing, so run putnami install with network access", root, sdk)
	}
	return []string{envAndroidHome + "=" + sdk, envAndroidSDKRoot + "=" + sdk}, nil
}

// androidBuild reports whether the Gradle build at root applies the Android
// plugin: its root build file, or the build file of one of its projects, as
// the detector reads them.
func androidBuild(root string) bool {
	if _, text := buildFile(root, "."); androidPlugin.MatchString(text) {
		return true
	}
	if settingsFile(root, ".") == "" {
		return false
	}
	for _, c := range gradle.Detect(root).Components {
		if _, text := buildFile(root, c.ID); androidPlugin.MatchString(text) {
			return true
		}
	}
	return false
}

// machineSDK reports whether the machine names an Android SDK for the build
// at root: ANDROID_HOME or ANDROID_SDK_ROOT in env, or sdk.dir in the root's
// local.properties.
func machineSDK(root string, env []string) bool {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if (name == envAndroidHome || name == envAndroidSDKRoot) && value != "" {
			return true
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "local.properties")) //nolint:gosec // the build's own local properties
	return err == nil && readProperties(data)["sdk.dir"] != ""
}

// androidSDK answers the SDK the extension installs under the Putnami home,
// toolchains/android/sdk, and whether its licenses are accepted. Unless the
// run is offline, it installs the pinned command-line tools, checked by
// SHA-256, and accepts the licenses with sdkmanager, once per machine.
func androidSDK(ctx context.Context, offline bool, info func(string)) (string, bool, error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", false, fmt.Errorf("no Putnami home to install the Android SDK in: set %s", putnamihome.Env)
	}
	base := filepath.Join(home, "toolchains", "android")
	sdk := filepath.Join(base, "sdk")
	if licensesAccepted(sdk) {
		return sdk, true, nil
	}
	if offline {
		return sdk, false, nil
	}
	a, ok := cmdlineTools[hostPlatform]
	if !ok {
		return "", false, fmt.Errorf("the extension pins no Android SDK command-line tools for %s", hostPlatform)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", false, err
	}
	lock, err := filelock.Acquire(filepath.Join(base, ".lock"), true, false)
	if err != nil {
		return "", false, fmt.Errorf("lock %s: %w", base, err)
	}
	defer func() { _ = lock.Release() }()
	if licensesAccepted(sdk) {
		return sdk, true, nil
	}
	tools := filepath.Join(base, "cmdline-tools-"+cmdlineToolsVersion)
	if _, err := os.Stat(sdkmanager(tools)); err != nil {
		info("installing the Android SDK command-line tools " + cmdlineToolsVersion)
	}
	pin := pinnedarchive.Pin{URL: downloadURL(androidRepository + a.name), SHA256: a.sha256, Format: pinnedarchive.Zip}
	if _, err := pinnedarchive.Install(ctx, pin, tools, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
		_, err := os.Stat(sdkmanager(candidate))
		return err == nil
	}}); err != nil {
		return "", false, fmt.Errorf("install the Android SDK command-line tools %s: %w", cmdlineToolsVersion, err)
	}
	java, err := licenseJava(ctx, info)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		return "", false, err
	}
	info(fmt.Sprintf("accepting the Android SDK licenses in %s, as the workspace's options of %s state", sdk, ExtensionName))
	argv := []string{sdkmanager(tools), "--sdk_root=" + sdk, "--licenses"}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the pinned sdkmanager
	cmd.Env = append(os.Environ(), "JAVA_HOME="+java, "PATH="+filepath.Join(java, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(strings.Repeat("y\n", 64))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if !licensesAccepted(sdk) {
		return "", false, fmt.Errorf("%s accepted no Android SDK license in %s", strings.Join(argv, " "), sdk)
	}
	return sdk, true, nil
}

// sdkmanager is the sdkmanager of the command-line tools installed in dir.
func sdkmanager(dir string) string {
	name := "sdkmanager"
	if runtime.GOOS == "windows" {
		name = "sdkmanager.bat"
	}
	return filepath.Join(dir, "cmdline-tools", "bin", name)
}

// licensesAccepted reports whether the SDK holds the accepted Android SDK
// license, the one its platforms and build tools need.
func licensesAccepted(sdk string) bool {
	info, err := os.Stat(filepath.Join(sdk, "licenses", "android-sdk-license"))
	return err == nil && info.Mode().IsRegular()
}
