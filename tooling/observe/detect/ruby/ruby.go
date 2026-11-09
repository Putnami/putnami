// Package ruby is the Ruby detector of @putnami/observe: it finds the
// components of a Ruby or Rails repository, such as a Rails modular monolith
// with packwerk packs and local engine gems, without running Ruby, Bundler or
// packwerk.
//
// A directory with a package.yml, a packwerk pack, is a workspace component;
// any other directory with a *.gemspec or a Gemfile is a manifest one, so the
// root of a Rails application is the component ".". The edges are the
// dependencies a pack's package.yml lists, the gems a Gemfile names by a
// local path, and the local gems a gemspec depends on. Gemfiles and gemspecs
// are Ruby: the detector reads them as text and never evaluates them.
package ruby

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Ecosystem is the evidence ecosystem slug of a Ruby component.
const Ecosystem = "ruby"

// Files the detector reads.
const (
	PackageFile   = "package.yml"
	Gemfile       = "Gemfile"
	GemspecSuffix = ".gemspec"
)

// Diagnostic codes.
const (
	CodePackageInvalid = "observe.ruby_package_invalid"
	CodeWalkBounded    = "observe.ruby_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the walk visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked: installed gems and packages, and the
// temporary files, logs and coverage of a Rails application.
var skippedDirs = map[string]bool{"node_modules": true, "vendor": true, "tmp": true, "log": true, "coverage": true}

// Component is one component of a Ruby repository.
type Component struct {
	// ID is the directory relative to the repository root, or ".".
	ID string
	// Name is the gem name its gemspec gives, else a pack's directory.
	Name string
	// Source is workspace for a pack, manifest for a gem or a Gemfile.
	Source string
	// DependsOn lists the ids of the components it depends on, sorted.
	DependsOn []string
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty outside a Ruby repository.
	Components  []Component
	Diagnostics []diag.Diagnostic
}

// dir is what one directory holds.
type dir struct {
	pack     bool
	gemspecs []string
	gemfile  bool
	gemNames []string
}

// Detect reads the Ruby repository at root.
func Detect(root string) Detection {
	dirs, diags := walk(root)
	out := Detection{Diagnostics: diags}
	if len(dirs) == 0 {
		return out
	}
	ids := make([]string, 0, len(dirs))
	for id := range dirs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// The gem names come first: an edge may name any gem of the repository.
	gems := map[string][]string{}
	for _, id := range ids {
		d := dirs[id]
		for _, file := range d.gemspecs {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(join(id, file))))
			if err != nil {
				continue
			}
			if name := gemspecName(data); name != "" {
				d.gemNames = append(d.gemNames, name)
				if !slices.Contains(gems[name], id) {
					gems[name] = append(gems[name], id)
				}
			}
		}
	}
	for _, id := range ids {
		d := dirs[id]
		c := Component{ID: id, Source: evidence.SourceManifest}
		if d.pack {
			c.Source = evidence.SourceWorkspace
			c.Name = id
		}
		if len(d.gemNames) > 0 {
			c.Name = d.gemNames[0]
		}
		edges := map[string]bool{}
		if d.pack {
			deps, err := packageDependencies(read(root, join(id, PackageFile)))
			if err != nil {
				file := join(id, PackageFile)
				out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodePackageInvalid, file, "the dependencies of %s give no edge: %v", file, err))
			}
			for _, dep := range deps {
				if target, ok := normalize(dep); ok {
					edges[target] = true
				}
			}
		}
		if d.gemfile {
			for _, ref := range gemfileReferences(read(root, join(id, Gemfile))) {
				if ref.path != "" {
					if target, ok := relative(id, ref.path); ok {
						edges[target] = true
					}
					continue
				}
				// A gem of a path block lies at the block's path or below it.
				block, ok := relative(id, ref.block)
				for _, target := range gems[ref.name] {
					if ok && contains(block, target) {
						edges[target] = true
						break
					}
				}
			}
		}
		for _, file := range d.gemspecs {
			for _, name := range gemspecDependencies(read(root, join(id, file))) {
				// A gem name two directories give is the first one's, in
				// path order.
				if targets := gems[name]; len(targets) > 0 {
					edges[targets[0]] = true
				}
			}
		}
		for target := range edges {
			if _, component := dirs[target]; component && target != id {
				c.DependsOn = append(c.DependsOn, target)
			}
		}
		sort.Strings(c.DependsOn)
		out.Components = append(out.Components, c)
	}
	return out
}

