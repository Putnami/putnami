package store

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	ci "go.putnami.dev/protocol/ci"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The workspace protocol states the verdict policy file name without
// depending on the CI protocol that defines the file, so this test keeps the
// two names equal.
func TestVerdictPolicyFileIsTheCIProtocolName(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-verdict-policy-is-no-build-input",
		"the-workspace-protocol-names-the-ci-verdict-policy-file")
	if wsproto.VerdictPolicyFile != ci.VerdictPolicyFile {
		t.Fatalf("workspace protocol names %q, CI protocol names %q", wsproto.VerdictPolicyFile, ci.VerdictPolicyFile)
	}
	if !wsproto.IsNamedOnlyInput(ci.VerdictPolicyFile) {
		t.Fatalf("%s is read as a build input", ci.VerdictPolicyFile)
	}
}

// A putnami.verdict-policy.json file is no build input. Adding, editing or
// removing one leaves the key of every pattern set that does not name it as
// it was, the empty set and `git:**` included. A pattern whose last segment
// names the file keys it.
func TestCacheKey_AVerdictPolicyMovesOnlyAKeyWhosePatternNamesIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-verdict-policy-is-no-build-input",
		"a-pattern-that-does-not-name-a-verdict-policy-leaves-it-out-of-the-key")
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "main.go", "package main\n")
	gitInputWrite(t, root, "settings.json", "{}\n")
	gitInputWrite(t, root, "config/settings.json", "{}\n")
	gitInputRun(t, root, "add", "-A")
	blind := [][]string{
		nil, {"**"}, {"**/*.json"}, {"*.json"}, {"config/**"}, {"git:**"}, {"git:**/*.json"},
		{"**/*.json", "!**/*.go"},
	}
	naming := [][]string{
		{"**/putnami.verdict-policy.json"}, {"putnami.verdict-policy.json"},
		{"config/putnami.verdict-policy.json"}, {"git:**/putnami.verdict-policy.json"},
	}
	keys := func(sets [][]string) []string {
		t.Helper()
		out := make([]string, len(sets))
		for i, patterns := range sets {
			out[i] = gitInputKey(t, root, patterns...)
		}
		return out
	}
	policies := []string{"putnami.verdict-policy.json", "config/putnami.verdict-policy.json"}
	writePolicies := func(content string) {
		for _, policy := range policies {
			gitInputWrite(t, root, policy, content)
		}
	}

	blindKeys, namingKeys := keys(blind), keys(naming)
	for _, step := range []struct {
		name  string
		apply func()
		// changes says whether the step changes the policy files, which a
		// naming key must then follow. Staging changes no file.
		changes bool
	}{
		{"a verdict policy is added", func() { writePolicies(`{"version":1}`) }, true},
		{"the verdict policy is staged", func() { gitInputRun(t, root, "add", "-A") }, false},
		{"the verdict policy is edited", func() { writePolicies(`{"version":2}`) }, true},
		{"the verdict policy is removed", func() {
			for _, policy := range policies {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(policy))); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
	} {
		step.apply()
		blindAfter, namingAfter := keys(blind), keys(naming)
		for i, patterns := range blind {
			if blindAfter[i] != blindKeys[i] {
				t.Errorf("%s: the key of %q moved", step.name, patterns)
			}
		}
		for i, patterns := range naming {
			if step.changes && namingAfter[i] == namingKeys[i] {
				t.Errorf("%s: the key of %q did not move", step.name, patterns)
			}
		}
		namingKeys = namingAfter
	}

	// The same patterns key every other file they select, so the cases above
	// prove a rule and not a pattern that selects nothing.
	gitInputWrite(t, root, "settings.json", `{"changed":true}`)
	gitInputWrite(t, root, "config/settings.json", `{"changed":true}`)
	for i, key := range keys(blind) {
		if key == blindKeys[i] {
			t.Errorf("an edit of the files %q selects left its key", blind[i])
		}
	}
}

// The batch guard, the files a key lists and the generate assets a key folds
// in leave a verdict policy out the same way: a directory or a pattern that
// does not name it selects every other file and not that one.
func TestKeyFiles_AVerdictPolicyNeedsAPatternThatNamesIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-verdict-policy-is-no-build-input",
		"a-pattern-that-does-not-name-a-verdict-policy-leaves-it-out-of-the-key")
	dir := t.TempDir()
	policy := filepath.Join(dir, "doc", "putnami.verdict-policy.json")
	gitInputWrite(t, dir, "doc/putnami.verdict-policy.json", "{}\n")

	for _, tc := range []struct {
		patterns []string
		want     bool
	}{
		{patterns: nil, want: false},
		{patterns: []string{"**/*.json"}, want: false},
		{patterns: []string{"doc/**"}, want: false},
		{patterns: []string{"**/putnami.verdict-policy.json"}, want: true},
	} {
		if got, err := HasMatchingFiles(dir, tc.patterns, nil); err != nil || got != tc.want {
			t.Errorf("HasMatchingFiles(%q) = %v, %v, want %v", tc.patterns, got, err, tc.want)
		}
	}

	gitInputWrite(t, dir, "doc/guide.md", "# Guide\n")
	for _, tc := range []struct {
		patterns []string
		want     string
	}{
		{patterns: nil, want: "guide.md"},
		{patterns: []string{"**"}, want: "guide.md"},
		{patterns: []string{"doc/putnami.verdict-policy.json"}, want: "putnami.verdict-policy.json"},
	} {
		files, err := CollectKeyFiles(dir, tc.patterns, nil)
		if err != nil || len(files) != 1 || filepath.Base(files[0]) != tc.want {
			t.Errorf("CollectKeyFiles(%q) = %v, %v, want %s alone", tc.patterns, files, err, tc.want)
		}
	}

	asset := filepath.Join(dir, "doc")
	if files := ExtraKeyFiles([]string{asset}); slices.Contains(files, policy) || len(files) != 1 {
		t.Errorf("ExtraKeyFiles(doc) = %v, want doc/guide.md alone", files)
	}
	if files := ExtraKeyFiles([]string{policy}); !slices.Equal(files, []string{policy}) {
		t.Errorf("ExtraKeyFiles(the policy) = %v, want the policy", files)
	}
	before, named := hashExtraFiles("", []string{asset}), hashExtraFiles("", []string{policy})
	gitInputWrite(t, dir, "doc/putnami.verdict-policy.json", `{"version":2}`)
	if after := hashExtraFiles("", []string{asset}); after != before {
		t.Error("an edit of a verdict policy moved the hash of the directory asset that holds it")
	}
	if after := hashExtraFiles("", []string{policy}); after == named {
		t.Error("an edit of a verdict policy left the hash of the asset that names it")
	}
}
