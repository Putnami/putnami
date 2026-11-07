// Package specs finds the specs a repository keeps in the formats the
// features protocol reads (Spec Kit, OpenSpec, Kiro and Gherkin) and reads
// their requirements with that protocol's readers.
package specs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
)

// Spec is one spec of the repository.
type Spec struct {
	// Format is the features protocol's format name: speckit, openspec, kiro
	// or gherkin.
	Format string `json:"format"`
	// Path is the repository-relative directory of the spec, or the file of a
	// Gherkin feature.
	Path string `json:"path"`
	// File is the repository-relative file that holds the requirements.
	File string `json:"file"`
	// Component is the component that owns the spec, empty when none does.
	Component string `json:"component,omitempty"`
	// Changed says the change touches a file of the spec.
	Changed bool `json:"changed"`
	// Requirements are the spec's requirements, in file order.
	Requirements []Requirement `json:"requirements"`
}

// Requirement is one requirement of a spec.
type Requirement struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Line is the 1-based line of the requirement in Spec.File.
	Line int `json:"line,omitempty"`
}

// CodeUnreadable is the diagnostic of a spec observe found and cannot read.
const CodeUnreadable = "observe.spec_unreadable"

// maxFiles bounds the files the walk visits, as report discovery bounds it.
const maxFiles = 200000

// skippedDirs are never walked.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// Request is what one collection reads.
type Request struct {
	// Root is the repository's top directory.
	Root string
	// Owner returns the component that holds a repository path.
	Owner func(path string) (string, bool)
	// Changed lists the repository paths the change touches.
	Changed []string
}

// candidate is one spec the walk found, before it is read.
type candidate struct {
	format features.SpecFormat
	dir    string // the reference path the reader reads
	files  []string
}

// Collect finds and reads every spec under the root. A spec its reader
// refuses is a warning, and the others are still reported.
func Collect(req Request) ([]Spec, []diag.Diagnostic) {
	found := map[string]*candidate{}
	visited := 0
	_ = filepath.WalkDir(req.Root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if file != req.Root && skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > maxFiles {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(req.Root, file)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if format, dir, ok := classify(rel); ok {
			key := string(format) + "\x00" + dir
			c := found[key]
			if c == nil {
				c = &candidate{format: format, dir: dir}
				found[key] = c
			}
			c.files = append(c.files, rel)
		}
		return nil
	})

	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := map[string]bool{}
	for _, file := range req.Changed {
		changed[file] = true
	}
	specs := []Spec{}
	var diags []diag.Diagnostic
	for _, key := range keys {
		spec, findings, ok := read(req.Root, found[key])
		diags = append(diags, findings...)
		if !ok {
			continue
		}
		for _, file := range found[key].files {
			if changed[file] {
				spec.Changed = true
			}
		}
		if req.Owner != nil {
			if owner, ok := req.Owner(spec.File); ok {
				spec.Component = owner
			}
		}
		specs = append(specs, spec)
	}
	return specs, diags
}

// classify says which format a repository file belongs to and the directory
// the format's reader reads it from.
//
//   - Spec Kit: specs/<feature>/spec.md
//   - OpenSpec: openspec/specs/<capability>/spec.md
//   - Kiro: .kiro/specs/<feature>/requirements.md
//   - Gherkin: a .feature file, read on its own
func classify(rel string) (features.SpecFormat, string, bool) {
	parts := strings.Split(rel, "/")
	n := len(parts)
	base := parts[n-1]
	switch {
	case strings.HasSuffix(base, ".feature"):
		return features.SpecFormatGherkin, rel, true
	case n >= 3 && base == "requirements.md" && parts[n-3] == "specs" && n >= 4 && parts[n-4] == ".kiro":
		return features.SpecFormatKiro, path.Dir(rel), true
	case n >= 3 && base == "spec.md" && parts[n-3] == "specs" && n >= 4 && parts[n-4] == "openspec":
		return features.SpecFormatOpenSpec, path.Dir(rel), true
	case n >= 3 && base == "spec.md" && parts[n-3] == "specs":
		return features.SpecFormatSpecKit, path.Dir(rel), true
	}
	return "", "", false
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// featureID names the spec for the protocol's reader, which projects it onto
// a native spec of one feature.
func featureID(dir string) string {
	name := strings.TrimSuffix(path.Base(dir), ".feature")
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "spec"
	}
	return "observe/" + slug
}

func read(root string, c *candidate) (Spec, []diag.Diagnostic, bool) {
	reader, ok := features.SpecReaderFor(c.format)
	if !ok {
		return Spec{}, nil, false
	}
	input := features.SpecInput{
		Feature:   featureID(c.dir),
		Reference: features.SpecReference{Format: c.format, Path: c.dir},
	}
	for _, file := range c.files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			return Spec{}, []diag.Diagnostic{diag.Warningf(CodeUnreadable, file, "the %s spec %s cannot be read: %v", c.format, c.dir, err)}, false
		}
		input.Files = append(input.Files, features.SpecFile{Path: file, Data: data})
	}
	projection := features.ReadSpecSource(reader, input)
	if projection.Spec == nil {
		reasons := []string{}
		for _, finding := range projection.Findings {
			if finding.Severity == diag.Error {
				reasons = append(reasons, finding.String())
			}
		}
		return Spec{}, []diag.Diagnostic{diag.Warningf(CodeUnreadable, c.dir, "the %s spec %s cannot be read: %s",
			c.format, c.dir, strings.Join(reasons, "; "))}, false
	}
	spec := Spec{Format: string(c.format), Path: c.dir, File: c.files[0], Requirements: []Requirement{}}
	for i, requirement := range projection.Spec.Requirements {
		r := Requirement{ID: requirement.ID, Text: requirement.Text}
		if i < len(projection.Requirements) {
			position := projection.Requirements[i].Position
			r.Line = position.Line
			if position.File != "" {
				spec.File = position.File
			}
		}
		spec.Requirements = append(spec.Requirements, r)
	}
	return spec, nil, true
}
