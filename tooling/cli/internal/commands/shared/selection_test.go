package shared

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// selectionWorkspace builds a workspace with the whole selection vocabulary in
// play: an alias, a group, a path scope, and tags that overlap the groups so a
// filter and a selector can disagree.
func selectionWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeSelectionFile(t, root, "putnami.workspace.json", `{
  "name": "selection",
  "includes": ["billing", "shipping", "warehouse"],
  "projectAliases": {"bill": "/billing"},
  "groups": {"commerce": "/billing,/shipping"}
}`)
	writeSelectionFile(t, root, "billing/putnami.json", `{"name":"@acme/billing","type":"application","tags":["service","public"]}`)
	writeSelectionFile(t, root, "shipping/putnami.json", `{"name":"@acme/shipping","type":"application","tags":["service"]}`)
	writeSelectionFile(t, root, "warehouse/putnami.json", `{"name":"@acme/warehouse","type":"application","tags":["internal"]}`)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func writeSelectionFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveProjectSelectionSpeaksTheWholeSelectionVocabulary pins that this
// read-only surface resolves identities, aliases, groups, scope expressions,
// tags, and exclusions the same way a job command does — because a second
// answer to "what does --projects mean" is the defect, not the feature.
func TestResolveProjectSelectionSpeaksTheWholeSelectionVocabulary(t *testing.T) {
	ws := selectionWorkspace(t)
	tests := []struct {
		name      string
		selection ProjectSelection
		want      []string
		mode      string
		scoped    bool
	}{
		{
			name:      "no flag is the whole workspace",
			selection: ProjectSelection{},
			want:      []string{"/billing", "/shipping", "/warehouse"},
			mode:      SelectionModeAll,
		},
		{
			name:      "--all is the explicit spelling of the same default",
			selection: ProjectSelection{All: true, Projects: "*"},
			want:      []string{"/billing", "/shipping", "/warehouse"},
			mode:      SelectionModeAll,
		},
		{
			name:      "--all beside a filter narrows",
			selection: ProjectSelection{All: true, Projects: "*", FilterTag: "public"},
			want:      []string{"/billing"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "project name",
			selection: ProjectSelection{Projects: "@acme/billing"},
			want:      []string{"/billing"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "project id",
			selection: ProjectSelection{Projects: "/shipping"},
			want:      []string{"/shipping"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "project alias",
			selection: ProjectSelection{Projects: "bill"},
			want:      []string{"/billing"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "group",
			selection: ProjectSelection{Projects: "commerce"},
			want:      []string{"/billing", "/shipping"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "comma union",
			selection: ProjectSelection{Projects: "/billing,/warehouse"},
			want:      []string{"/billing", "/warehouse"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "tag filter alone narrows",
			selection: ProjectSelection{FilterTag: "service"},
			want:      []string{"/billing", "/shipping"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "exclude-tag narrows",
			selection: ProjectSelection{ExcludeTag: "internal"},
			want:      []string{"/billing", "/shipping"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "exclude by name narrows",
			selection: ProjectSelection{Exclude: "@acme/warehouse"},
			want:      []string{"/billing", "/shipping"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
		{
			name:      "group intersected with a tag",
			selection: ProjectSelection{Projects: "commerce", FilterTag: "public"},
			want:      []string{"/billing"},
			mode:      SelectionModeProjects,
			scoped:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := ResolveProjectSelection(ws, test.selection)
			if err != nil {
				t.Fatalf("ResolveProjectSelection: %v", err)
			}
			if strings.Join(resolved.ProjectIDs, ",") != strings.Join(test.want, ",") {
				t.Fatalf("projects = %v, want %v", resolved.ProjectIDs, test.want)
			}
			if resolved.Mode != test.mode || resolved.Scoped != test.scoped {
				t.Fatalf("mode = %q scoped = %v, want %q / %v", resolved.Mode, resolved.Scoped, test.mode, test.scoped)
			}
			if len(resolved.Projects()) != len(resolved.ProjectIDs) {
				t.Fatalf("Projects() and ProjectIDs disagree: %d vs %d", len(resolved.Projects()), len(resolved.ProjectIDs))
			}
		})
	}
}

// TestResolveProjectSelectionOrdersDeterministically pins that the same flags
// over the same tree resolve to the same bytes, whatever order the workspace
// happens to hand its projects over in.
func TestResolveProjectSelectionOrdersDeterministically(t *testing.T) {
	ws := selectionWorkspace(t)
	forward, err := ResolveProjectSelection(ws, ProjectSelection{Projects: "/warehouse,/billing,/shipping"})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := ResolveProjectSelection(ws, ProjectSelection{Projects: "/shipping,/warehouse,/billing"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(forward.ProjectIDs, ",") != strings.Join(reversed.ProjectIDs, ",") {
		t.Fatalf("selection order leaked into the result: %v vs %v", forward.ProjectIDs, reversed.ProjectIDs)
	}
	if strings.Join(forward.ProjectIDs, ",") != "/billing,/shipping,/warehouse" {
		t.Fatalf("projects = %v, want sorted ids", forward.ProjectIDs)
	}
}

// TestResolveProjectSelectionIgnoresWorkspaceDisableTags pins a DELIBERATE
// difference from a job run. `disable.tags` says "do not build these here"; it
// does not say "these projects have no features". An inventory surface that
// applied it would answer a question nobody asked, and the omission would be
// invisible — so tag narrowing here happens only when the caller asks for it.
func TestResolveProjectSelectionIgnoresWorkspaceDisableTags(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, root, "putnami.workspace.json", `{
  "name": "selection",
  "includes": ["billing", "warehouse"],
  "disable": {"tags": ["internal"]}
}`)
	writeSelectionFile(t, root, "billing/putnami.json", `{"name":"@acme/billing","type":"application","tags":["service"]}`)
	writeSelectionFile(t, root, "warehouse/putnami.json", `{"name":"@acme/warehouse","type":"application","tags":["internal"]}`)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveProjectSelection(ws, ProjectSelection{All: true, Projects: "*"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(resolved.ProjectIDs, ",") != "/billing,/warehouse" {
		t.Fatalf("selection = %v, want the disabled-tag project still inventoried", resolved.ProjectIDs)
	}
}

// TestResolveProjectSelectionRejectsASelectorThatMatchesNothing pins the
// precedent `context map` set: the caller named something, so silence would be
// a wrong answer.
func TestResolveProjectSelectionRejectsASelectorThatMatchesNothing(t *testing.T) {
	ws := selectionWorkspace(t)
	for _, selection := range []ProjectSelection{
		{Projects: "@acme/nope"},
		{FilterTag: "no-project-carries-this"},
	} {
		_, err := ResolveProjectSelection(ws, selection)
		if !errors.Is(err, cmderr.ErrNotFound) {
			t.Fatalf("ResolveProjectSelection(%+v) error = %v, want not-found", selection, err)
		}
	}
}

// TestResolveProjectSelectionRefusesTwoWaysToSaySelection keeps the two
// selection sources from silently overriding one another.
func TestResolveProjectSelectionRefusesTwoWaysToSaySelection(t *testing.T) {
	ws := selectionWorkspace(t)
	_, err := ResolveProjectSelection(ws, ProjectSelection{Projects: "@acme/billing", Impacted: true})
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Fatalf("error = %v, want a usage refusal", err)
	}
}

// TestResolveProjectSelectionTreatsAnEmptyImpactAsAnExplicitNoOp pins the other
// `context map` precedent: nothing changed is an ANSWER, not a failure.
func TestResolveProjectSelectionTreatsAnEmptyImpactAsAnExplicitNoOp(t *testing.T) {
	ws := gitSelectionWorkspace(t)
	resolved, err := ResolveProjectSelection(ws, ProjectSelection{Impacted: true, Baseline: "HEAD"})
	if err != nil {
		t.Fatalf("ResolveProjectSelection: %v", err)
	}
	if !resolved.EmptyImpact || len(resolved.ProjectIDs) != 0 {
		t.Fatalf("resolved = %+v, want the explicit no-op", resolved)
	}
	if resolved.Mode != SelectionModeImpacted || !resolved.Scoped {
		t.Fatalf("resolved mode = %q scoped = %v", resolved.Mode, resolved.Scoped)
	}
	if resolved.Baseline == "" {
		t.Fatal("an impacted selection must report the ref it measured against")
	}
}

// TestResolveProjectSelectionReportsTheImpactedSetAndItsBaseline pins that the
// canonical impact resolver is what answers, baseline included.
func TestResolveProjectSelectionReportsTheImpactedSetAndItsBaseline(t *testing.T) {
	ws := gitSelectionWorkspace(t)
	writeSelectionFile(t, ws.Root, "billing/changed.txt", "changed")
	resolved, err := ResolveProjectSelection(ws, ProjectSelection{Impacted: true, Baseline: "HEAD"})
	if err != nil {
		t.Fatalf("ResolveProjectSelection: %v", err)
	}
	if strings.Join(resolved.ProjectIDs, ",") != "/billing" {
		t.Fatalf("impacted projects = %v, want the changed project", resolved.ProjectIDs)
	}
	if resolved.EmptyImpact {
		t.Fatal("a non-empty impact was reported as a no-op")
	}

	// Filters still apply to the impacted set, through the same canonical filter.
	filtered, err := ResolveProjectSelection(ws, ProjectSelection{Impacted: true, Baseline: "HEAD", ExcludeTag: "public"})
	if err != nil {
		t.Fatalf("ResolveProjectSelection: %v", err)
	}
	if !filtered.EmptyImpact || len(filtered.ProjectIDs) != 0 {
		t.Fatalf("filtered impact = %+v, want an explicit no-op rather than a failure", filtered)
	}
}

// gitSelectionWorkspace is selectionWorkspace inside a real git repository, so
// `--impacted` runs the canonical resolver rather than a stub.
func gitSelectionWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws := selectionWorkspace(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = ws.Root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "fixture")
	return ws
}

// TestResolveProjectSelectionTreatsParserSentinelsAsModes pins a trap the
// global flag parser sets: `--all` and `--impacted` write "*" and "[impacted]"
// into the SAME field an explicit `--projects` selector lands in. Reading that
// field raw makes `--impacted` look like "--impacted and --projects at once"
// and refuses a perfectly ordinary invocation.
func TestResolveProjectSelectionTreatsParserSentinelsAsModes(t *testing.T) {
	ws := gitSelectionWorkspace(t)
	resolved, err := ResolveProjectSelection(ws, ProjectSelection{
		Impacted: true, Projects: "[impacted]", Baseline: "HEAD",
	})
	if err != nil {
		t.Fatalf("--impacted as the parser spells it: %v", err)
	}
	if resolved.Mode != SelectionModeImpacted {
		t.Fatalf("mode = %q, want the impacted mode rather than a selector", resolved.Mode)
	}
	all, err := ResolveProjectSelection(ws, ProjectSelection{All: true, Projects: "*"})
	if err != nil {
		t.Fatalf("--all as the parser spells it: %v", err)
	}
	if all.Mode != SelectionModeAll || all.Scoped {
		t.Fatalf("--all resolved = %+v, want the unscoped default", all)
	}
}
