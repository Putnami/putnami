package screens

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/screenshots"
)

// ManifestFile is the name of the manifest in the output directory.
const ManifestFile = screenshots.ManifestFile

// Sides are the two sides of a change a page is captured on.
const (
	SideBefore = screenshots.SideBefore
	SideAfter  = screenshots.SideAfter
)

// The manifest is a go.putnami.dev/protocol/screenshots document: the
// protocol owns its members, its closed sets, its rules and its bounds.
type (
	// Manifest is the record of one run of putnami change screenshots.
	Manifest = screenshots.Manifest
	// Page is one touched page: its page file, the route it is captured at,
	// the path visited for that route, and the sides that hold the page.
	Page = screenshots.Page
	// Proof is one feature requirement a page proves.
	Proof = screenshots.Proof
	// Capture is one PNG file of the manifest, relative to the manifest's
	// directory, with the status and the final URL of its document.
	Capture = screenshots.Capture
)

// Digest returns the hex sha256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var unsafeSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a route path or a project directory into a file name part.
func Slug(text string) string {
	slug := strings.Trim(unsafeSlug.ReplaceAllString(strings.ToLower(text), "-"), "-")
	if slug == "" {
		return "root"
	}
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return slug
}

// CaptureName names the file of one capture of the page at index i.
func CaptureName(i int, page Page, side string, viewport Viewport, scheme string) string {
	return fmt.Sprintf("%02d-%s-%s-%s-%s.png", i+1, Slug(page.Route), side, viewport.Name, scheme)
}

// ReadManifest reads the manifest at file with the protocol decoder, which
// refuses a manifest the contract does not allow.
func ReadManifest(file string) (Manifest, error) {
	data, err := readBounded(file, screenshots.MaxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	manifest, diags := screenshots.Decode(data)
	if manifest == nil {
		return Manifest{}, fmt.Errorf("not a screenshots manifest:\n%s", diag.ErrorText(diags))
	}
	return *manifest, nil
}

// WriteManifest writes the manifest into dir through the protocol, which
// refuses to serialize a manifest its decoder would refuse.
func WriteManifest(dir string, manifest Manifest) (string, error) {
	data, err := screenshots.Marshal(manifest)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, ManifestFile)
	return file, os.WriteFile(file, data, 0o644)
}

// Check verifies a manifest read from dir against a change: it is a version 2
// manifest that names head and project, it holds every capture each of its
// pages needs on each side that holds it, each capture file is intact, every
// touched route holds its after captures, and names each touched requirement
// its head route proves. It returns every problem it finds.
func Check(m Manifest, dir, head, project string, touched []Page, requirements map[Requirement]bool) []string {
	var problems []string
	if m.ProtocolVersion != screenshots.ProtocolVersion {
		problems = append(problems, fmt.Sprintf("it is a version %d manifest, which names no requirement: run putnami change screenshots again", m.ProtocolVersion))
	}
	if m.HeadSHA != head {
		problems = append(problems, fmt.Sprintf("it captures %s, not the head %s", short(m.HeadSHA), short(head)))
	}
	if filepath.Clean(m.Project) != filepath.Clean(project) {
		problems = append(problems, fmt.Sprintf("it captures the project %s, not %s", m.Project, project))
	}
	type key struct{ route, side, viewport, scheme string }
	have := map[key]bool{}
	for _, capture := range m.Captures {
		name := capture.File
		if !ownFile(name) {
			problems = append(problems, fmt.Sprintf("the capture %s is not a file of its directory", capture.File))
			continue
		}
		data, err := readBounded(filepath.Join(dir, name), screenshots.MaxCaptureBytes)
		if err != nil {
			problems = append(problems, fmt.Sprintf("the capture %s is missing: %v", capture.File, unwrapPathError(err)))
			continue
		}
		if Digest(data) != capture.SHA256 {
			problems = append(problems, fmt.Sprintf("the capture %s does not hold the bytes the manifest hashed", capture.File))
			continue
		}
		have[key{capture.Route, capture.Side, capture.Viewport, capture.Scheme}] = true
	}
	need := func(route, side string) {
		for _, viewport := range Viewports {
			for _, scheme := range Schemes {
				if !have[key{route, side, viewport.Name, scheme}] {
					problems = append(problems, fmt.Sprintf("%s has no %s %s %s capture", route, side, viewport.Name, scheme))
				}
			}
		}
	}
	pages := map[string]Page{}
	for _, page := range m.Pages {
		pages[page.Route] = page
		if page.Before {
			need(page.Route, SideBefore)
		}
		if page.After {
			need(page.Route, SideAfter)
		}
	}
	for _, page := range touched {
		if !page.After {
			continue
		}
		recorded, ok := pages[page.Route]
		if !ok || !recorded.After {
			problems = append(problems, fmt.Sprintf("the touched page %s (%s) is not captured", page.Route, page.File))
			continue
		}
		named := map[screenshots.Proof]bool{}
		for _, proof := range recorded.Proves {
			named[proof] = true
		}
		for _, proof := range page.Proves {
			if requirements[Requirement{Feature: proof.Feature, Requirement: proof.Requirement}] && !named[proof] {
				problems = append(problems, fmt.Sprintf("the page %s does not name the touched requirement %s#%s it proves", page.Route, proof.Feature, proof.Requirement))
			}
		}
	}
	return problems
}

// ownFile reports whether a manifest's file name names a PNG file directly in
// the manifest's directory.
func ownFile(name string) bool {
	return name == filepath.Base(name) && name != "." && name != ".." && strings.HasSuffix(name, ".png") &&
		!strings.ContainsAny(name, "/\\")
}

// Plural writes a count with its noun, singular for one.
func Plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "no commit"
	}
	return sha
}

func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
