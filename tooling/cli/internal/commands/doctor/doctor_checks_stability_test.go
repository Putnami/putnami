package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// stabilityWorkspaceVersion is the version the seeded fixture's committed
// OpenAPI spec carries. Nothing derives it any more — the workspace declares no
// version — so it is only there to give the spec a well-formed info block.
const stabilityWorkspaceVersion = "4.5.6"

// stableBinding is a syntactically valid source-v1 binding (the parser rejects
// malformed ones, and a malformed value would be reported as an invalid manifest
// instead of as the volatility class under test).
const stableBinding = "source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

// seedStabilityWorkspace writes a one-project fixture workspace whose committed
// artifacts carry every workspace-derived content class the stability check
// knows about: the workspace version in an OpenAPI spec, a dependency-closure
// package entry in the capability manifest, and a source binding in that
// manifest's provenance. projectConfig is the project's authored putnami.json,
// which is what decides whether those values are DECLARED (and therefore fine)
// or inherited.
func seedStabilityWorkspace(t *testing.T, projectConfig string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("putnami.workspace.json", `{"name":"fixture"}`)
	write("app/putnami.json", projectConfig)
	write("app/schema/openapi.json", `{
  "openapi": "3.0.3",
  "info": { "title": "app", "version": "`+stabilityWorkspaceVersion+`" },
  "paths": {}
}`)
	write("app/schema/http-routes.json", `{
  "$schema": "https://putnami.dev/schemas/putnami-http-routes-v1.json",
  "protocol": "putnami.http-routes.v1",
  "routes": [],
  "digest": "sha256:fd4d81a72818dde428f4413b2bd84c3ae4cf53e296ce9187cc3d1156b965a03a"
}`)
	// example/lib declares the config contribution, so it is part of the
	// manifest's capability surface; example/unrelated contributes nothing and is
	// pure closure.
	write("app/schema/capabilities.json", `{
  "protocolVersion": 2,
  "project": "example/app",
  "configDefinitions": [
    {
      "identity": { "ownerProject": "example/app", "kind": "config", "key": "database.default" },
      "path": "database.default",
      "provenance": {
        "project": "example/app",
        "package": "example/lib",
        "sourceKind": "framework",
        "sourceBinding": "`+stableBinding+`",
        "declaration": { "root": "package", "path": "plugin.go" }
      }
    }
  ],
  "packages": [
    {
      "identity": { "ownerProject": "example/lib", "kind": "package", "key": "example/lib" },
      "package": "example/lib",
      "provenance": {
        "project": "example/lib",
        "package": "example/lib",
        "sourceKind": "framework",
        "declaration": { "root": "project", "path": "putnami.json" }
      }
    },
    {
      "identity": { "ownerProject": "example/unrelated", "kind": "package", "key": "example/unrelated" },
      "package": "example/unrelated",
      "provenance": {
        "project": "example/unrelated",
        "package": "example/unrelated",
        "sourceKind": "framework",
        "declaration": { "root": "project", "path": "putnami.json" }
      }
    }
  ]
}`)
	return root
}

// declaringProjectConfig is a project that authored its commit regime but
// declares no version of its own — the state that leaves it inheriting the
// workspace version.
const declaringProjectConfig = `{"name":"example/app","options":{"generate":{"schema":true}}}`

// TestDoctorFlagsWorkspaceDerivedCommittedContent pins the stability contract: each
// workspace-derived content class in a committed generated artifact surfaces as
// an attributable finding — naming the artifact and the field — and none of them
// blocks a profile.
func TestDoctorFlagsWorkspaceDerivedCommittedContent(t *testing.T) {
	root := seedStabilityWorkspace(t, declaringProjectConfig)

	report, err := DoctorRun(root, []*workspace.Project{proj("app")}, doctor.ProfileProduction, doctorTestClock)
	if err != nil {
		t.Fatalf("stability findings must not block a profile: %v", err)
	}

	byField := map[string]doctor.Finding{}
	for _, f := range report.Findings {
		if f.Code != doctor.CheckCommittedManifestStability {
			continue
		}
		if f.Severity != doctor.SeverityWarning {
			t.Errorf("finding %s graded %s under production, want an advisory warning", f.Evidence[0].Field, f.Severity)
		}
		byField[f.Evidence[0].Field] = f
	}
	for field, wantPath := range map[string]string{
		"packages":      "app/schema/capabilities.json",
		"sourceBinding": "app/schema/capabilities.json",
	} {
		got, ok := byField[field]
		if !ok {
			t.Fatalf("no stability finding for %q; got %#v", field, report.Findings)
		}
		if got.Evidence[0].Path != wantPath {
			t.Errorf("finding %q evidence path = %q, want %q", field, got.Evidence[0].Path, wantPath)
		}
		if got.Remediation == "" {
			t.Errorf("finding %q carries no remediation", field)
		}
	}
	// The closure finding names the offending package so a reader can act
	// without opening the manifest, and never the entries that legitimately
	// belong to the surface.
	closure := byField["packages"].Message
	if !strings.Contains(closure, "example/unrelated") || strings.Contains(closure, "example/lib") {
		t.Errorf("closure message = %q, want it to name only the non-contributing package", closure)
	}
}

