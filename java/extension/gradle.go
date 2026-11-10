package java

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/observe/detect/gradle"
	"go.putnami.dev/tooling/stacks"
)

// gradleInstallationsProperty is the Gradle property that lists the JDK homes
// Gradle's toolchains may use, beside the ones it detects. Gradle reads it
// from 6.7 on.
const gradleInstallationsProperty = "org.gradle.java.installations.paths"

// gradleRelease is a Gradle distribution the extension installs for a build
// that has no wrapper.
type gradleRelease struct {
	version string
	// minJava and maxJava bound the JDK majors the release runs on.
	minJava, maxJava int
	// sha256 is the digest services.gradle.org publishes beside the archive.
	sha256 string
}

// gradleReleases are the pinned distributions, newest first: a build runs the
// newest one that runs on its JDK.
var gradleReleases = []gradleRelease{
	{"9.8.2", 17, 25, "21bd5da2de49b642904077f38c0efdc7778e92b3ed0889392dca61b97e389607"},
	{"8.14.6", 8, 24, "7988ed071b2a07900e2ec715fca15c6ab72bce6db433af301fa7fa7e9407bd24"},
}

// gradleURL is where a Gradle distribution is published. Tests point it at a
// local server.
var gradleURL = func(version string) string {
	return "https://services.gradle.org/distributions/gradle-" + version + "-bin.zip"
}

// gradleFor answers the pinned release that runs on a JDK major: the newest
// whose range holds it, else the newest.
func gradleFor(major int) gradleRelease {
	for _, r := range gradleReleases {
		if major >= r.minJava && major <= r.maxJava {
			return r
		}
	}
	return gradleReleases[0]
}

// gradleExecutable is the launcher of an installed distribution under dir.
func gradleExecutable(dir, version string) string {
	name := "gradle"
	if runtime.GOOS == "windows" {
		name = "gradle.bat"
	}
	return filepath.Join(dir, "gradle-"+version, "bin", name)
}

// installGradle installs the release under the Putnami home, unless an
// earlier call did, and answers its launcher. Offline, it answers the
// installed one or fails.
func installGradle(ctx context.Context, r gradleRelease, info func(string)) (string, error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", fmt.Errorf("no Putnami home to install Gradle in: set %s", putnamihome.Env)
	}
	dir := filepath.Join(home, "toolchains", "gradle", "gradle-"+r.version)
	executable := gradleExecutable(dir, r.version)
	if _, err := os.Stat(executable); err == nil {
		return executable, nil
	}
	if offline() {
		return "", fmt.Errorf("the build has no Gradle wrapper, and Gradle %s is not installed: an offline run installs nothing", r.version)
	}
	info("installing Gradle " + r.version)
	pin := pinnedarchive.Pin{URL: gradleURL(r.version), SHA256: r.sha256, Format: pinnedarchive.Zip}
	if _, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
		_, err := os.Stat(gradleExecutable(candidate, r.version))
		return err == nil
	}}); err != nil {
		return "", fmt.Errorf("install Gradle %s: %w", r.version, err)
	}
	return executable, nil
}

