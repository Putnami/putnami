package markers

import (
	"path"
	"regexp"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/internal/ci"
)

// javaWrappers are the files a Maven or Gradle wrapper keeps the build tool's
// version in, relative to the build's root directory.
var javaWrappers = []string{".mvn/wrapper/maven-wrapper.properties", "gradle/wrapper/gradle-wrapper.properties"}

// gradleBuildFiles are the Gradle scripts that may turn dependency locking on.
var gradleBuildFiles = map[string]bool{
	"build.gradle": true, "build.gradle.kts": true, "settings.gradle": true, "settings.gradle.kts": true,
}

// enforcerLocks are the Maven Enforcer rules that lock dependency versions:
// every transitive version resolves to the highest one asked, the tree
// converges on one version, and no version is a range or a snapshot.
var enforcerLocks = regexp.MustCompile(`requireUpperBoundDeps|dependencyConvergence|banDynamicVersions|requireReleaseDeps`)

// javaBuilds are the CI fragments that run a Maven or Gradle build.
var javaBuilds = []string{"mvn ", "mvnw", "gradle ", "gradlew"}

// javaWrapper reports whether a file pins a Maven or Gradle wrapper for a
// build at the root or in a top-level directory, as for every other pin.
func javaWrapper(file string) bool {
	for _, wrapper := range javaWrappers {
		if file == wrapper {
			return true
		}
		if dir, ok := strings.CutSuffix(file, "/"+wrapper); ok && !strings.Contains(dir, "/") {
			return true
		}
	}
	return false
}

// gradleBuildFile reports whether a file is a Gradle script at the root or
// one level down.
func gradleBuildFile(file string) bool {
	return gradleBuildFiles[path.Base(file)] && strings.Count(file, "/") <= maxPinDepth
}

// javaLocks returns the files that lock a Java build's dependencies: Gradle
// lockfiles, Gradle scripts that call dependencyLocking, and Maven projects
// whose Enforcer plugin applies a locking rule. Like every lockfile, they sit
// at the root or one level down.
func (c *computation) javaLocks() []string {
	var locks []string
	for _, file := range c.Files {
		if strings.Count(file.Path, "/") > maxPinDepth {
			continue
		}
		content := string(c.Contents[file.Path])
		switch {
		case path.Base(file.Path) == "gradle.lockfile":
			locks = append(locks, file.Path)
		case gradleBuildFile(file.Path) && strings.Contains(content, "dependencyLocking"):
			locks = append(locks, file.Path)
		case path.Base(file.Path) == "pom.xml" && strings.Contains(content, "maven-enforcer-plugin") && enforcerLocks.MatchString(content):
			locks = append(locks, file.Path)
		}
	}
	return locks
}

// javaWrappersPresent returns the Maven and Gradle wrappers at the root or
// one level down.
func (c *computation) javaWrappersPresent() []string {
	var found []string
	for _, file := range c.Files {
		if javaWrapper(file.Path) {
			found = append(found, file.Path)
		}
	}
	return found
}

// javaBuildJobs returns the CI configurations that run a Maven or Gradle
// build on pull requests.
func (c *computation) javaBuildJobs() []ci.Config {
	return ci.Runs(c.configs, javaBuilds...)
}
