package runtimecli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// statusTreeCommit and statusTreeTree are one full commit and the full tree of
// its content, as control-api answers them.
const (
	statusTreeCommit = "abc1234def5678abc1234def5678abc1234def56"
	statusTreeTree   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// statusTreeSample holds one row per TREE rule: the run's commit is the row's
// commit and the move carried a tree; the last run was on another commit; the
// row has no commit; the move carried no tree; and no move opened the run.
const statusTreeSample = `{"workspace_id":"ws-acme","deployments":[
	{"project":"apps/same","environment":"prod","revision":"same-1","last_release_id":"cm_same","commit_sha":"` + statusTreeCommit + `","trigger":{"kind":"channel-move","namespace":"acme","channel":"prod","generation":3,"source_revision":"` + statusTreeCommit + `","source_tree":"` + statusTreeTree + `"}},
	{"project":"apps/other","environment":"prod","revision":"other-1","last_release_id":"cm_other","commit_sha":"` + statusTreeCommit + `","trigger":{"kind":"channel-move","namespace":"acme","channel":"prod","generation":4,"source_revision":"ffff1234def5678abc1234def5678abc1234def5","source_tree":"` + statusTreeTree + `"}},
	{"project":"apps/nocommit","environment":"prod","revision":"nocommit-1","last_release_id":"cm_nocommit","trigger":{"kind":"channel-move","namespace":"acme","channel":"prod","generation":5,"source_tree":"` + statusTreeTree + `"}},
	{"project":"apps/treeless","environment":"prod","revision":"treeless-1","last_release_id":"cm_treeless","commit_sha":"` + statusTreeCommit + `","trigger":{"kind":"channel-move","namespace":"acme","channel":"prod","generation":6,"source_revision":"` + statusTreeCommit + `"}},
	{"project":"apps/cli","environment":"prod","revision":"cli-1","last_release_id":"rel-cli","commit_sha":"` + statusTreeCommit + `"}
]}`

// TestStatusProvenanceTreeDescribesTheCommitBesideIt pins the TREE column end
// to end: control-api's source_tree decodes through the generated client, and
// the short tree renders only beside the commit it was recorded for. A tree
// printed next to another commit would state a false provenance.
func TestStatusProvenanceTreeDescribesTheCommitBesideIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusTreeSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{"provenance": true}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := strings.Join(cap.stdout, "\n")
	rows := statusRowsByProject(t, out)
	for project, want := range map[string]string{
		"apps/same":     statusTreeTree[:7],
		"apps/other":    "-",
		"apps/nocommit": "-",
		"apps/treeless": "-",
		"apps/cli":      "-",
	} {
		row := rows[project]
		if len(row) < 3 {
			t.Fatalf("%s row = %q, want PROJECT COMMIT TREE ...\n%s", project, row, out)
		}
		if row[2] != want {
			t.Errorf("%s TREE = %q, want %q\n%s", project, row[2], want, out)
		}
	}
	if strings.Contains(out, statusTreeTree) {
		t.Fatalf("the tree renders in full, want the 7-char short form:\n%s", out)
	}
}

// TestStatusStructuredOutputCarriesTheSourceTree pins that --output keeps the
// full tree on the trigger, so a script reads what the table abbreviates.
func TestStatusStructuredOutputCarriesTheSourceTree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusTreeSample))
	}))
	defer srv.Close()

	cap := &captureIO{}
	if err := runStatus(t, srv, cap, map[string]any{"output": "jsonl"}); err != nil {
		t.Fatalf("Status: %v", err)
	}
	var got deploymentsSummary
	if err := decodeResultData([]byte(strings.Join(cap.stdout, "\n")), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Deployments) != 5 || got.Deployments[0].Trigger == nil {
		t.Fatalf("deployments = %+v, want the five rows with a trigger first", got.Deployments)
	}
	if tree := got.Deployments[0].Trigger.SourceTree; tree != statusTreeTree {
		t.Fatalf("trigger source_tree = %q, want %q", tree, statusTreeTree)
	}
	if tree := got.Deployments[3].Trigger.SourceTree; tree != "" {
		t.Fatalf("treeless trigger source_tree = %q, want it absent", tree)
	}
}

// TestProvenanceTreeNeedsTheRunCommit covers the rule on the value shape.
func TestProvenanceTreeNeedsTheRunCommit(t *testing.T) {
	for name, tc := range map[string]struct {
		entry deploymentSummaryEntry
		want  string
	}{
		"no trigger": {deploymentSummaryEntry{CommitSHA: statusTreeCommit}, ""},
		"same commit": {deploymentSummaryEntry{CommitSHA: statusTreeCommit,
			Trigger: &deployTrigger{SourceRevision: statusTreeCommit, SourceTree: statusTreeTree}}, statusTreeTree[:7]},
		"same commit, padded": {deploymentSummaryEntry{CommitSHA: " " + statusTreeCommit,
			Trigger: &deployTrigger{SourceRevision: statusTreeCommit + "\n", SourceTree: statusTreeTree}}, statusTreeTree[:7]},
		"other commit": {deploymentSummaryEntry{CommitSHA: statusTreeCommit,
			Trigger: &deployTrigger{SourceRevision: strings.Repeat("f", 40), SourceTree: statusTreeTree}}, ""},
		"both commits empty": {deploymentSummaryEntry{
			Trigger: &deployTrigger{SourceTree: statusTreeTree}}, ""},
		"no tree": {deploymentSummaryEntry{CommitSHA: statusTreeCommit,
			Trigger: &deployTrigger{SourceRevision: statusTreeCommit}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := provenanceTree(tc.entry); got != tc.want {
				t.Fatalf("provenanceTree = %q, want %q", got, tc.want)
			}
		})
	}
}
