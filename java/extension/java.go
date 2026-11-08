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
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect}, nil
}

// Detect names the Gradle projects and the Maven modules among the candidate
// paths. Both detectors read the build at the workspace root, which is every
// component's stack root. A directory both builds claim is the Gradle
// build's.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	asked := map[string]bool{}
	for _, p := range request.Paths {
		asked[p] = true
	}
	var out []stacks.Component
	g := gradle.Detect(root)
	m := maven.Detect(root)
	diags := append(slices.Clone(g.Diagnostics), m.Diagnostics...)
	claimed := map[string]bool{}
	for _, c := range g.Components {
		claimed[c.ID] = true
		if !asked[c.ID] {
			continue
		}
		file, text := buildFile(root, c.ID)
		out = append(out, stacks.Component{
			Path:         c.ID,
			Name:         gradleName(c.Name),
			Ref:          gradleRef(c.Name),
			Variant:      gradleVariant(text),
			SourceFile:   file,
			Root:         wsproto.ProbeRootPath,
			Dependencies: slices.Clone(c.DependsOn),
		})
	}
	for _, c := range m.Components {
		if claimed[c.ID] {
			if asked[c.ID] {
				diags = append(diags, diag.Warningf(CodeBuildConflict, c.ID, "%s is both a Gradle project and the Maven module %s; it runs as the Gradle project", c.ID, c.Name))
			}
			continue
		}
		if !asked[c.ID] {
			continue
		}
		_, artifact, _ := strings.Cut(c.Name, ":")
		out = append(out, stacks.Component{
			Path:         c.ID,
			Name:         artifact,
			Ref:          c.Name,
			Variant:      VariantMaven,
			SourceFile:   joinRel(c.ID, maven.POM),
			Root:         wsproto.ProbeRootPath,
			Dependencies: slices.Clone(c.DependsOn),
		})
	}
	return out, diags
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
	return dir + "/" + name
}
