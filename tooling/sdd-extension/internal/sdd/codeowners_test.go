package sdd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const codeownersSpec = "tooling/specification-driven-development"

func ownersOptions(owners ...string) map[string]map[string]any {
	list := make([]any, 0, len(owners))
	for _, owner := range owners {
		list = append(list, owner)
	}
	return map[string]map[string]any{"sdd": {"owners": list}}
}

// codeownersWorkspace is a scope `go` holding two projects, one of them nested
// in the other, plus a project outside the scope.
func codeownersWorkspace(t *testing.T) (string, *workspace.Workspace) {
	t.Helper()
	root := decisionsWorkspace(t)
	ws := fixtureWorkspace("w", root,
		appProject("api", "go/framework/api"),
		appProject("migration", "go/framework/migration"),
		appProject("migratecli", "go/framework/migration/migratecli"),
		appProject("site", "sites/web"),
	)
	return root, ws
}

func readCodeowners(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatalf("read CODEOWNERS: %v", err)
	}
	return string(data)
}

func TestCodeownersFileFollowsTheDeclarations(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "the-file-follows-the-declarations")
	root, ws := codeownersWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "go", "putnami.json"),
		`{"includes": ["framework/api"], "options": {"sdd": {"owners": ["@acme/go"]}}}`)
	// A byte order mark is not part of the document, and a symlinked
	// putnami.json is followed, as the CLI follows it. A host that cannot
	// create a symlink gets the file itself.
	migration := "\ufeff" + `{"name": "migration", "options": {"sdd": {"owners": ["@acme/data", "dba@acme.dev"]}}}`
	writeFixtureFile(t, filepath.Join(root, "go", "framework", "migration", "declared.json"), migration)
	if err := os.Symlink("declared.json", filepath.Join(root, "go", "framework", "migration", "putnami.json")); err != nil {
		writeFixtureFile(t, filepath.Join(root, "go", "framework", "migration", "putnami.json"), migration)
	}
	// A project that declares nothing gets no rule: the scope rule covers it.
	writeFixtureFile(t, filepath.Join(root, "go", "framework", "api", "putnami.json"), `{"name": "api"}`)
	writeFixtureFile(t, filepath.Join(root, featureproto.SpecsBaselineFilename), `{}`)

	report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err != nil {
		t.Fatalf("build: %v (%+v)", err, report.Diagnostics)
	}
	if !report.Adopted || !report.Written {
		t.Fatalf("report = %+v, want an adopted workspace and a written file", report)
	}
	want := codeownersHeader + `
* @lead

/go/ @acme/go
/go/framework/migration/ @acme/data dba@acme.dev

` + codeownersGovernanceComment + `**/specs.baseline.json @lead
**/specs/*.json @lead
**/putnami.features.json @lead
/putnami.workspace.json @lead
`
	if got := readCodeowners(t, root); got != want {
		t.Fatalf("CODEOWNERS =\n%s\nwant\n%s", got, want)
	}
	if len(report.Rules) != 7 || report.Rules[1].Source != "go/putnami.json" {
		t.Fatalf("rules = %+v, want 7 rules with the scope rule sourced from go/putnami.json", report.Rules)
	}
}

func TestCodeownersSingleMaintainerGetsOneRule(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "a-single-maintainer-gets-one-rule")
	root, ws := codeownersWorkspace(t)
	if _, err := BuildCodeownersResult(ws, ownersOptions("@solo")); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := readCodeowners(t, root), codeownersHeader+"\n* @solo\n"; got != want {
		t.Fatalf("CODEOWNERS =\n%s\nwant\n%s", got, want)
	}
}

func TestCodeownersUnchangedFileIsNotRewritten(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "an-unchanged-file-is-not-rewritten")
	root, ws := codeownersWorkspace(t)
	if report, err := BuildCodeownersResult(ws, ownersOptions("@solo")); err != nil || !report.Written {
		t.Fatalf("first run: written=%v err=%v, want a write", report.Written, err)
	}
	file := filepath.Join(root, ".github", "CODEOWNERS")
	before, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	report, err := BuildCodeownersResult(ws, ownersOptions("@solo"))
	if err != nil || report.Written {
		t.Fatalf("second run: written=%v err=%v, want no write", report.Written, err)
	}
	// The atomic write replaces the file, so a rewrite would change its identity.
	if after, err := os.Stat(file); err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the second run touched CODEOWNERS (stat err = %v)", err)
	}
	// A hand edit is drift: the next run puts the declared file back.
	writeFixtureFile(t, filepath.Join(root, ".github", "CODEOWNERS"), "* @someone-else\n")
	report, err = BuildCodeownersResult(ws, ownersOptions("@solo"))
	if err != nil || !report.Written {
		t.Fatalf("after a hand edit: written=%v err=%v, want a rewrite", report.Written, err)
	}
	if got, want := readCodeowners(t, root), codeownersHeader+"\n* @solo\n"; got != want {
		t.Fatalf("CODEOWNERS =\n%s\nwant\n%s", got, want)
	}
}