// SkipsDir reports whether the walk skips a directory of that name: a hidden
// one, or one of node_modules, vendor, tmp, log and coverage.
func SkipsDir(name string) bool {
	return strings.HasPrefix(name, ".") || skippedDirs[name]
}

// walk lists the directories that hold a package.yml, a gemspec or a
// Gemfile. It skips node_modules, vendor, tmp, log, coverage and hidden
// directories, such as .git and .bundle.
func walk(root string) (map[string]*dir, []diag.Diagnostic) {
	dirs := map[string]*dir{}
	var diags []diag.Diagnostic
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Ruby detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if p != root && SkipsDir(name) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || (name != PackageFile && name != Gemfile && !strings.HasSuffix(name, GemspecSuffix)) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		id, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel))
		if !ok {
			return nil
		}
		d := dirs[id]
		if d == nil {
			d = &dir{}
			dirs[id] = d
		}
		switch {
		case name == PackageFile:
			d.pack = true
		case name == Gemfile:
			d.gemfile = true
		default:
			d.gemspecs = append(d.gemspecs, name)
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Ruby detector cannot walk the repository: %v", err))
	}
	for _, d := range dirs {
		sort.Strings(d.gemspecs)
	}
	return dirs, diags
}

func read(root, file string) []byte {
	data, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	return data
}

// join joins a repository directory and a path relative to it.
func join(dir, rel string) string {
	if dir == wsproto.ProbeRootPath {
		return rel
	}
	return dir + "/" + rel
}

// normalize cleans a repository path; a path outside the repository or
// absolute is none.
func normalize(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") {
		return "", false
	}
	return wsproto.NormalizeProbePath(path.Clean(p))
}

// relative resolves a path a Gemfile in dir gives. An absolute path, which
// joined would read as a repository one, is none.
func relative(dir, p string) (string, bool) {
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	return normalize(join(dir, p))
}

// contains reports whether the repository directory dir holds target.
func contains(dir, target string) bool {
	return dir == wsproto.ProbeRootPath || target == dir || strings.HasPrefix(target, dir+"/")
}

// --- package.yml --------------------------------------------------------------

// packageDependencies reads the top-level dependencies list of a packwerk
// package.yml: a block sequence below the key, or a flow sequence on its
// line. No dependencies key is no dependency.
func packageDependencies(data []byte) ([]string, error) {
	var out []string
	in := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		if in {
			if item, ok := strings.CutPrefix(trimmed, "-"); ok && (item == "" || item[0] == ' ' || item[0] == '\t') {
				value, err := scalar(item)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
				continue
			}
			if line[0] == ' ' || line[0] == '\t' {
				return nil, fmt.Errorf("dependencies holds %q, not a list item", trimmed)
			}
			in = false
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		value, ok := strings.CutPrefix(line, "dependencies:")
		if !ok {
			continue
		}
		value = strings.TrimSpace(stripComment(value))
		switch {
		case value == "" || value == "~" || value == "null":
			in = true
		case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
			for _, item := range strings.Split(value[1:len(value)-1], ",") {
				if strings.TrimSpace(item) == "" {
					continue
				}
				dep, err := scalar(item)
				if err != nil {
					return nil, err
				}
				out = append(out, dep)
			}
		default:
			return nil, fmt.Errorf("dependencies is %q, not a list", value)
		}
	}
	return out, nil
}

