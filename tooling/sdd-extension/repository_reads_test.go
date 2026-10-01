package sdd

import (
	"path/filepath"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The repository files this project's other tests read are declared test
// inputs, so editing one moves the test task's cache key instead of replaying
// a verdict recorded on other bytes: the domain manifests the architecture pins
// hold, the cloud-pilot fixtures, the protocol fixtures internal/sdd and
// internal/features compare against, and every feature manifest and spec the
// adoption walk finds.
func TestRepositoryReadsOfTheOtherTestsAreDeclaredTestInputs(t *testing.T) {
	root := workspaceRoot(t)
	read := []string{
		"protocols/putnami.architecture.json",
		"sites/telemetry.putnami.dev/putnami.architecture.json",
		"go/framework/putnami.architecture.json",
		"typescript/framework/putnami.architecture.json",
		"sites/putnami.dev/putnami.architecture.json",
		"tooling/contributor/putnami.architecture.json",
		"protocols/architecture/fixtures/valid/cloud-pilot/runtime.json",
		"protocols/architecture/fixtures/valid/cloud-pilot/observability.json",
		"protocols/architecture/fixtures/conformance/typescript-emitted-capabilities.json",
		"protocols/features/fixtures/equivalence/go-typescript-design.golden.json",
		"protocols/capabilities/fixtures/v2/valid/all-kinds.json",
		"protocols/capabilities/fixtures/valid/full.json",
	}
	authoring := projectsAuthoringSDDDocuments(t, root)
	if len(authoring) == 0 {
		t.Fatal("the adoption walk found no project authoring SDD documents; the assertion would pass vacuously")
	}
	for _, dir := range authoring {
		if manifest := dir + "/" + featureproto.ManifestFilename; fileExists(filepath.Join(root, filepath.FromSlash(manifest))) {
			read = append(read, manifest)
		}
		specs, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), featureproto.SpecDirectory, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range specs {
			read = append(read, relative(root, spec))
		}
	}

	projectRoot := filepath.Join(root, "tooling", "sdd-extension")
	patterns := declaredTestInputs(t, projectRoot)
	for _, rel := range read {
		fromProject, err := filepath.Rel(projectRoot, filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !wsproto.SelectsPath(filepath.ToSlash(fromProject), patterns) {
			t.Errorf("%s is read by this project's tests but is not a declared test input (options.test.filePatterns in tooling/sdd-extension/putnami.json)", rel)
		}
	}
}
