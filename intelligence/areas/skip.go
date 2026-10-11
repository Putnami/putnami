package areas

import (
	"path"
	"strings"
)

// skippedDirs are directory names whose content no one writes by hand:
// installed or vendored dependencies, a package manager's own release, and
// build output.
var skippedDirs = map[string]bool{
	"node_modules":     true,
	"vendor":           true,
	"vendors":          true,
	"third_party":      true,
	"bower_components": true,
	".yarn":            true,
	".gen":             true,
	"dist":             true,
}

// lockfiles are the dependency lockfiles the detector recognizes. They are
// written by tools, so they never count as code.
var lockfiles = map[string]bool{
	"Cargo.lock": true, "Gemfile.lock": true, "Pipfile.lock": true, "bun.lock": true,
	"bun.lockb": true, "composer.lock": true, "go.sum": true, "gradle.lockfile": true,
	"package-lock.json": true, "pnpm-lock.yaml": true, "poetry.lock": true,
	"putnami.lock.json": true, "uv.lock": true, "yarn.lock": true,
}

// skipped reports whether a path is excluded by its name alone: a lockfile, a
// minified bundle, or a file under a vendored or build-output directory.
func skipped(file string) bool {
	base := path.Base(file)
	if lockfiles[base] || strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return true
	}
	for _, segment := range strings.Split(path.Dir(file), "/") {
		if skippedDirs[segment] {
			return true
		}
	}
	return false
}
