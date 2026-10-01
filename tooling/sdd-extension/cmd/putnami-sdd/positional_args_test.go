package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A subcommand-local flag must never be counted as a positional.
//
// The CLI forwards every token it did not consume, and it rewrites `--owner=x`
// into `--owner` `x` on the way. Before this was a set, `architecture init
// billing --owner billing-team` counted `billing-team` as a second positional
// and the arity check rejected a call the manifest documents as an example.
func TestPositionalArgsSkipsValueTakingSubcommandFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"bare", []string{"billing"}, []string{"billing"}},
		{"separate value", []string{"billing", "--owner", "billing-team"}, []string{"billing"}},
		{"inline value", []string{"billing", "--owner=billing-team"}, []string{"billing"}},
		{"two value flags", []string{"billing", "--owner", "team", "--at", "billing/annex"}, []string{"billing"}},
		{"boolean keeps its neighbor", []string{"--dry-run", "billing"}, []string{"billing"}},
		{"value flag before positional", []string{"--project", "/a", "billing"}, []string{"billing"}},
		{"trailing value flag has no value", []string{"billing", "--session"}, []string{"billing"}},
		{"unknown flag consumes nothing", []string{"billing", "--verbose", "extra"}, []string{"billing", "extra"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := positionalArgs(testCase.args)
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Errorf("positionalArgs(%v) = %v, want %v", testCase.args, got, testCase.want)
			}
		})
	}
}

// The set is only correct while it matches the manifest. A new string flag on any
// subcommand breaks a documented invocation silently, so the manifest is the
// oracle and this test is the join.
func TestEveryValueTakingSubcommandFlagIsKnown(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json")) //nolint:gosec // the committed manifest of this extension
	if err != nil {
		t.Fatalf("read the extension manifest: %v", err)
	}
	var manifest struct {
		CommandGroups map[string]struct {
			Subcommands map[string]struct {
				Flags map[string]struct {
					Type string `json:"type"`
				} `json:"flags"`
			} `json:"subcommands"`
		} `json:"commandGroups"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse the extension manifest: %v", err)
	}

	declared := map[string]bool{}
	for _, group := range manifest.CommandGroups {
		for _, subcommand := range group.Subcommands {
			for name, flag := range subcommand.Flags {
				if flag.Type == "boolean" {
					continue
				}
				declared["--"+name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the manifest declares no value-taking subcommand flag; this assertion would pass vacuously")
	}

	var missing, stale []string
	for name := range declared {
		if !subcommandValueFlags[name] {
			missing = append(missing, name)
		}
	}
	for name := range subcommandValueFlags {
		if !declared[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("these value-taking subcommand flags are not in subcommandValueFlags, so their value is counted as a positional: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("subcommandValueFlags names flags no subcommand declares: %v", stale)
	}
}
