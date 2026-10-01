package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type experimentalTemplateManifest struct {
	Name                     string            `json:"name"`
	Description              string            `json:"description"`
	Extension                string            `json:"extension"`
	WorkspaceDevDependencies map[string]string `json:"workspaceDevDependencies"`
}

type experimentalSupportEntry struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Default *bool  `json:"default"`
	Parity  string `json:"parity"`
}

type experimentalSupportCatalog struct {
	Entries []experimentalSupportEntry `json:"entries"`
}

// TestExperimentalPythonTemplatesDeclareTheirBoundary keeps the policy and
// generated template surface together. The support catalog is the reviewed
// authority; the descriptor, generated manifest, and README make that same
// boundary visible before an experimenter creates a project.
func TestExperimentalPythonTemplatesDeclareTheirBoundary(t *testing.T) {
	spectest.Proves(t, "python/experimental-library-template", "explicit-template", "the-library-template-is-named-and-declares-the-python-extension")
	spectest.Proves(t, "python/experimental-library-template", "minimal-library-shape", "the-library-template-ships-setuptools-metadata-and-a-greeting-test")
	spectest.Proves(t, "python/experimental-library-template", "experimental-disclosure", "the-library-catalog-record-and-readme-disclose-the-experimental-boundary")
	spectest.Proves(t, "python/experimental-server-template", "explicit-template", "the-server-template-is-named-and-declares-the-python-extension")
	spectest.Proves(t, "python/experimental-server-template", "minimal-server-shape", "the-server-template-ships-fastapi-uvicorn-and-a-root-response-test")
	spectest.Proves(t, "python/experimental-server-template", "experimental-disclosure", "the-server-catalog-record-and-readme-disclose-the-experimental-boundary")
	root := filepath.Join("..", "..", "..", "..")
	catalog := readExperimentalSupportCatalog(t, root)
	assertExperimentalSupportEntry(t, catalog, "@putnami/python")

	for _, template := range []struct {
		name          string
		shapeExpected []string
	}{
		{
			name:          "python-server",
			shapeExpected: []string{"fastapi>=", "uvicorn>=", "client.get(\"/\")", `{"Hello": "World"}`},
		},
		{
			name:          "python-library",
			shapeExpected: []string{"setuptools>=", "def test_hello", "assert hello(\"World\")"},
		},
	} {
		directory := filepath.Join(root, "python", "templates", template.name)
		manifest := readExperimentalTemplateManifest(t, filepath.Join(directory, "putnami.template.json"))
		if manifest.Name != template.name {
			t.Errorf("template name = %q, want %q", manifest.Name, template.name)
		}
		if manifest.Extension != "@putnami/python" {
			t.Errorf("%s extension = %q, want @putnami/python", template.name, manifest.Extension)
		}
		if manifest.WorkspaceDevDependencies["go.putnami.dev/python/extension"] == "" {
			t.Errorf("%s does not declare the Python extension dependency", template.name)
		}
		for _, disclosure := range []string{"experimental", "non-default", "parity"} {
			if !strings.Contains(strings.ToLower(manifest.Description), disclosure) {
				t.Errorf("%s description does not disclose %q: %q", template.name, disclosure, manifest.Description)
			}
		}

		projectManifest := readExperimentalFile(t, filepath.Join(directory, "putnami.json.template"))
		if !strings.Contains(projectManifest, `"@putnami/python"`) {
			t.Errorf("%s generated project manifest does not explicitly opt into @putnami/python", template.name)
		}
		shape := readExperimentalFile(t, filepath.Join(directory, "pyproject.toml.template")) +
			readExperimentalFile(t, filepath.Join(directory, "tests", map[string]string{
				"python-server":  "test_server.py.template",
				"python-library": "test_hello.py.template",
			}[template.name]))
		for _, expected := range template.shapeExpected {
			if !strings.Contains(shape, expected) {
				t.Errorf("%s generated shape does not contain %q", template.name, expected)
			}
		}
		readme := strings.ToLower(readExperimentalFile(t, filepath.Join(directory, "README.md.template")))
		for _, disclosure := range []string{"experimental", "not a default", "no feature or compatibility parity"} {
			if !strings.Contains(readme, disclosure) {
				t.Errorf("%s generated README does not disclose %q", template.name, disclosure)
			}
		}
		assertExperimentalTemplateDoesNotPublishAuthority(t, directory)
		assertExperimentalSupportEntry(t, catalog, template.name)
	}
}

// TestExperimentalPythonContractInputsAreCached prevents the static contract
// test from returning a stale pass when its reviewed inputs change outside the
// Go package itself.
func TestExperimentalPythonContractInputsAreCached(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "experimental-boundary", "the-experimental-surface-contract-inputs-are-cache-keyed")
	root := filepath.Join("..", "..", "..", "..")
	var project struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(readExperimentalFile(t, filepath.Join(root, "python", "extension", "putnami.json"))), &project); err != nil {
		t.Fatalf("parse Python extension project config: %v", err)
	}
	for _, want := range []string{
		"../../putnami.support.json",
		"../templates/python-server/**",
		"../templates/python-library/**",
		"putnami.extension.json",
	} {
		if !containsExperimentalString(project.Options.Test.FilePatterns, want) {
			t.Errorf("test filePatterns = %v, missing %q", project.Options.Test.FilePatterns, want)
		}
	}
}