// hasWrapper reports whether the Gradle build at root holds its wrapper.
func hasWrapper(root string) bool {
	for _, name := range []string{"gradlew", "gradlew.bat"} {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// adaptGradle answers a Gradle step to run: a step that names the wrapper of
// a build that has none runs the pinned Gradle, so does a step that names
// gradle when none is on PATH, and every Gradle step gets the homes of the
// JDKs the build's toolchains ask for.
func adaptGradle(ctx context.Context, root stacks.RootStep, step []string, info func(string)) ([]string, error) {
	out := slices.Clone(step)
	switch name := programName(step[0]); {
	case name == "gradlew" && !hasWrapper(filepath.Dir(step[0])),
		name == "gradle" && !strings.ContainsAny(step[0], `/\`) && !onPath(root.Env, "gradle"):
		major, err := runningMajor(ctx, root)
		if err != nil {
			return nil, err
		}
		program, err := installGradle(ctx, gradleFor(major), info)
		if err != nil {
			return nil, err
		}
		out[0] = program
	}
	homes, err := toolchainHomes(ctx, root, info)
	if err != nil {
		return nil, err
	}
	if len(homes) > 0 {
		out = append(out, "-P"+gradleInstallationsProperty+"="+strings.Join(homes, ","))
	}
	return out, nil
}

// toolchainHomes answers the homes of the pinned JDKs the Gradle build at
// root.Dir asks for through its toolchains, other than the major of the JDK
// that runs it, which Gradle uses as it is. A JDK the extension has not
// installed yet is installed, unless the run is offline. A major the
// extension pins no JDK for is left to Gradle, which finds one on the machine
// or provisions one through a toolchain resolver the build applies.
func toolchainHomes(ctx context.Context, root stacks.RootStep, info func(string)) ([]string, error) {
	majors := requestedJavaMajors(root.Dir)
	if len(majors) == 0 {
		return nil, nil
	}
	running, err := runningMajor(ctx, root)
	if err != nil {
		return nil, err
	}
	var homes []string
	for _, major := range majors {
		if major == running {
			continue
		}
		r, ok := jdkOfMajor(major)
		if ok {
			_, ok = r.archives[hostPlatform]
		}
		if !ok {
			info(fmt.Sprintf("the build asks Gradle for a Java %d toolchain, and the extension pins no Temurin JDK %d for %s: Gradle looks for one itself", major, major, hostPlatform))
			continue
		}
		javac, installed := installedJDK(r)
		switch {
		case installed:
		case offline():
			info(fmt.Sprintf("the build asks Gradle for a Java %d toolchain, and the Temurin JDK %s is not installed: an offline run installs nothing", major, r.version))
			continue
		default:
			if javac, err = installJDK(ctx, r, info); err != nil {
				return nil, err
			}
		}
		homes = append(homes, javaHome(javac))
	}
	return homes, nil
}

// runningMajor answers the major of the JDK the step runs, from its javac.
func runningMajor(ctx context.Context, root stacks.RootStep) (int, error) {
	javac := "javac"
	if root.Program != nil {
		javac = root.Program("javac")
	}
	if !strings.ContainsAny(javac, `/\`) {
		if found, ok := stacks.LookPathIn(root.Env, javac); ok {
			javac = found
		}
	}
	cmd := exec.CommandContext(ctx, javac, "-version") //nolint:gosec // the selected JDK
	cmd.Dir = root.Dir
	cmd.Env = root.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("read the Java version with %s -version: %w", javac, err)
	}
	m := javacVersion.FindStringSubmatch(out.String())
	if m == nil {
		return 0, fmt.Errorf("javac -version printed no version: %q", strings.TrimSpace(out.String()))
	}
	return strconv.Atoi(m[1])
}

// programName is the name of a step's program, without its directory and
// its Windows extension.
func programName(program string) string {
	name := filepath.Base(program)
	for _, ext := range []string{".bat", ".cmd", ".exe"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

var (
	// toolchainCall finds a call that names a toolchain's Java version:
	// JavaLanguageVersion.of(17) and jvmToolchain(17), in the Kotlin and
	// Groovy DSLs and in a plugin's Kotlin or Java source.
	toolchainCall = regexp.MustCompile(`(?:\bJavaLanguageVersion\s*\.\s*of|\bjvmToolchain)\s*\(`)
	// literalMajor is a major written as a number or a string.
	literalMajor = regexp.MustCompile(`^["']?([0-9]+)["']?$`)
	// javaVersionConstant is JavaVersion.VERSION_17, or VERSION_1_8.
	javaVersionConstant = regexp.MustCompile(`^JavaVersion\s*\.\s*VERSION_(?:1_)?([0-9]+)\b`)
	// catalogVersion is a version of the libs catalog, read as a number.
	catalogVersion = regexp.MustCompile(`^libs\s*\.\s*versions\s*\.\s*([A-Za-z0-9_.]+?)\s*\.\s*get\s*\(\s*\)(?:\s*\.\s*(?:toInt|toInteger)\s*\(\s*\)|\s+as\s+int)?$`)
)

const (
	// maxToolchainSources bounds the files of buildSrc and build-logic the
	// toolchain read opens.
	maxToolchainSources = 4000
	// daemonJVMProperties holds the build's daemon JVM criteria.
	daemonJVMProperties = "gradle/gradle-daemon-jvm.properties"
)

// requestedJavaMajors answers the Java majors the Gradle build at root asks
// for: the toolchains of its build files, of its script plugins and of the
// convention plugins and plugin sources of buildSrc and build-logic, read as
// a literal, a JavaVersion constant or a version of the libs catalog, and the
// daemon JVM criteria of gradle/gradle-daemon-jvm.properties. A version the
// build computes another way is not read.
func requestedJavaMajors(root string) []int {
	catalog := catalogVersions(root)
	found := map[int]bool{}
	read := func(path string) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > gradle.MaxScriptBytes {
			return
		}
		data, err := os.ReadFile(path) //nolint:gosec // a source of the Gradle build
		if err != nil {
			return
		}
		for _, major := range toolchainMajors(stripComments(string(data)), catalog) {
			found[major] = true
		}
	}
	for _, name := range append(slices.Clone(gradle.BuildFiles), gradle.SettingsFiles...) {
		read(filepath.Join(root, name))
	}
	for _, c := range gradle.Detect(root).Components {
		for _, name := range gradle.BuildFiles {
			read(filepath.Join(root, filepath.FromSlash(c.ID), name))
		}
	}
	scripts, _ := filepath.Glob(filepath.Join(root, "gradle", "*.gradle*"))
	for _, script := range scripts {
		if strings.HasSuffix(script, ".gradle") || strings.HasSuffix(script, ".gradle.kts") {
			read(script)
		}
	}
	budget := maxToolchainSources
	for _, dir := range []string{"buildSrc", "build-logic"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || budget <= 0 {
				return fs.SkipDir
			}
			if d.IsDir() {
				if name := d.Name(); name == "build" || name == ".gradle" || name == ".kotlin" {
					return fs.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".gradle", ".kts", ".kt", ".groovy", ".java":
				budget--
				read(path)
			}
			return nil
		})
	}
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(daemonJVMProperties))); err == nil { //nolint:gosec // a file of the Gradle build
		if m := daemonToolchainVersion.FindSubmatch(data); m != nil {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				found[n] = true
			}
		}
	}
	var out []int
	for major := range found {
		out = append(out, major)
	}
	slices.Sort(out)
	return out
}

// daemonToolchainVersion reads the daemon JVM criteria's Java version.
var daemonToolchainVersion = regexp.MustCompile(`(?m)^\s*toolchainVersion\s*=\s*([0-9]+)\s*$`)

// toolchainMajors answers the majors the toolchain calls of a source name.
func toolchainMajors(source string, catalog map[string]string) []int {
	var out []int
	for _, loc := range toolchainCall.FindAllStringIndex(source, -1) {
		arg := strings.TrimSpace(callArgument(source, loc[1]-1))
		var text string
		if m := literalMajor.FindStringSubmatch(arg); m != nil {
			text = m[1]
		} else if m := javaVersionConstant.FindStringSubmatch(arg); m != nil {
			text = m[1]
		} else if m := catalogVersion.FindStringSubmatch(arg); m != nil {
			text = catalog[catalogKey(m[1])]
		}
		if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// callArgument answers the text between the parenthesis at open and its
// match, or "" when the call does not close.
func callArgument(text string, open int) string {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[open+1 : i]
			}
		}
	}
	return ""
}

// catalogVersions reads the [versions] table of gradle/libs.versions.toml,
// by the key its accessor names.
func catalogVersions(root string) map[string]string {
	data, err := os.ReadFile(filepath.Join(root, "gradle", "libs.versions.toml")) //nolint:gosec // the build's version catalog
	if err != nil {
		return nil
	}
	out := map[string]string{}
	table := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := catalogTable.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if table != "versions" {
			continue
		}
		if m := catalogEntry.FindStringSubmatch(line); m != nil {
			out[catalogKey(m[1])] = m[2] + m[3]
		}
	}
	return out
}

var (
	catalogTable = regexp.MustCompile(`^\[\s*([A-Za-z0-9_.\-]+)\s*\]`)
	catalogEntry = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// catalogKey is a catalog alias as its accessor names it: java-toolchain,
// java_toolchain and java.toolchain are libs.versions.java.toolchain.
func catalogKey(alias string) string {
	return strings.NewReplacer("-", ".", "_", ".").Replace(alias)
}
