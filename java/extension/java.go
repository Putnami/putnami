// Package java is @putnami/java, the stack extension of Java and Kotlin: it
// runs the projects of a Gradle build, Android included, and the modules of a
// Maven build as Putnami projects, with the stack core of
// go.putnami.dev/tooling/stacks and the Gradle and Maven detectors of
// putnami observe.
package java

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/gradle"
	"go.putnami.dev/tooling/observe/detect/maven"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/java"

// The profile's variants, one per build.
const (
	VariantGradle          = "gradle"
	VariantGradleJaCoCo    = "gradle-jacoco"
	VariantGradlePlatform  = "gradle-platform"
	VariantGradleCatalog   = "gradle-catalog"
	VariantAndroid         = "android"
	VariantAndroidCoverage = "android-coverage"
	VariantMaven           = "maven"
)

// CodeBuildConflict is the diagnostic of a directory both builds claim.
const CodeBuildConflict = "java.build_conflict"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Java stack: the embedded profile and the Gradle and Maven
// detectors.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect, Provide: Provide(profile.Toolchain), Tools: Tools}, nil
}

// Detect names the Gradle projects and the Maven modules among the candidate
// paths. A Gradle build's root is the nearest directory that holds a settings
// file; a Maven build's root is the topmost directory of an unbroken chain of
// pom.xml files. Each build's root is the stack root of its components; a root
// that is no component is an input-only project they depend on. A Gradle
// build inside another one, such as buildSrc or an included build, is no build
// of its own. A directory both builds claim is the Gradle build's.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	asked := map[string]bool{}
	gradleRoots := map[string]bool{}
	mavenRoots := map[string]bool{}
	for _, p := range request.Paths {
		dir, ok := wsproto.NormalizeProbePath(p)
		if !ok {
			continue
		}
		asked[dir] = true
		if r, ok := gradleBuildRoot(root, dir); ok {
			gradleRoots[r] = true
		}
		if r, ok := mavenBuildRoot(root, dir); ok {
			mavenRoots[r] = true
		}
	}
	var out []stacks.Component
	var diags []diag.Diagnostic
	claimed := map[string]bool{}
	for _, buildRoot := range sortedKeys(gradleRoots) {
		g := gradle.Detect(filepath.Join(root, filepath.FromSlash(buildRoot)))
		diags = append(diags, rebaseDiagnostics(g.Diagnostics, buildRoot)...)
		hasRoot := false
		for _, c := range g.Components {
			path := joinRel(buildRoot, c.ID)
			claimed[path] = true
			hasRoot = hasRoot || path == buildRoot
			if !asked[path] {
				continue
			}
			file, text := buildFile(root, path)
			out = append(out, stacks.Component{
				Path:         path,
				Name:         gradleName(c.Name),
				Ref:          gradleRef(c.Name),
				Variant:      gradleVariant(text),
				SourceFile:   file,
				Root:         buildRoot,
				Dependencies: rebasePaths(c.DependsOn, buildRoot),
				Type:         gradleType(text),
			})
			out[len(out)-1].ReleaseMembers = imageMember(root, wsproto.ProbeProject{Path: path, SourceName: gradleName(c.Name), Type: gradleType(text)})
		}
		if !hasRoot && buildRoot != wsproto.ProbeRootPath && asked[buildRoot] {
			claimed[buildRoot] = true
			out = append(out, stacks.Component{Path: buildRoot, SourceFile: settingsFile(root, buildRoot), Root: buildRoot, Variant: VariantGradle, InputOnly: true})
		}
	}
	for _, buildRoot := range sortedKeys(mavenRoots) {
		m := maven.Detect(filepath.Join(root, filepath.FromSlash(buildRoot)))
		diags = append(diags, rebaseDiagnostics(m.Diagnostics, buildRoot)...)
		hasRoot := false
		for _, c := range m.Components {
			path := joinRel(buildRoot, c.ID)
			if claimed[path] {
				if asked[path] {
					diags = append(diags, diag.Warningf(CodeBuildConflict, path, "%s is both a Gradle project and the Maven module %s; it runs as the Gradle project", path, c.Name))
				}
				continue
			}
			hasRoot = hasRoot || path == buildRoot
			if !asked[path] {
				continue
			}
			_, artifact, _ := strings.Cut(c.Name, ":")
			out = append(out, stacks.Component{
				Path:         path,
				Name:         artifact,
				Ref:          c.Name,
				Variant:      VariantMaven,
				SourceFile:   joinRel(path, maven.POM),
				Root:         buildRoot,
				Dependencies: rebasePaths(c.DependsOn, buildRoot),
				Type:         mavenType(root, path),
			})
			out[len(out)-1].ReleaseMembers = imageMember(root, wsproto.ProbeProject{Path: path, SourceName: artifact, Type: mavenType(root, path)})
		}
		if !hasRoot && buildRoot != wsproto.ProbeRootPath && asked[buildRoot] && !claimed[buildRoot] {
			out = append(out, stacks.Component{Path: buildRoot, SourceFile: joinRel(buildRoot, maven.POM), Root: buildRoot, Variant: VariantMaven, InputOnly: true})
		}
	}
	return out, diags
}