// TestExperimentalPythonUpgradeMessagingDoesNotClaimAFrameworkReleaseSet
// protects the upgrade boundary. The job upgrades the uv-resolved dependencies
// of explicitly configured Python projects; it does not invent a framework
// package release set or make Python a default upgrade path.
func TestExperimentalPythonUpgradeMessagingDoesNotClaimAFrameworkReleaseSet(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "experimental-boundary", "upgrade-messaging-never-implies-a-supported-framework-release-set")
	manifest := readExperimentalFile(t, filepath.Join("..", "..", "putnami.extension.json"))
	for _, text := range []string{
		"explicitly configured experimental Python projects",
		"not a Putnami Python framework release set",
	} {
		if !strings.Contains(manifest, text) {
			t.Errorf("extension manifest does not state %q", text)
		}
	}
}

func readExperimentalTemplateManifest(t *testing.T, path string) experimentalTemplateManifest {
	t.Helper()
	var manifest experimentalTemplateManifest
	if err := json.Unmarshal([]byte(readExperimentalFile(t, path)), &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return manifest
}

func readExperimentalSupportCatalog(t *testing.T, root string) map[string]experimentalSupportEntry {
	t.Helper()
	var catalog experimentalSupportCatalog
	if err := json.Unmarshal([]byte(readExperimentalFile(t, filepath.Join(root, "putnami.support.json"))), &catalog); err != nil {
		t.Fatalf("parse support catalog: %v", err)
	}
	entries := make(map[string]experimentalSupportEntry, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		entries[entry.ID] = entry
	}
	return entries
}

func assertExperimentalSupportEntry(t *testing.T, catalog map[string]experimentalSupportEntry, id string) {
	t.Helper()
	entry, found := catalog[id]
	if !found {
		t.Errorf("support catalog has no %s entry", id)
		return
	}
	if entry.Kind != "package" {
		t.Errorf("support catalog entry for %s kind = %q, want package", id, entry.Kind)
	}
	if entry.Status != "experimental" {
		t.Errorf("support catalog entry for %s status = %q, want experimental", id, entry.Status)
	}
	if entry.Default == nil {
		t.Errorf("support catalog entry for %s default is missing, want false", id)
	} else if *entry.Default {
		t.Errorf("support catalog entry for %s default = true, want false", id)
	}
	if entry.Parity != "unsupported" {
		t.Errorf("support catalog entry for %s parity = %q, want unsupported", id, entry.Parity)
	}
}

func assertExperimentalTemplateDoesNotPublishAuthority(t *testing.T, directory string) {
	t.Helper()
	for _, forbidden := range []string{"putnami.features.json", "specs"} {
		_, err := os.Stat(filepath.Join(directory, forbidden))
		if err == nil {
			t.Errorf("template %s must not ship %s", directory, forbidden)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stat %s/%s: %v", directory, forbidden, err)
		}
	}
}

func containsExperimentalString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readExperimentalFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The library template's shape requirement names a src-layout package, which
// the boundary test above does not reach: it reads pyproject.toml.template and
// the test file, not the package tree. A flat-layout template would still pass
// there while shipping a project setuptools cannot package the declared way.
func TestExperimentalPythonLibraryUsesSrcLayout(t *testing.T) {
	spectest.Proves(t, "python/experimental-library-template", "minimal-library-shape", "the-library-template-is-a-src-layout-package")
	root := filepath.Join("..", "..", "..", "..")
	pkg := filepath.Join(root, "python", "templates", "python-library", "src", "__module__")

	for _, name := range []string{"__init__.py", "hello.py"} {
		if _, err := os.Stat(filepath.Join(pkg, name)); err != nil {
			t.Errorf("python-library is not a src-layout package: %s missing: %v", name, err)
		}
	}
}

// The server template's shape requirement names configured run and serve
// entrypoints. The boundary test reads pyproject.toml.template and the test
// file; the entrypoints live in putnami.json.template, so nothing checked
// them. Without them `putnami serve` has no entrypoint to resolve.
func TestExperimentalPythonServerConfiguresRunAndServeEntrypoints(t *testing.T) {
	spectest.Proves(t, "python/experimental-server-template", "minimal-server-shape", "the-server-template-configures-run-and-serve-entrypoints")
	root := filepath.Join("..", "..", "..", "..")
	manifest := readExperimentalFile(t, filepath.Join(root, "python", "templates", "python-server", "putnami.json.template"))

	for _, task := range []string{"@putnami/python:run", "@putnami/python:serve"} {
		if !strings.Contains(manifest, task) {
			t.Errorf("python-server declares no %s entrypoint", task)
		}
	}
	if !strings.Contains(manifest, `"entrypoint"`) {
		t.Error("python-server names no entrypoint for its run and serve tasks")
	}
}