// scalar reads one plain or quoted YAML scalar of a list.
func scalar(item string) (string, error) {
	value := strings.TrimSpace(stripComment(item))
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	} else if value == "" || strings.ContainsAny(value[:1], `"'[{&*!|>%@`+"`") {
		return "", fmt.Errorf("the dependency %q is not a pack name", strings.TrimSpace(item))
	}
	return value, nil
}

// stripComment cuts a YAML or Ruby comment: a # at the start or after a
// space, outside quotes.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// --- Gemfile and gemspec -------------------------------------------------------

// reference is one gem entry of a Gemfile that may name a local gem.
type reference struct {
	name string
	// path is its path option, relative to the Gemfile's directory.
	path string
	// block is the path of the path block it sits in, when it has no path
	// option.
	block string
}

var (
	gemEntry   = regexp.MustCompile(`^gem\s*\(?\s*["']([^"']+)["']`)
	pathOption = regexp.MustCompile(`(?:\bpath:|:path\s*=>)\s*["']([^"']+)["']`)
	pathBlock  = regexp.MustCompile(`^path\s*\(?\s*["']([^"']+)["']`)
	blockOpen  = regexp.MustCompile(`\bdo(\s*\|[^|]*\|)?$`)
	// keyword opens a block an end closes, such as an if around gem entries.
	keyword  = regexp.MustCompile(`^(if|unless|case|begin|while|until|def|class|module)\b`)
	blockEnd = regexp.MustCompile(`^end\b`)
)

// gemfileReferences returns the gem entries of a Gemfile with a literal path
// option, or within a path block. Blocks are followed by their do and end
// lines.
func gemfileReferences(data []byte) []reference {
	var out []reference
	var blocks []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	pending := ""
	for scanner.Scan() {
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		// An entry whose arguments go on after a trailing comma is one line.
		if pending != "" {
			line = pending + " " + line
			pending = ""
		}
		if strings.HasSuffix(line, ",") {
			pending = line
			continue
		}
		if blockEnd.MatchString(line) {
			if len(blocks) > 0 {
				blocks = blocks[:len(blocks)-1]
			}
			// end.each do |x| closes a block and opens another.
			if !blockOpen.MatchString(line) {
				continue
			}
		}
		if m := gemEntry.FindStringSubmatch(line); m != nil {
			ref := reference{name: m[1]}
			if p := pathOption.FindStringSubmatch(line); p != nil {
				ref.path = p[1]
			} else {
				for i := len(blocks) - 1; i >= 0; i-- {
					if blocks[i] != "" {
						ref.block = blocks[i]
						break
					}
				}
			}
			if ref.path != "" || ref.block != "" {
				out = append(out, ref)
			}
		}
		if keyword.MatchString(line) && !blockOpen.MatchString(line) {
			blocks = append(blocks, "")
		}
		if blockOpen.MatchString(line) {
			block := ""
			if m := pathBlock.FindStringSubmatch(line); m != nil {
				block = m[1]
			}
			blocks = append(blocks, block)
		}
	}
	return out
}

var (
	specName       = regexp.MustCompile(`\.name\s*=\s*["']([^"']+)["']`)
	specDependency = regexp.MustCompile(`\.add_(?:runtime_|development_)?dependency\s*\(?\s*["']([^"']+)["']`)
)

// gemspecName returns the literal name a gemspec gives, "" when none.
func gemspecName(data []byte) string {
	for line := range strings.Lines(string(data)) {
		if m := specName.FindStringSubmatch(stripComment(line)); m != nil {
			return m[1]
		}
	}
	return ""
}

// gemspecDependencies returns the gem names a gemspec depends on, runtime and
// development.
func gemspecDependencies(data []byte) []string {
	var out []string
	for line := range strings.Lines(string(data)) {
		if m := specDependency.FindStringSubmatch(stripComment(line)); m != nil && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
