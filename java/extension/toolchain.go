package java

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
)

// defaultVersion is the JDK a task installs when .java-version accepts it.
const defaultVersion = "21.0.13"

// archive is one platform's archive of a JDK release.
type archive struct {
	url        string
	format     string
	executable string
	sha256     string
}

// jdk is an Eclipse Temurin release the extension can pin.
type jdk struct {
	version  string
	archives map[string]archive
}

// jdks lists the shipped Temurin releases, oldest first, with the SHA-256 the
// Adoptium API publishes for each archive and the path of its javac.
var jdks = []jdk{
	{"17.0.21", map[string]archive{
		"darwin/amd64":  {"https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_x64_mac_hotspot_17.0.21_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-17.0.21+9/Contents/Home/bin/javac", "868f5c672b3389fdbe48e44f94ae170f01668b1401360944fc7159d7cc9772cc"},
		"darwin/arm64":  {"https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_aarch64_mac_hotspot_17.0.21_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-17.0.21+9/Contents/Home/bin/javac", "78b08c7f5a0598242d5719c022e19479cb4a3aa45c37c19726a757a469ed1c15"},
		"linux/amd64":   {"https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_x64_linux_hotspot_17.0.21_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-17.0.21+9/bin/javac", "8b9b611d917e3dfd8419d3566b5949226f69b052fed7e109c1368add1dfe8711"},
		"linux/arm64":   {"https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_aarch64_linux_hotspot_17.0.21_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-17.0.21+9/bin/javac", "3e4eca7ead088f080af1476007decf010a240a1eb4e1e78c54fa32fbcc347084"},
		"windows/amd64": {"https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_x64_windows_hotspot_17.0.21_9.zip", wsproto.LockArchiveFormatZip, "jdk-17.0.21+9/bin/javac.exe", "98518ecf554465057ee5509462631deeee374bc368c12a411d225cd29381f84b"},
	}},
	{"21.0.13", map[string]archive{
		"darwin/amd64":  {"https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_x64_mac_hotspot_21.0.13_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-21.0.13+9/Contents/Home/bin/javac", "9aa1d1c17ae5467a022679e7e0af5c00797d94ac462ed1a46f4db07a8a34a6dd"},
		"darwin/arm64":  {"https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_aarch64_mac_hotspot_21.0.13_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-21.0.13+9/Contents/Home/bin/javac", "bf036b35b071bfb2f54f2c0af8ee8d88afa95ee6eb3dce02c0f94cfc2524d04a"},
		"linux/amd64":   {"https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_x64_linux_hotspot_21.0.13_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-21.0.13+9/bin/javac", "d7f5d5cf865a2902b70ff549b97586a7d3f234e016f041ab447ea740c7d66d1c"},
		"linux/arm64":   {"https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_aarch64_linux_hotspot_21.0.13_9.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-21.0.13+9/bin/javac", "e3522e707fc7dbe2a72c30746b1e7068301a0213ecd213d4491f35a14eb55578"},
		"windows/amd64": {"https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13%2B9/OpenJDK21U-jdk_x64_windows_hotspot_21.0.13_9.zip", wsproto.LockArchiveFormatZip, "jdk-21.0.13+9/bin/javac.exe", "a4dbaa05fca4bd89d84584e58c459bd732efebb1722af80ab13c6516832004f9"},
	}},
	{"25.0.5", map[string]archive{
		"darwin/amd64":  {"https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5%2B8/OpenJDK25U-jdk_x64_mac_hotspot_25.0.5_8.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-25.0.5+8/Contents/Home/bin/javac", "32cd4d2151c91aa00d0fc911edfd05245ed40fa6b1aa894d5f35c928cd67a59d"},
		"darwin/arm64":  {"https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5%2B8/OpenJDK25U-jdk_aarch64_mac_hotspot_25.0.5_8.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-25.0.5+8/Contents/Home/bin/javac", "36dc14d797aaff6d89aab302cd053fdba6207dfdcf772052fa7b023a94b0e857"},
		"linux/amd64":   {"https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5%2B8/OpenJDK25U-jdk_x64_linux_hotspot_25.0.5_8.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-25.0.5+8/bin/javac", "68982529869257d0ee6b824ec5aa270ff8b118bb438670da0b97ebdab178d7fb"},
		"linux/arm64":   {"https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5%2B8/OpenJDK25U-jdk_aarch64_linux_hotspot_25.0.5_8.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-25.0.5+8/bin/javac", "bb5b2ded252f21c6090cf780b0a408ad0d83bbb557e702955643557f13231880"},
		"windows/amd64": {"https://github.com/adoptium/temurin25-binaries/releases/download/jdk-25.0.5%2B8/OpenJDK25U-jdk_x64_windows_hotspot_25.0.5_8.zip", wsproto.LockArchiveFormatZip, "jdk-25.0.5+8/bin/javac.exe", "636bdc13dc649ddc7d5921c5c7d23cca6e44fd19d662b45cc9c02135a6c96c42"},
	}},
}

// hostPlatform is the platform the extension runs on. Tests name another.
var hostPlatform = runtime.GOOS + "/" + runtime.GOARCH

// downloadURL rewrites an archive URL. Tests point it at a local server.
var downloadURL = func(url string) string { return url }