func TestCodeownersDeclarationWithoutCatchAllFails(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "a-declaration-without-a-catch-all-fails")
	root, ws := codeownersWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "sites", "web", "putnami.json"), `{"options": {"sdd": {"owners": ["@web"]}}}`)

	report, err := BuildCodeownersResult(ws, nil)
	if err == nil {
		t.Fatal("a directory declaration without a catch-all passed")
	}
	assertCodeownersDiagnostic(t, report, ErrorCodeCodeownersNoDefault, "putnami.workspace.json#options.sdd.owners")
	assertNoCodeowners(t, root)
}

func TestCodeownersInvalidOwnerFailsWithoutWriting(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "an-invalid-owner-fails-without-writing")
	for name, tc := range map[string]struct{ body, code string }{
		"not a handle":  {`{"options": {"sdd": {"owners": ["web-team"]}}}`, ErrorCodeInvalidOwners},
		"empty list":    {`{"options": {"sdd": {"owners": []}}}`, ErrorCodeInvalidOwners},
		"not an array":  {`{"options": {"sdd": {"owners": "@web"}}}`, ErrorCodeInvalidOwners},
		"duplicate":     {`{"options": {"sdd": {"owners": ["@web", "@WEB"]}}}`, ErrorCodeInvalidOwners},
		"sdd not block": {`{"options": {"sdd": ["@web"]}}`, ErrorCodeInvalidOwners},
		"malformed":     {`{"options": `, ErrorCodeCodeownersRead},
	} {
		t.Run(name, func(t *testing.T) {
			root, ws := codeownersWorkspace(t)
			writeFixtureFile(t, filepath.Join(root, "sites", "web", "putnami.json"), tc.body)
			report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
			if err == nil {
				t.Fatalf("an invalid declaration passed: %+v", report)
			}
			assertCodeownersDiagnostic(t, report, tc.code, "")
			assertNoCodeowners(t, root)
		})
	}
	// Without a catch-all, the workspace has not adopted CODEOWNERS yet. A file
	// that may hide a declaration still fails, or it would pass unseen.
	for name, tc := range map[string]struct{ body, code string }{
		"invalid owner": {`{"options": {"sdd": {"owners": "@web"}}}`, ErrorCodeInvalidOwners},
		"sdd not block": {`{"options": {"sdd": ["@web"]}}`, ErrorCodeInvalidOwners},
		"sdd a string":  {`{"options": {"sdd": "@web"}}`, ErrorCodeInvalidOwners},
		"malformed":     {`{"options": `, ErrorCodeCodeownersRead},
	} {
		t.Run(name+" without a catch-all", func(t *testing.T) {
			root, ws := codeownersWorkspace(t)
			writeFixtureFile(t, filepath.Join(root, "sites", "web", "putnami.json"), tc.body)
			report, err := BuildCodeownersResult(ws, nil)
			if err == nil {
				t.Fatalf("an invalid declaration passed without a catch-all: %+v", report)
			}
			assertCodeownersDiagnostic(t, report, tc.code, "")
			assertNoCodeowners(t, root)
		})
	}
	t.Run("owners in the root putnami.json", func(t *testing.T) {
		root, ws := codeownersWorkspace(t)
		writeFixtureFile(t, filepath.Join(root, "putnami.json"), `{"options": {"sdd": {"owners": ["@root"]}}}`)
		report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
		if err == nil {
			t.Fatalf("owners in the root putnami.json passed: %+v", report)
		}
		assertCodeownersDiagnostic(t, report, ErrorCodeInvalidOwners, "putnami.json#options.sdd.owners")
		assertNoCodeowners(t, root)
	})
	t.Run("invalid catch-all", func(t *testing.T) {
		root, ws := codeownersWorkspace(t)
		report, err := BuildCodeownersResult(ws, ownersOptions("@lead", "not an owner"))
		if err == nil {
			t.Fatalf("an invalid catch-all passed: %+v", report)
		}
		assertCodeownersDiagnostic(t, report, ErrorCodeInvalidOwners, "putnami.workspace.json#options.sdd.owners")
		assertNoCodeowners(t, root)
	})
}