// TestDoctorFlagsClosureInHistoricalPackageVersions keeps the read-path
// compatibility contract honest: old v2 manifests remain valid input, but
// their packageVersions entries are still subject to the same closure check as
// the current packages representation.
func TestDoctorFlagsClosureInHistoricalPackageVersions(t *testing.T) {
	root := seedStabilityWorkspace(t, declaringProjectConfig)
	path := filepath.Join(root, "app", "schema", "capabilities.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	historical := strings.Replace(string(data), `"packages": [`, `"packageVersions": [`, 1)
	historical = strings.Replace(historical, `"package": "example/lib",`, `"package": "example/lib",
      "version": "1.0.0",`, 1)
	historical = strings.Replace(historical, `"package": "example/unrelated",`, `"package": "example/unrelated",
      "version": "1.0.0",`, 1)
	if err := os.WriteFile(path, []byte(historical), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := DoctorRun(root, []*workspace.Project{proj("app")}, doctor.ProfileProduction, doctorTestClock)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Code == doctor.CheckCommittedManifestStability && finding.Evidence[0].Field == "packages" {
			if !strings.Contains(finding.Message, "example/unrelated") {
				t.Fatalf("historical closure finding = %q, want unrelated package", finding.Message)
			}
			return
		}
	}
	t.Fatalf("historical packageVersions closure was not reported: %#v", report.Findings)
}

// TestDoctorStabilityWaiverPath exercises the waiver path for the new code: a
// project-scoped waiver suppresses the whole class, and a field-scoped one
// accepts exactly one finding while the others keep reporting.
func TestDoctorStabilityWaiverPath(t *testing.T) {
	waive := func(t *testing.T, root, field string) doctor.Report {
		t.Helper()
		waiver := `{"protocolVersion":1,"waivers":[{"code":"doctor.committed_manifest_stability","project":"/app"`
		if field != "" {
			waiver += `,"field":"` + field + `"`
		}
		waiver += `,"owner":"platform","reason":"regeneration scheduled","expires":"2030-01-01"}]}`
		if err := os.WriteFile(filepath.Join(root, doctor.WaiverFilename), []byte(waiver), 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := DoctorRun(root, []*workspace.Project{proj("app")}, doctor.ProfileProduction, doctorTestClock)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	stability := func(report doctor.Report) (total, waived int) {
		for _, f := range report.Findings {
			if f.Code != doctor.CheckCommittedManifestStability {
				continue
			}
			total++
			if f.WaivedBy != nil {
				waived++
			}
		}
		return total, waived
	}

	root := seedStabilityWorkspace(t, declaringProjectConfig)
	total, waived := stability(waive(t, root, ""))
	if total != 2 || waived != 2 {
		t.Fatalf("project-scoped waiver: %d/%d stability findings waived, want 2/2", waived, total)
	}

	total, waived = stability(waive(t, root, "packages"))
	if total != 2 || waived != 1 {
		t.Fatalf("field-scoped waiver: %d/%d stability findings waived, want 1/2", waived, total)
	}
}

// TestDoctorSchemaCommitRegime pins task 4: a project that tracks generated
// schemas without declaring options.generate.schema is warned once, and either
// declared value clears it — the check is about the decision being authored, not
// about which way it went.
func TestDoctorSchemaCommitRegime(t *testing.T) {
	tests := map[string]struct {
		config string
		want   bool
	}{
		"undeclared":       {config: `{"name":"example/app"}`, want: true},
		"declared true":    {config: `{"name":"example/app","options":{"generate":{"schema":true}}}`, want: false},
		"declared false":   {config: `{"name":"example/app","options":{"generate":{"schema":false}}}`, want: false},
		"unrelated option": {config: `{"name":"example/app","options":{"serve":{"port":3000}}}`, want: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := seedStabilityWorkspace(t, test.config)
			report, err := DoctorRun(root, []*workspace.Project{proj("app")}, doctor.ProfileProduction, doctorTestClock)
			if err != nil {
				t.Fatal(err)
			}
			var got []doctor.Finding
			for _, f := range report.Findings {
				if f.Code == doctor.CheckUndeclaredSchemaCommit {
					got = append(got, f)
				}
			}
			switch {
			case test.want && len(got) != 1:
				t.Fatalf("want exactly one commit-regime finding, got %#v", got)
			case test.want && got[0].Evidence[0].Path != "app/putnami.json":
				t.Fatalf("commit-regime evidence points at %q, want the project's own config", got[0].Evidence[0].Path)
			case test.want && !strings.Contains(got[0].Message, "schema/http-routes.json"):
				t.Fatalf("commit-regime finding omits the committed route schema: %s", got[0].Message)
			case !test.want && len(got) != 0:
				t.Fatalf("declared regime still warned: %#v", got)
			}
		})
	}
}

// TestDoctorStabilityIsQuietOnAStableProject asserts the checks report nothing
// for a project whose committed artifacts carry none of the classes — the state
// every committing project holds after the one-time re-stamp.
func TestDoctorStabilityIsQuietOnAStableProject(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture"}`)
	write("app/putnami.json", declaringProjectConfig)
	write("app/README.md", "# app\n")
	write("app/schema/openapi.json", `{
  "openapi": "3.0.3",
  "info": { "title": "app", "version": "0.0.0" },
  "paths": {}
}`)
	write("app/schema/capabilities.json", `{
  "protocolVersion": 2,
  "project": "example/app",
  "packages": [
    {
      "identity": { "ownerProject": "example/app", "kind": "package", "key": "example/app" },
      "package": "example/app",
      "provenance": {
        "project": "example/app",
        "package": "example/app",
        "sourceKind": "framework",
        "declaration": { "root": "project", "path": "putnami.json" }
      }
    }
  ]
}`)

	report, err := DoctorRun(root, []*workspace.Project{proj("app")}, doctor.ProfileProduction, doctorTestClock)
	if err != nil || len(report.Findings) != 0 {
		t.Fatalf("stable project produced %#v (err %v)", report.Findings, err)
	}
}
