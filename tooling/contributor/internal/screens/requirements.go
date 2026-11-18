package screens

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"strings"

	features "go.putnami.dev/protocol/features"
)

// Requirement names one feature requirement, with the keys a test and a
// page proof bind with.
type Requirement struct {
	Feature     string
	Requirement string
}

func (r Requirement) String() string { return r.Feature + "#" + r.Requirement }

// ReadAt reads a workspace-relative file at a commit, and tells whether the
// commit holds it.
type ReadAt func(commit, file string) ([]byte, bool, error)

// FeatureFile reports whether a workspace-relative path is a feature
// declaration: a putnami.features.json, or a .json file directly under a
// features/ directory.
func FeatureFile(file string) bool {
	dir, name := path.Split(file)
	if name == features.ManifestFilename {
		return true
	}
	return strings.HasSuffix(name, ".json") && path.Base(strings.TrimSuffix(dir, "/")) == features.FeatureDirectory
}

// TouchedRequirements returns the requirements a change adds, or whose
// declaration it changes, in the feature declarations among the changed
// workspace-relative paths, read at base and at head. A requirement the head
// no longer declares is not touched: no page proves it at the head.
func TouchedRequirements(changed []string, base, head string, read ReadAt) (map[Requirement]bool, error) {
	// The declarations of all changed files are compared together, so a
	// requirement that moves between two files, or between the two forms of
	// declaration, is not touched by the move.
	before, after := map[Requirement][]byte{}, map[Requirement][]byte{}
	for _, file := range changed {
		if !FeatureFile(file) {
			continue
		}
		for _, side := range []struct {
			commit   string
			declared map[Requirement][]byte
		}{{base, before}, {head, after}} {
			found, err := declaredAt(read, side.commit, file)
			if err != nil {
				return nil, err
			}
			maps.Copy(side.declared, found)
		}
	}
	touched := map[Requirement]bool{}
	for requirement, declaration := range after {
		if previous, held := before[requirement]; !held || !bytes.Equal(previous, declaration) {
			touched[requirement] = true
		}
	}
	return touched, nil
}

// declaredAt returns each requirement a feature declaration file holds at a
// commit, with its canonical declaration; none when the commit lacks the
// file.
func declaredAt(read ReadAt, commit, file string) (map[Requirement][]byte, error) {
	data, found, err := read(commit, file)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", file, short(commit), err)
	}
	if !found {
		return nil, nil
	}
	// A file the feature parsers refuse declares no requirement here: a
	// features/ directory of an application may hold other JSON, and the
	// gate's validate refuses a malformed declaration.
	var declared []features.Feature
	if path.Base(file) == features.ManifestFilename {
		if manifest, _ := features.ParseManifest(data); manifest != nil {
			declared = manifest.Features
		}
	} else if document, _ := features.ParseFeatureDocument(data); document != nil {
		declared = []features.Feature{document.Feature}
	}
	requirements := map[Requirement][]byte{}
	for _, feature := range declared {
		for _, requirement := range feature.Requirements {
			canonical, err := json.Marshal(requirement)
			if err != nil {
				return nil, err
			}
			requirements[Requirement{Feature: feature.ID, Requirement: requirement.ID}] = canonical
		}
	}
	return requirements, nil
}