func TestCodeownersNoDeclarationIsAdoption(t *testing.T) {
	spectest.Proves(t, codeownersSpec, "codeowners-follow-declared-owners", "no-declaration-is-adoption")
	root, ws := codeownersWorkspace(t)
	handWritten := "# written by a person\n* @someone\n"
	writeFixtureFile(t, filepath.Join(root, ".github", "CODEOWNERS"), handWritten)
	writeFixtureFile(t, filepath.Join(root, "go", "putnami.json"), `{"includes": ["framework/api"], "options": {"sdd": {}}}`)
	// A null sdd member declares nothing.
	writeFixtureFile(t, filepath.Join(root, "go", "framework", "api", "putnami.json"), `{"options": {"sdd": null}}`)

	report, err := BuildCodeownersResult(ws, map[string]map[string]any{"sdd": {"verification": map[string]any{}}})
	if err != nil {
		t.Fatalf("a workspace without owners failed: %v", err)
	}
	if report.Adopted || report.Written || len(report.Rules) != 0 {
		t.Fatalf("report = %+v, want nothing adopted or written", report)
	}
	if got := readCodeowners(t, root); got != handWritten {
		t.Fatalf("the hand-written CODEOWNERS changed to %q", got)
	}
}

func TestCodeownersRefusesADirectoryARuleCannotName(t *testing.T) {
	root := decisionsWorkspace(t)
	ws := fixtureWorkspace("w", root, appProject("odd", "apps/odd dir"))
	writeFixtureFile(t, filepath.Join(root, "apps", "odd dir", "putnami.json"), `{"options": {"sdd": {"owners": ["@odd"]}}}`)
	report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err == nil {
		t.Fatal("a directory with a space passed")
	}
	assertCodeownersDiagnostic(t, report, ErrorCodeCodeownersPath, "apps/odd dir/putnami.json")
}

func TestOwnerDirectoriesPutEveryParentFirst(t *testing.T) {
	_, ws := codeownersWorkspace(t)
	ws.Projects = append(ws.Projects, appProject("sibling", "go-tools/lint"), appProject("root", "."))
	got := ownerDirectories(ws)
	// "go-tools" sorts before "go" because '-' sorts before '/'. The order
	// between siblings does not matter; a parent before its children does.
	want := []string{"go-tools", "go-tools/lint", "go", "go/framework", "go/framework/api", "go/framework/migration",
		"go/framework/migration/migratecli", "sites", "sites/web"}
	if len(got) != len(want) {
		t.Fatalf("directories = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directories = %v, want %v", got, want)
		}
	}
}

func assertCodeownersDiagnostic(t *testing.T, report CodeownersReport, code, field string) {
	t.Helper()
	for _, finding := range report.Diagnostics {
		if finding.Code == code && finding.Severity == diag.Error && (field == "" || finding.Field == field) {
			return
		}
	}
	t.Fatalf("diagnostics = %+v, want an error %s on %q", report.Diagnostics, code, field)
}

// TestCodeownersGovernsAProjectSpecFloor: a floor committed in a project's
// directory adopts the governance rules as a workspace-root one does.
func TestCodeownersGovernsAProjectSpecFloor(t *testing.T) {
	root, ws := codeownersWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "sites", "web", featureproto.SpecsBaselineFilename), `{}`)

	report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err != nil {
		t.Fatalf("build: %v (%+v)", err, report.Diagnostics)
	}
	if len(report.Rules) != 1+len(specGovernancePatterns) {
		t.Fatalf("rules = %+v, want the catch-all and the governance rules", report.Rules)
	}
}

func assertNoCodeowners(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".github", "CODEOWNERS")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("CODEOWNERS exists after a failed run (stat err = %v)", err)
	}
}

func TestCodeownersGovernsASymlinkedSpecFloor(t *testing.T) {
	root, ws := codeownersWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "floor.json"), `{}`)
	if err := os.Symlink("floor.json", filepath.Join(root, featureproto.SpecsBaselineFilename)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	report, err := BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err != nil {
		t.Fatalf("build: %v (%+v)", err, report.Diagnostics)
	}
	// The ratchet follows the link, so the floor it enforces keeps its review.
	if len(report.Rules) != 1+len(specGovernancePatterns) {
		t.Fatalf("rules = %+v, want the catch-all and the governance rules", report.Rules)
	}

	if err := os.Remove(filepath.Join(root, featureproto.SpecsBaselineFilename)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, featureproto.SpecsBaselineFilename), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err = BuildCodeownersResult(ws, ownersOptions("@lead"))
	if err == nil {
		t.Fatalf("a directory named like the spec floor passed: %+v", report)
	}
	assertCodeownersDiagnostic(t, report, ErrorCodeCodeownersRead, featureproto.SpecsBaselineFilename)
}