// Provide installs, under the Putnami home, the Temurin JDK the expected
// version accepts, defaultVersion first, else the newest, when the javac on
// PATH is missing or outside it, and answers its javac. A release installed by
// an earlier task is reused with no network access.
func Provide(ctx context.Context, toolchain stacks.Toolchain, expectedAll []stacks.Expected, info func(string)) (stacks.Provided, error) {
	expected := expectedAll[0]
	chosen, ok := choose(acceptedBy(toolchain, jdks, expected), "")
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no Temurin JDK the extension pins (%s) satisfies %s %s", pinnedVersions(), expected.Source, expected.Version)
	}
	a, ok := chosen.archives[hostPlatform]
	if !ok {
		return stacks.Provided{}, fmt.Errorf("the extension pins no Temurin JDK %s for %s", chosen.version, hostPlatform)
	}
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return stacks.Provided{}, fmt.Errorf("no Putnami home to install the JDK in: set %s", putnamihome.Env)
	}
	dir := filepath.Join(home, "toolchains", "java", "java-"+chosen.version)
	executable := filepath.Join(dir, filepath.FromSlash(a.executable))
	if _, err := os.Stat(executable); err != nil {
		info("installing the Eclipse Temurin JDK " + chosen.version)
	}
	format := pinnedarchive.TarGz
	if a.format == wsproto.LockArchiveFormatZip {
		format = pinnedarchive.Zip
	}
	pin := pinnedarchive.Pin{URL: downloadURL(a.url), SHA256: a.sha256, Format: format}
	if _, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), SkipLinks: true, Complete: func(candidate string) bool {
		_, err := os.Stat(filepath.Join(candidate, filepath.FromSlash(a.executable)))
		return err == nil
	}}); err != nil {
		return stacks.Provided{}, fmt.Errorf("install the Temurin JDK %s: %w", chosen.version, err)
	}
	return stacks.Provided{Executable: executable}, nil
}

func pinnedVersions() string {
	var out []string
	for _, r := range jdks {
		out = append(out, r.version)
	}
	return strings.Join(out, ", ")
}

// acceptedBy returns the releases expected accepts, oldest first. A version no
// file states accepts every release.
func acceptedBy(toolchain stacks.Toolchain, candidates []jdk, expected stacks.Expected) []jdk {
	return slices.DeleteFunc(slices.Clone(candidates), func(r jdk) bool {
		return toolchain.Compare(expected, r.version).Fail
	})
}

func choose(accepted []jdk, committed string) (jdk, bool) {
	if len(accepted) == 0 {
		return jdk{}, false
	}
	for _, version := range []string{committed, defaultVersion} {
		if i := slices.IndexFunc(accepted, func(r jdk) bool { return r.version == version }); i >= 0 {
			return accepted[i], true
		}
	}
	return accepted[len(accepted)-1], true
}

// Maven is installed by the extension when the machine has none.
const mavenVersion = "3.9.11"

// mavenURL is where the Maven archive is published. Tests point it at a local
// server.
var mavenURL = "https://archive.apache.org/dist/maven/maven-3/" + mavenVersion + "/binaries/apache-maven-" + mavenVersion + "-bin.tar.gz"

// mavenSHA256 is the SHA-256 of the archive, whose SHA-512 Apache publishes.
var mavenSHA256 = "4b7195b6a4f5c81af4c0212677a32ee8143643401bc6e1e8412e6b06ea82beac"

// Tools installs Maven for the tasks of a Maven component when mvn is not on
// the task's PATH, and answers its bin directory. A Gradle build runs its
// wrapper and needs no tool.
func Tools(ctx context.Context, _ string, meta stacks.Metadata, env []string, info func(string)) ([]string, error) {
	if meta.Variant != VariantMaven || onPath(env, "mvn") {
		return nil, nil
	}
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return nil, fmt.Errorf("no Putnami home to install Apache Maven in: set %s", putnamihome.Env)
	}
	dir := filepath.Join(home, "toolchains", "maven", "maven-"+mavenVersion)
	bin := filepath.Join(dir, "apache-maven-"+mavenVersion, "bin")
	if _, err := os.Stat(filepath.Join(bin, "mvn")); err == nil {
		return []string{bin}, nil
	}
	info("installing Apache Maven " + mavenVersion)
	pin := pinnedarchive.Pin{URL: mavenURL, SHA256: mavenSHA256, Format: pinnedarchive.TarGz}
	if _, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
		_, err := os.Stat(filepath.Join(candidate, "apache-maven-"+mavenVersion, "bin", "mvn"))
		return err == nil
	}}); err != nil {
		return nil, fmt.Errorf("install Apache Maven %s: %w", mavenVersion, err)
	}
	return []string{bin}, nil
}

// onPath reports whether name resolves on the PATH of env.
func onPath(env []string, name string) bool {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.EqualFold(key, "PATH") {
			continue
		}
		for _, dir := range filepath.SplitList(value) {
			for _, candidate := range executableNames(name) {
				if info, err := os.Stat(filepath.Join(dir, candidate)); err == nil && !info.IsDir() {
					return true
				}
			}
		}
	}
	return false
}

func executableNames(name string) []string {
	if runtime.GOOS == "windows" {
		return []string{name + ".cmd", name + ".exe", name + ".bat"}
	}
	return []string{name}
}
