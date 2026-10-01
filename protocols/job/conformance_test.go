package job

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidate(data); diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			// Lenient parse must accept everything strict parse accepts.
			if _, err := Parse(data); err != nil {
				t.Errorf("lenient parse of %s failed: %v", path, err)
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseAndValidate(data); !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

// TestForwardCompatibility pins the lenient consumer path: an older
// extension must keep working when a newer orchestrator adds fields.
func TestForwardCompatibility(t *testing.T) {
	data, err := os.ReadFile("fixtures/invalid/unknown-field.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := Parse(data)
	if err != nil {
		t.Fatalf("lenient parse must ignore unknown fields: %v", err)
	}
	if ctx.Job.Name != "build" {
		t.Errorf("job name = %q, want build", ctx.Job.Name)
	}
}

func TestParamCoercion(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if got := ctx.Params.Bool("minify", false); !got {
		t.Error("minify should resolve true")
	}
	if got := ctx.Params.Float("coverage-threshold", 0, "coverageThreshold"); got != 80 {
		t.Errorf("coverage-threshold = %v, want 80 (numeric string coercion)", got)
	}
	if got := ctx.Params.Int("missing", 7); got != 7 {
		t.Errorf("missing param = %d, want default 7", got)
	}
	if got := ctx.Params.String("missing", "coverage-threshold"); got != "80" {
		t.Errorf("fallback resolution = %q, want 80", got)
	}
}

func TestContextHelpers(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if !ctx.HasPublishChannel("npm") {
		t.Error("expected npm publish channel")
	}
	if ctx.HasPublishChannel("docker") {
		t.Error("unexpected docker publish channel")
	}
	if got := ctx.Project.GetBinString(); got != "dist/cli.js" {
		t.Errorf("bin = %q, want dist/cli.js", got)
	}
}
