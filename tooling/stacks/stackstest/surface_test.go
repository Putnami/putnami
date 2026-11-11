package stackstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const surfaceManifestJSON = `{
  "workspace": {"installInputs": ["Cargo.lock"]},
  "commands": {
    "lint": {"flags": {"docs-links": {"type": "boolean", "default": true}}, "run": [{"id": "lint", "task": "lint-run"}, {"id": "docs", "task": "lint-docs"}]},
    "test": {"flags": {"test": {"type": "string", "short": "-t"}}},
    "serve": {"flags": {"port": {"type": "number", "default": 3000}, "args": {"type": "string"}}},
    "run": {"flags": {"args": {"type": "string"}}},
    "package": {"flags": {"docker": {}, "docker-base-image": {}, "docker-tag": {}, "docker-load": {}, "dry-run": {}, "platform": {}, "port": {"type": "number", "default": 8080}}},
    "publish": {"flags": {"docker": {}, "docker-registry": {}, "dry-run": {}}},
    "workspace-install": {}
  }
}`

func surface(t *testing.T, manifest string, exemptions ...Exemption) *recorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "putnami.extension.json")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	func() {
		defer func() {
			if v := recover(); v != nil && v != r {
				panic(v)
			}
		}()
		CheckSurface(r, path, exemptions...)
	}()
	return r
}

func TestCheckSurfaceAcceptsTheSharedSurface(t *testing.T) {
	if r := surface(t, surfaceManifestJSON); len(r.errors) != 0 {
		t.Errorf("errors: %v", r.errors)
	}
}

func TestCheckSurfaceReportsEachDrift(t *testing.T) {
	drifted := strings.NewReplacer(
		`"args": {"type": "string"}}},
    "run"`, `"other": {}}},
    "run"`,
		`"default": 8080`, `"default": 3000`,
		`"short": "-t"`, `"short": "-f"`,
		`, {"id": "docs", "task": "lint-docs"}`, ``,
		`"installInputs": ["Cargo.lock"]`, `"installInputs": []`,
	).Replace(surfaceManifestJSON)
	r := surface(t, drifted)
	for _, want := range []string{"serve declares no args flag", "package flag port defaults to 3000", "short form is \"-f\"", "lint runs no lint-docs task", "declares no workspace.installInputs"} {
		if !strings.Contains(strings.Join(r.errors, "\n"), want) {
			t.Errorf("no error %q in %v", want, r.errors)
		}
	}
}

func TestCheckSurfaceHonorsAnExemptionWithItsReason(t *testing.T) {
	noFilter := strings.Replace(surfaceManifestJSON, `"test": {"flags": {"test": {"type": "string", "short": "-t"}}},`, `"test": {},`, 1)
	if r := surface(t, noFilter, Exemption{Convention: ConventionTestFilter, Reason: "the runner fails an empty selection"}); len(r.errors) != 0 {
		t.Errorf("an exempted filter: %v", r.errors)
	}
	if r := surface(t, noFilter, Exemption{Convention: ConventionTestFilter}); len(r.errors) != 1 || !strings.Contains(r.errors[0], "states no reason") {
		t.Errorf("an exemption with no reason: %v", r.errors)
	}
	if r := surface(t, surfaceManifestJSON, Exemption{Convention: ConventionTestFilter, Reason: "x"}); len(r.errors) != 1 || !strings.Contains(r.errors[0], "exempted but declared") {
		t.Errorf("an exempted filter that is declared: %v", r.errors)
	}
	if r := surface(t, `{`); !r.fatal {
		t.Errorf("an unreadable manifest is fatal: %v", r.errors)
	}
}