// gradleBuildRoot answers the root of the Gradle build that holds dir: the
// nearest directory, dir included, that holds a settings file. A settings
// file inside another Gradle build names an included build, which belongs to
// that outer build: dir then has no build of its own.
func gradleBuildRoot(root, dir string) (string, bool) {
	found := ""
	for d := dir; ; d = parentDir(d) {
		if settingsFile(root, d) != "" {
			if found != "" {
				return "", false
			}
			found = d
		}
		if d == wsproto.ProbeRootPath {
			break
		}
	}
	return found, found != ""
}

// mavenBuildRoot answers the root of the Maven build that holds dir: the
// topmost directory of the unbroken chain of pom.xml files from dir up.
func mavenBuildRoot(root, dir string) (string, bool) {
	if !isFile(root, joinRel(dir, maven.POM)) {
		return "", false
	}
	found := dir
	for d := dir; d != wsproto.ProbeRootPath; {
		d = parentDir(d)
		if !isFile(root, joinRel(d, maven.POM)) {
			break
		}
		found = d
	}
	return found, true
}

// settingsFile answers the settings file of a Gradle build rooted at dir, or
// "" when dir holds none.
func settingsFile(root, dir string) string {
	for _, name := range gradle.SettingsFiles {
		if isFile(root, joinRel(dir, name)) {
			return joinRel(dir, name)
		}
	}
	return ""
}

func isFile(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular()
}

func parentDir(dir string) string {
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[:i]
	}
	return wsproto.ProbeRootPath
}

func rebasePaths(paths []string, buildRoot string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, joinRel(buildRoot, p))
	}
	return out
}

func rebaseDiagnostics(diags []diag.Diagnostic, buildRoot string) []diag.Diagnostic {
	out := slices.Clone(diags)
	for i := range out {
		if out[i].Field != "" {
			out[i].Field = joinRel(buildRoot, out[i].Field)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// gradleName is the project name of a Gradle path: :libs:core is libs/core.
// The root project has none, so it keeps its directory's name.
func gradleName(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, ":"), ":", "/")
}

// gradleRef is the task path prefix of a Gradle path, which a step joins to
// a task name: : for the root project, :api: for :api.
func gradleRef(path string) string {
	if path == gradle.RootPath {
		return gradle.RootPath
	}
	return path + ":"
}

var (
	// androidPlugin finds an Android Gradle plugin applied by id, by the
	// alias of a version catalog, or the android block it configures.
	androidPlugin = regexp.MustCompile(`com\.android\.(?:application|library|test|dynamic-feature)|plugins\.android[.\-_]?(?:application|library|test|dynamic)|(?m)^\s*android\s*\{`)
	// androidCoverage finds unit test coverage turned on in a build type.
	androidCoverage = regexp.MustCompile(`(?:[Ee]nableUnitTestCoverage|testCoverageEnabled)\s*=?\s*true`)
	// platformPlugin finds java-platform: a project of dependency
	// constraints, with check and assemble and no test task.
	platformPlugin = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_$./-])(?:org\.gradle\.)?java-platform(?:[^A-Za-z0-9_$/.-]|$)`)
	// catalogPlugin finds version-catalog: a project that publishes a
	// catalog, with neither check, assemble nor test.
	catalogPlugin = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_$./-])(?:org\.gradle\.)?version-catalog(?:[^A-Za-z0-9_$/.-]|$)`)
	// jacocoPlugin finds the jacoco plugin, applied or configured; a path
	// or a coordinate that holds the word, such as a URL, is no plugin.
	jacocoPlugin = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_$./-])(?:org\.gradle\.)?jacoco(?:[^A-Za-z0-9_$/.-]|$)`)
)

// Component types the planner reads: an application runs, a library does not.
const (
	TypeApplication = "application"
	TypeLibrary     = "library"
)

var (
	// gradleApplication finds the plugins that make a runnable project: the
	// application plugin, Spring Boot, and an Android application.
	gradleApplication = regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_$./-])(?:org\.gradle\.)?application(?:[^A-Za-z0-9_$/.-]|$)|org\.springframework\.boot|plugins\.spring[.\-_]?boot|com\.android\.application|plugins\.android[.\-_]?application`)
	// mavenApplication finds a module that builds a runnable jar: the
	// Spring Boot plugin.
	mavenApplication = regexp.MustCompile(`<artifactId>\s*spring-boot-maven-plugin\s*</artifactId>`)
)

