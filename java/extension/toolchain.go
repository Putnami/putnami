package java

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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

// LockIdentity is the key of the JDK pin in the workspace lock.
const LockIdentity = "java"

// defaultVersion is the JDK a workspace gets when no .java-version narrows it.
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
// Adoptium API publishes for each archive. The executable is javac: its
// -version prints the bare version the lock's probe compares.
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

// HandleToolchainResolve serves the toolchain-resolve control call. The bool
// reports whether args name that call.
func HandleToolchainResolve(args []string, stdin io.Reader, stdout io.Writer, profile stacks.Profile) (bool, error) {
	return wsproto.ServeToolchainResolve(args, stdin, stdout, func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ToolchainResolveResult{}, fmt.Errorf("resolve the workspace root: %w", err)
		}
		return answerToolchainResolve(root, request, profile), nil
	})
}

// answerToolchainResolve answers one request against the workspace at root. An
// identity it pins nothing for is omitted, which leaves the lock without it.
func answerToolchainResolve(root string, request wsproto.ToolchainResolveRequest, profile stacks.Profile) wsproto.ToolchainResolveResult {
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  ExtensionName,
		Toolchains: map[string]wsproto.ToolchainResolution{},
	}
	for _, identity := range request.Identities {
		if identity != LockIdentity {
			result.Toolchains[identity] = wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
				Message: fmt.Sprintf("%s pins the %s toolchain only, not %q", ExtensionName, LockIdentity, identity),
			}}
			continue
		}
		if pin, ok := resolvePin(root, request, profile); ok {
			result.Toolchains[identity] = wsproto.ToolchainResolution{Pin: &pin}
		}
	}
	return result
}

// resolvePin returns the JDK the workspace at root pins: among the shipped
// releases every .java-version of its builds accepts, or the root's accepts
// when none suits them all, the committed pin when it is one, else
// defaultVersion when accepted, else the newest accepted. A workspace with no
// Gradle or Maven build pins nothing.
func resolvePin(root string, request wsproto.ToolchainResolveRequest, profile stacks.Profile) (wsproto.LockToolchainEntry, bool) {
	builds := buildDirs(root)
	if len(builds) == 0 {
		return wsproto.LockToolchainEntry{}, false
	}
	accepted := slices.Clone(jdks)
	if expected, err := profile.Toolchain.ExpectedVersion(root, wsproto.ProbeRootPath, wsproto.ProbeRootPath); err == nil {
		accepted = acceptedBy(profile.Toolchain, accepted, expected)
	}
	every := accepted
	for _, dir := range builds {
		if expected, err := profile.Toolchain.ExpectedVersion(root, dir, wsproto.ProbeRootPath); err == nil {
			every = acceptedBy(profile.Toolchain, every, expected)
		}
	}
	if len(every) > 0 {
		accepted = every
	}
	chosen, ok := choose(accepted, request.Locked[LockIdentity].Version)
	if !ok {
		return wsproto.LockToolchainEntry{}, false
	}
	if _, served := chosen.archives[request.Platform]; !served {
		return wsproto.LockToolchainEntry{}, false
	}
	return chosen.lockEntry(), true
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

// lockEntry returns the release as a lock version 5 pin.
func (r jdk) lockEntry() wsproto.LockToolchainEntry {
	entry := wsproto.LockToolchainEntry{
		Version:     r.version,
		Archives:    make(map[string]wsproto.LockToolchainArchive, len(r.archives)),
		Integrities: make(map[string]string, len(r.archives)),
	}
	for platform, a := range r.archives {
		entry.Archives[platform] = wsproto.LockToolchainArchive{URL: a.url, Format: a.format, Executable: a.executable}
		entry.Integrities[platform] = a.sha256
	}
	return entry
}

// skippedDirs are the directories the build search does not enter: VCS data,
// build outputs and installed dependencies.
var skippedDirs = []string{".git", ".gradle", ".putnami", "build", "node_modules", "target", "vendor"}

// buildDirs answers the workspace-relative directories that hold a Gradle or
// Maven build file.
func buildDirs(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root && slices.Contains(skippedDirs, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		switch entry.Name() {
		case "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts":
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err == nil {
				if dir, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel)); ok && !slices.Contains(out, dir) {
					out = append(out, dir)
				}
			}
		}
		return nil
	})
	return out
}

// Maven is installed by the extension when the machine has none: its version
// command prints several lines, which no lock probe compares.
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
