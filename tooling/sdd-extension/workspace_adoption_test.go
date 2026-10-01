package sdd

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

// extensionProjectID is how a project in THIS workspace names this extension.
// A workspace-local extension is attached by its project id, not by its publish
// name, because nothing has been published yet.
const extensionProjectID = "/tooling/sdd-extension"

// TestEveryProjectThatAuthorsSDDDocumentsDeclaresThisExtension closes the one
// silent hole in the way `validate` reaches a project.
//
// Activation is TWO conditions, and both are necessary: the command's
// activationFiles must match the project's tree, AND the project must name the
// providing extension in its `extensions` array (isProjectExtensionDep,
// tooling/cli/internal/jobs/planner_match.go). The second is the workspace's
// opt-in and it is right that it exists — an optional DX vertical must cost
// nothing in a workspace that does not use it.
//
// What it costs HERE is that a project which starts authoring a spec and
// forgets the declaration is not validated, and nothing says so: the command
// simply plans no job for it, which looks exactly like a project that has
// nothing to validate. Every other extension's version of this mistake is loud
// — a Go project that omits /go/extension does not build — and this one is
// silent, so it is asserted instead.
//
// The test walks the repository it lives in. That is deliberate and it is not
// the boundary the extension binary must respect: this is a check on the
// WORKSPACE's adoption of the gate, not a runtime derivation of workspace
// facts, and it never runs inside the extension.
func TestEveryProjectThatAuthorsSDDDocumentsDeclaresThisExtension(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "opt-in-activation", "every-authoring-project-declares-the-extension")
	root := workspaceRoot(t)
	authoring := projectsAuthoringSDDDocuments(t, root)
	if len(authoring) == 0 {
		t.Fatal("no project in this workspace authors a feature manifest or a spec; this assertion would pass vacuously")
	}

	var missing []string
	for _, projectDir := range authoring {
		config := readProjectConfig(t, filepath.Join(root, projectDir, "putnami.json"))
		if config == nil {
			missing = append(missing, projectDir+" (no putnami.json)")
			continue
		}
		declared := false
		for _, name := range config.Extensions {
			if name == extensionProjectID || name == "@putnami/sdd" {
				declared = true
				break
			}
		}
		if !declared {
			missing = append(missing, projectDir)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("these projects author SDD documents but do not declare %s, so `putnami validate` plans no job for them and their documents are never checked:\n  %s",
			extensionProjectID, strings.Join(missing, "\n  "))
	}
}

// projectsAuthoringSDDDocuments returns the workspace-relative directories that
// carry a durable feature manifest or at least one canonical spec — exactly the
// `validate` command's activationFiles, evaluated the way the planner evaluates
// them.
func projectsAuthoringSDDDocuments(t *testing.T, root string) []string {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(path)
		switch {
		case entry.Name() == featureproto.ManifestFilename:
			found[relative(root, dir)] = true
		case filepath.Base(dir) == featureproto.SpecDirectory && strings.HasSuffix(entry.Name(), ".json"):
			found[relative(root, filepath.Dir(dir))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the workspace: %v", err)
	}
	// The workspace root itself is not a project and declares no extensions.
	delete(found, "")
	dirs := make([]string, 0, len(found))
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func readProjectConfig(t *testing.T, path string) *wsproto.ProjectConfig {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a workspace-relative project manifest inside this repository
	if err != nil {
		return nil
	}
	var config wsproto.ProjectConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &config
}

// workspaceRoot walks up from this package until it finds the workspace
// configuration, so the test does not depend on how deep the project sits.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above %s", wsproto.WorkspaceConfigFilename, dir)
		}
		dir = parent
	}
}