// gradleType reads a Gradle project's type from its build file.
func gradleType(build string) string {
	if gradleApplication.MatchString(build) {
		return TypeApplication
	}
	return TypeLibrary
}

// mavenType reads a Maven module's type from its POM.
func mavenType(root, dir string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(joinRel(dir, maven.POM)))) //nolint:gosec // a POM under the workspace root
	if err == nil && mavenApplication.Match(data) {
		return TypeApplication
	}
	return TypeLibrary
}

// gradleVariant reads the variant of a Gradle project from its build file.
func gradleVariant(build string) string {
	switch {
	case androidPlugin.MatchString(build) && androidCoverage.MatchString(build):
		return VariantAndroidCoverage
	case androidPlugin.MatchString(build):
		return VariantAndroid
	case platformPlugin.MatchString(build):
		return VariantGradlePlatform
	case catalogPlugin.MatchString(build):
		return VariantGradleCatalog
	case jacocoPlugin.MatchString(build):
		return VariantGradleJaCoCo
	default:
		return VariantGradle
	}
}

// buildFile returns the workspace-relative build file of a Gradle project
// and its text with its comments removed; none when it has no build file.
func buildFile(root, dir string) (string, string) {
	for _, name := range gradle.BuildFiles {
		rel := joinRel(dir, name)
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // a build file under the workspace root
		if err == nil && len(data) <= gradle.MaxScriptBytes {
			return rel, stripComments(string(data))
		}
	}
	return "", ""
}

// stripComments removes the // and /* */ comments of a build script, so
// that a plugin that is commented out picks no variant. Strings, triple-quoted
// ones included, are kept whole: a "//" or "/*" inside one is no comment.
func stripComments(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"' || c == '\'':
			end := stringEnd(text, i)
			b.WriteString(text[i : end+1])
			i = end
		case strings.HasPrefix(text[i:], "//"):
			for i < len(text) && text[i] != '\n' {
				i++
			}
			if i < len(text) {
				b.WriteByte('\n')
			}
		case strings.HasPrefix(text[i:], "/*"):
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stringEnd returns the index of the last byte of the string that opens at
// i, a triple-quoted one included; the last index when it does not close.
func stringEnd(text string, i int) int {
	quote := text[i]
	if triple := strings.Repeat(string(quote), 3); strings.HasPrefix(text[i:], triple) {
		if end := strings.Index(text[i+3:], triple); end >= 0 {
			return i + 3 + end + 2
		}
		return len(text) - 1
	}
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case quote, '\n':
			return j
		}
	}
	return len(text) - 1
}

func joinRel(dir, name string) string {
	if dir == "" || dir == wsproto.ProbeRootPath {
		return name
	}
	if name == wsproto.ProbeRootPath {
		return dir
	}
	return dir + "/" + name
}
