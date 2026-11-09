// Package ruby is @putnami/ruby, the stack extension of Ruby and Rails: it
// runs the applications, packwerk packs and local gems of a Ruby repository
// as Putnami projects, with the stack core of go.putnami.dev/tooling/stacks
// and the Ruby detector of putnami observe.
package ruby

import (
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	rubydetect "go.putnami.dev/tooling/observe/detect/ruby"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/ruby"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Ruby stack: the embedded profile and the Ruby detector.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{Extension: ExtensionName, Profile: profile, Detect: Detect, IsComponent: IsComponent}, nil
}

// IsComponent reports whether a directory that holds a Gemfile, a
// package.yml or a gemspec is a component: for the Ruby detector, each such
// directory is, unless it lies under a directory its walk skips.
func IsComponent(_, dir string) bool {
	if dir == wsproto.ProbeRootPath {
		return true
	}
	for _, seg := range strings.Split(dir, "/") {
		if rubydetect.SkipsDir(seg) {
			return false
		}
	}
	return true
}

// Detect names the components among the candidate paths with the Ruby
// detector. A component's name is its gem's, else its pack's path, else, for
// the workspace root, the workspace directory's name. Its stack root is the
// nearest directory, from its own up to the workspace root, that holds a
// Gemfile: the bundle its commands run in. A component whose directory holds
// config.ru is an application, any other a library.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	detection := rubydetect.Detect(root)
	var out []stacks.Component
	for _, c := range detection.Components {
		if !slices.Contains(request.Paths, c.ID) {
			continue
		}
		name := c.Name
		if name == "" {
			name = c.ID
		}
		if name == wsproto.ProbeRootPath {
			name = filepath.Base(root)
		}
		stackRoot := bundleRoot(root, c.ID)
		kind := componentType(root, c.ID)
		out = append(out, stacks.Component{
			Path:           c.ID,
			Name:           name,
			Variant:        testVariant(root, c.ID),
			SourceFile:     sourceFile(root, c.ID),
			Root:           stackRoot,
			Dependencies:   slices.Clone(c.DependsOn),
			Type:           kind,
			ReleaseMembers: imageMember(root, c.ID, name, stackRoot, kind),
		})
	}
	return out, detection.Diagnostics
}

// Profile variants: the test runner of a component.
const (
	VariantRSpec    = "rspec"
	VariantMinitest = "minitest"
	VariantUntested = "untested"
)

// testVariant names the test runner of the component at dir: rspec when it
// holds a spec/**/*_spec.rb file, minitest when it holds a test/**/*_test.rb
// file and no spec file, untested when it holds no Ruby file under spec/ or
// test/, and rspec otherwise, which fails on test files it cannot run rather
// than pass over them. The workspace adapter hashes the Ruby files under
// spec/ and test/, so adding or removing one asks the probe again.
func testVariant(root, dir string) string {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	spec, test := filepath.Join(abs, "spec"), filepath.Join(abs, "test")
	switch {
	case holds(spec, "_spec.rb"):
		return VariantRSpec
	case holds(test, "_test.rb"):
		return VariantMinitest
	case !holds(spec, ".rb") && !holds(test, ".rb"):
		return VariantUntested
	}
	return VariantRSpec
}

// holds reports whether the tree under dir holds a regular file whose name
// ends with suffix.
func holds(dir, suffix string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || found {
			return fs.SkipAll
		}
		if !entry.IsDir() && entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), suffix) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// sourceFile is the file that names the component: its gemspec, its
// package.yml, else its Gemfile.
func sourceFile(root, dir string) string {
	gemspecs, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*"+rubydetect.GemspecSuffix))
	slices.Sort(gemspecs)
	candidates := []string{rubydetect.PackageFile, rubydetect.Gemfile}
	if len(gemspecs) > 0 {
		candidates = append([]string{filepath.Base(gemspecs[0])}, candidates...)
	}
	for _, name := range candidates {
		if isFile(filepath.Join(root, filepath.FromSlash(dir), name)) {
			return join(dir, name)
		}
	}
	return ""
}

// bundleRoot is the nearest directory from dir up to the workspace root
// that holds a Gemfile; dir itself when none does.
func bundleRoot(root, dir string) string {
	for d := dir; ; d = path.Dir(d) {
		if isFile(filepath.Join(root, filepath.FromSlash(d), rubydetect.Gemfile)) {
			return d
		}
		if d == wsproto.ProbeRootPath {
			return dir
		}
	}
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func join(dir, name string) string {
	if dir == wsproto.ProbeRootPath {
		return name
	}
	return dir + "/" + name
}
