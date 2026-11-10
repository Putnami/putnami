package java

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
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
// Adoptium API publishes for each archive and the path of its javac. Java 8
// keeps the version its javac prints; Adoptium publishes no current Java 8
// release for macOS.
var jdks = []jdk{
	{"1.8.0_512", map[string]archive{
		"linux/amd64":   {"https://github.com/adoptium/temurin8-binaries/releases/download/jdk8u512-b09/OpenJDK8U-jdk_x64_linux_hotspot_8u512b09.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk8u512-b09/bin/javac", "fb3bfa869012419b562bb4235d0d83ec4f8c1dda62ae432ba20c6c3a7e43f7e4"},
		"linux/arm64":   {"https://github.com/adoptium/temurin8-binaries/releases/download/jdk8u512-b09/OpenJDK8U-jdk_aarch64_linux_hotspot_8u512b09.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk8u512-b09/bin/javac", "01263df22c4553f63146bbae807e70cd4628dd0d9ec7582f01cb2453f7c69aaa"},
		"windows/amd64": {"https://github.com/adoptium/temurin8-binaries/releases/download/jdk8u512-b09/OpenJDK8U-jdk_x64_windows_hotspot_8u512b09.zip", wsproto.LockArchiveFormatZip, "jdk8u512-b09/bin/javac.exe", "20f248ddd600f900cd9b235d50d54cdb27e7b750f140d381176a7612c53663cd"},
	}},
	{"11.0.33", map[string]archive{
		"darwin/amd64":  {"https://github.com/adoptium/temurin11-binaries/releases/download/jdk-11.0.33%2B10/OpenJDK11U-jdk_x64_mac_hotspot_11.0.33_10.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-11.0.33+10/Contents/Home/bin/javac", "11e3e9de3ce65ff957aee9b4b76876553e9d328cab9aede47996463625c88887"},
		"darwin/arm64":  {"https://github.com/adoptium/temurin11-binaries/releases/download/jdk-11.0.33%2B10/OpenJDK11U-jdk_aarch64_mac_hotspot_11.0.33_10.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-11.0.33+10/Contents/Home/bin/javac", "6ef5b027206691439b2dcb2fc05adc13cc7f5b6e75a08fce8f75ee188adc7751"},
		"linux/amd64":   {"https://github.com/adoptium/temurin11-binaries/releases/download/jdk-11.0.33%2B10/OpenJDK11U-jdk_x64_linux_hotspot_11.0.33_10.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-11.0.33+10/bin/javac", "c4b8eaf56c93b95b689ffc08d155df1bcdbbffb666f8f71d21d97aedaea6f0ec"},
		"linux/arm64":   {"https://github.com/adoptium/temurin11-binaries/releases/download/jdk-11.0.33%2B10/OpenJDK11U-jdk_aarch64_linux_hotspot_11.0.33_10.tar.gz", wsproto.LockArchiveFormatTarGz, "jdk-11.0.33+10/bin/javac", "60b0fb79bb678b6fae3e499c7b0a353e68633ed0ab69d1a6bafe6092ed1e59e4"},
		"windows/amd64": {"https://github.com/adoptium/temurin11-binaries/releases/download/jdk-11.0.33%2B10/OpenJDK11U-jdk_x64_windows_hotspot_11.0.33_10.zip", wsproto.LockArchiveFormatZip, "jdk-11.0.33+10/bin/javac.exe", "cff2e75657c96a718974732b4a7b2c81a245a0b2ea44e6d8ea08cd0248d8fa9d"},
	}},
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
	javac, err := installJDK(ctx, chosen, info)
	if err != nil {
		return stacks.Provided{}, err
	}
	return stacks.Provided{Executable: javac}, nil
}

// installJDK installs the release under the Putnami home, unless an earlier
// call did, and answers its javac.
func installJDK(ctx context.Context, r jdk, info func(string)) (string, error) {
	dir, a, err := jdkDir(r)
	if err != nil {
		return "", err
	}
	executable := filepath.Join(dir, filepath.FromSlash(a.executable))
	if _, err := os.Stat(executable); err != nil {
		info("installing the Eclipse Temurin JDK " + r.version)
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
		return "", fmt.Errorf("install the Temurin JDK %s: %w", r.version, err)
	}
	return executable, nil
}

// installedJDK answers the javac of the release when an earlier call
// installed it, with no network access.
func installedJDK(r jdk) (string, bool) {
	dir, a, err := jdkDir(r)
	if err != nil {
		return "", false
	}
	executable := filepath.Join(dir, filepath.FromSlash(a.executable))
	_, err = os.Stat(executable)
	return executable, err == nil
}

// jdkDir answers the directory the release installs into and its archive for
// the host.
func jdkDir(r jdk) (string, archive, error) {
	a, ok := r.archives[hostPlatform]
	if !ok {
		return "", archive{}, fmt.Errorf("the extension pins no Temurin JDK %s for %s", r.version, hostPlatform)
	}
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", archive{}, fmt.Errorf("no Putnami home to install the JDK in: set %s", putnamihome.Env)
	}
	return filepath.Join(home, "toolchains", "java", "java-"+r.version), a, nil
}

// javaHome is the home of the JDK whose javac is executable.
func javaHome(javac string) string {
	return filepath.Dir(filepath.Dir(javac))
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
		return toolchain.CompareWith(expected, r.version, matchesJava).Fail
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

// jdkOfMajor answers the newest release the extension pins of a Java major.
func jdkOfMajor(major int) (jdk, bool) {
	for i := len(jdks) - 1; i >= 0; i-- {
		if javaMajor(jdks[i].version) == major {
			return jdks[i], true
		}
	}
	return jdk{}, false
}

// MatchVersion is the Java rule for an exact expected version: Java 8 is
// both 8 and 1.8, and its javac prints 1.8.0_512, so 8, 1.8, 1.8.0 and 8.0.512
// accept it. Any other version matches as the core's rule says: equal, or a
// release of a shorter version, such as 21.0.13 for 21.
func MatchVersion(_ string, expected stacks.Expected, machine string) bool {
	return matchesJava(expected.Version, machine)
}

func matchesJava(expected, machine string) bool {
	e, m := plainJavaVersion(expected), plainJavaVersion(machine)
	if e == m {
		return true
	}
	rest, ok := strings.CutPrefix(m, e+".")
	return ok && rest != "" && strings.Trim(rest, "0123456789.") == ""
}

// plainJavaVersion writes a Java version without the 1. of Java 8 and older
// and with its update after a dot: 1.8.0_512 is 8.0.512, 1.8 is 8.
func plainJavaVersion(version string) string {
	v := strings.TrimPrefix(version, "v")
	if rest, ok := strings.CutPrefix(v, "1."); ok && rest != "" && rest[0] >= '1' && rest[0] <= '9' {
		v = rest
	}
	return strings.ReplaceAll(v, "_", ".")
}

// javaMajor answers the major of a Java version, 8 for 1.8.0_512, or 0.
func javaMajor(version string) int {
	major, _, _ := strings.Cut(plainJavaVersion(version), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// onPath reports whether name resolves on the PATH of env.
func onPath(env []string, name string) bool {
	_, ok := stacks.LookPathIn(env, name)
	return ok
}

// offline reports a run with offline dependencies, which installs nothing.
func offline() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}
