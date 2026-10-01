package workspace

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The base conformance_test.go covers only the workspace Config shape. This file
// graduates the other schema-bearing shapes into the corpus:
// ProjectConfig, ScopeConfig, and the previously unbound lock file. Each shape
// gets a valid/ and invalid/ fixture directory routed through its strict parser.

// shapeParser strict-parses+validates one wire shape and reports whether it
// produced an error diagnostic.
type shapeParser func([]byte) []diag.Diagnostic

func runShapeConformance(t *testing.T, shape string, parse shapeParser) {
	t.Helper()
	for _, kind := range []struct {
		dir        string
		wantErrors bool
	}{{"valid", false}, {"invalid", true}} {
		glob := filepath.Join("fixtures", shape, kind.dir, "*.json")
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no fixtures matched %s", glob)
		}
		for _, path := range files {
			t.Run(shape+"/"+kind.dir+"/"+filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				diags := parse(data)
				hasErr := diag.HasErrors(diags)
				if kind.wantErrors && !hasErr {
					t.Errorf("invalid fixture %s should produce errors but none found", path)
				}
				if !kind.wantErrors && hasErr {
					t.Errorf("valid fixture %s produced errors: %v", path, diags)
				}
			})
		}
	}
}

func TestConformance_ProjectConfigFixtures(t *testing.T) {
	runShapeConformance(t, "project", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateProjectConfig(b)
		return d
	})
}

func TestConformance_ScopeConfigFixtures(t *testing.T) {
	runShapeConformance(t, "scope", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateScopeConfig(b)
		return d
	})
}

func TestConformance_LockFixtures(t *testing.T) {
	runShapeConformance(t, "lock", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateLock(b)
		return d
	})
}

func TestConformance_AgentArtifactFixtures(t *testing.T) {
	runShapeConformance(t, "agent-artifact", func(b []byte) []diag.Diagnostic {
		_, d := ParseAndValidateAgentArtifactManifest(b)
		return d
	})
}
