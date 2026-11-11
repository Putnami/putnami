package sdd

import (
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	// rootVerdictPolicy sets a key that only the root file may set, and a map
	// by maturity on a key that concerns a feature.
	rootVerdictPolicy = `{
  "$schema": "https://putnami.dev/schemas/putnami-verdict-policy.json",
  "declarations": {"adr": "never", "spec": {"modeled": "never", "ga": "always"}},
  "gaps": {"unknown-impact": "block", "unproven-requirement": {"modeled": "inform", "coded": "accept"}}
}`
	// nestedVerdictPolicy is valid in a scope or a project directory.
	nestedVerdictPolicy = `{"gaps": {"no-requirement": "inform", "lost-proof": {"modeled": "accept", "ga": "block"}}}`
)

// verdictPolicyWorkspace writes a scope directory, go, which holds the
// project go/lib, and returns the workspace with that project as its member.
func verdictPolicyWorkspace(t *testing.T, root string) *workspace.Workspace {
	t.Helper()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"w"}`)
	writeFixtureFile(t, filepath.Join(root, "go", "putnami.json"), `{"tags":["go"]}`)
	writeFixtureFile(t, filepath.Join(root, "go", "lib", "putnami.json"), `{"name":"lib"}`)
	return fixtureWorkspace("w", root, appProject("lib", "go/lib"))
}

func writeVerdictPolicy(t *testing.T, root, dir, contents string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(dir), "putnami.verdict-policy.json"), contents)
}

// TestVerdictPolicyAbsentFileIsAdoption keeps a workspace without a policy
// file green: the CI plane then applies its defaults.
func TestVerdictPolicyAbsentFileIsAdoption(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "verdict-policies-are-checked", "no-policy-is-adoption")
	root := decisionsWorkspace(t)
	report, err := BuildVerdictPolicyResult(verdictPolicyWorkspace(t, root))
	if err != nil {
		t.Fatalf("a workspace without a policy file failed the task: %v (%+v)", err, report.Diagnostics)
	}
	if len(report.Policies) != 0 || len(report.Diagnostics) != 0 {
		t.Fatalf("report = %+v, want nothing read", report)
	}
}

// TestVerdictPolicyValidFilesPass reads a valid file at the root, in a scope
// directory and in a project directory.
func TestVerdictPolicyValidFilesPass(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "verdict-policies-are-checked", "a-valid-policy-passes")
	root := decisionsWorkspace(t)
	ws := verdictPolicyWorkspace(t, root)
	writeVerdictPolicy(t, root, "", rootVerdictPolicy)
	writeVerdictPolicy(t, root, "go", nestedVerdictPolicy)
	writeVerdictPolicy(t, root, "go/lib", nestedVerdictPolicy)

	report, err := BuildVerdictPolicyResult(ws)
	if err != nil {
		t.Fatalf("valid policy files failed the task: %v (%+v)", err, report.Diagnostics)
	}
	want := []string{"go/lib/putnami.verdict-policy.json", "go/putnami.verdict-policy.json", "putnami.verdict-policy.json"}
	if !reflect.DeepEqual(report.Policies, want) || len(report.Diagnostics) != 0 {
		t.Fatalf("report = %+v, want the three files read and no finding", report)
	}
}

// TestVerdictPolicyContentDefectFailsWithTheProtocolCode keeps the ci
// protocol's code and names the file and the member at fault.
func TestVerdictPolicyContentDefectFailsWithTheProtocolCode(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "verdict-policies-are-checked", "a-content-defect-fails-with-the-protocol-code")
	const file = "go/lib/putnami.verdict-policy.json"
	for name, tc := range map[string]struct {
		contents string
		code     string
		field    string
	}{
		"unknown declaration kind":              {`{"declarations":{"readme":"never"}}`, "ci.verdict_policy_kind", file + "#declarations.readme"},
		"the policy kind":                       {`{"declarations":{"policy":"never"}}`, "ci.verdict_policy_kind", file + "#declarations.policy"},
		"unknown gap kind":                      {`{"gaps":{"flaky-test":"inform"}}`, "ci.verdict_policy_kind", file + "#gaps.flaky-test"},
		"unknown value":                         {`{"gaps":{"no-requirement":"ignore"}}`, "ci.verdict_policy_value", file + "#gaps.no-requirement"},
		"unknown key":                           {`{"gaps":{},"mode":"report"}`, "ci.invalid_json", file},
		"unknown maturity":                      {`{"gaps":{"unproven-requirement":{"modeled":"inform","beta":"block"}}}`, "ci.verdict_policy_maturity", file + "#gaps.unproven-requirement.beta"},
		"map without modeled":                   {`{"gaps":{"unproven-requirement":{"coded":"block"}}}`, "ci.verdict_policy_maturity", file + "#gaps.unproven-requirement"},
		"map on a key that concerns no feature": {`{"gaps":{"no-requirement":{"modeled":"inform"}}}`, "ci.verdict_policy_maturity", file + "#gaps.no-requirement"},
		"root key outside the root":             {`{"gaps":{"unknown-impact":"block"}}`, "ci.verdict_policy_root_only", file + "#gaps.unknown-impact"},
		"unknown schema":                        {`{"$schema":"https://example.com/policy.json"}`, "ci.verdict_policy_schema", file + "#$schema"},
	} {
		t.Run(name, func(t *testing.T) {
			root := decisionsWorkspace(t)
			ws := verdictPolicyWorkspace(t, root)
			writeVerdictPolicy(t, root, "go/lib", tc.contents)

			report, err := BuildVerdictPolicyResult(ws)
			if err == nil {
				t.Fatalf("a policy file with a defect passed: %+v", report)
			}
			if len(report.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
			}
			if finding := report.Diagnostics[0]; finding.Code != tc.code || finding.Field != tc.field {
				t.Fatalf("finding = %+v, want %s on %s", finding, tc.code, tc.field)
			}
		})
	}
}

// TestVerdictPolicyOutsideTheRootAScopeOrAProjectFails refuses a file in any
// other directory, with one code, and still checks its content.
func TestVerdictPolicyOutsideTheRootAScopeOrAProjectFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "verdict-policies-are-checked", "a-policy-outside-the-root-a-scope-or-a-project-fails")
	for name, dir := range map[string]string{
		"a plain directory":                       "docs",
		"a directory inside a project":            "go/lib/src",
		"a directory above no project":            "tools",
		"a hidden directory":                      ".github",
		"a directory whose putnami.json is plain": "python",
	} {
		t.Run(name, func(t *testing.T) {
			root := decisionsWorkspace(t)
			ws := verdictPolicyWorkspace(t, root)
			// tools holds a scope manifest, but no project lives below it.
			writeFixtureFile(t, filepath.Join(root, "tools", "putnami.json"), `{"tags":["tools"]}`)
			// python holds a project, but its putnami.json declares no scope.
			writeFixtureFile(t, filepath.Join(root, "python", "putnami.json"), `{"options":{}}`)
			ws = fixtureWorkspace("w", root, append(ws.Projects, appProject("svc", "python/svc"))...)
			writeVerdictPolicy(t, root, dir, nestedVerdictPolicy)

			report, err := BuildVerdictPolicyResult(ws)
			if err == nil {
				t.Fatalf("a policy file in %s passed: %+v", dir, report)
			}
			want := dir + "/putnami.verdict-policy.json"
			if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != ErrorCodeVerdictPolicyLocation ||
				report.Diagnostics[0].Field != want {
				t.Fatalf("diagnostics = %+v, want one %s on %s", report.Diagnostics, ErrorCodeVerdictPolicyLocation, want)
			}
		})
	}

	root := decisionsWorkspace(t)
	ws := verdictPolicyWorkspace(t, root)
	writeVerdictPolicy(t, root, "docs", `{"gaps":{"no-requirement":"ignore"}}`)
	report, _ := BuildVerdictPolicyResult(ws)
	codes := map[string]bool{}
	for _, finding := range report.Diagnostics {
		codes[finding.Code] = true
	}
	if !codes[ErrorCodeVerdictPolicyLocation] || !codes["ci.verdict_policy_value"] {
		t.Fatalf("diagnostics = %+v, want the place and the content of a misplaced file both refused", report.Diagnostics)
	}
}

// TestVerdictPolicyReadsOnlyTheCandidateCut lists the policy files of a Git
// work tree from its candidate cut: an ignored file, and a file under a
// directory the cache key's file walk skips, are never read.
func TestVerdictPolicyReadsOnlyTheCandidateCut(t *testing.T) {
	root := candidateWorkspace(t)
	ws := verdictPolicyWorkspace(t, root)
	writeFixtureFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	writeVerdictPolicy(t, root, "go/lib", nestedVerdictPolicy)
	for _, dir := range []string{"ignored", "go/lib/.gen", "node_modules/pkg", "dist", "go/lib/out", "vendor/x"} {
		writeVerdictPolicy(t, root, dir, `{"gaps":{"no-requirement":"ignore"}}`)
	}

	report, err := BuildVerdictPolicyResult(ws)
	if err != nil {
		t.Fatalf("the candidate cut holds one valid file, yet the task failed: %v (%+v)", err, report.Diagnostics)
	}
	if want := []string{"go/lib/putnami.verdict-policy.json"}; !reflect.DeepEqual(report.Policies, want) {
		t.Fatalf("policies = %v, want %v", report.Policies, want)
	}
}

// TestVerdictPolicyUnreadableFileFailsClosed refuses a symbolic link named
// like a policy file, inside and outside a Git work tree, instead of skipping
// it.
func TestVerdictPolicyUnreadableFileFailsClosed(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "verdict-policies-are-checked", "an-unreadable-policy-fails-closed")
	for name, open := range map[string]func(t *testing.T) string{
		"in a Git work tree":      candidateWorkspace,
		"outside a Git work tree": decisionsWorkspace,
	} {
		t.Run(name, func(t *testing.T) {
			root := open(t)
			ws := verdictPolicyWorkspace(t, root)
			writeFixtureFile(t, filepath.Join(root, "policy.json"), nestedVerdictPolicy)
			symlinkIn(t, root, "../../policy.json", "go/lib/putnami.verdict-policy.json")

			report, err := BuildVerdictPolicyResult(ws)
			if err == nil {
				t.Fatalf("a symbolic link named like a policy file passed: %+v", report)
			}
			want := "go/lib/putnami.verdict-policy.json"
			if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != ErrorCodeVerdictPolicyUnreadable ||
				report.Diagnostics[0].Field != want {
				t.Fatalf("diagnostics = %+v, want one %s on %s", report.Diagnostics, ErrorCodeVerdictPolicyUnreadable, want)
			}
		})
	}
}
