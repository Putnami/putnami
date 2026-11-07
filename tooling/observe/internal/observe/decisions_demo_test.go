package observe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

const decisionsFeature = "tooling/observe-decisions"

// observeDecisionsDemo observes the decisions demo in the CI job of the go
// demo, with the decision directories named, nil for the default ones.
func observeDecisionsDemo(t *testing.T, dirs []string) Observation {
	t.Helper()
	root, base := demoRepository(t, "decisions")
	env := demos["go"].env
	opts := Options{
		Dir:       root,
		Env:       func(key string) string { return env[key] },
		ReadFile:  func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:   func(p string) string { return filepath.Join(root, p) },
		Producer:  Producer{Name: "@putnami/observe", Version: "1.0.0"},
		Decisions: dirs,
	}
	opts.Overrides.Base = base
	return Run(context.Background(), opts)
}

func TestTheMADRDemoIsExplained(t *testing.T) {
	spectest.Proves(t, decisionsFeature, "decisions-demo", "the-madr-demo-is-explained")
	spectest.Proves(t, decisionsFeature, "decisions-in-the-evidence", "touched-and-governing-decisions-are-listed")
	o := observeDecisionsDemo(t, nil)
	want := []evidence.Decision{
		{Path: "docs/decisions/0001-charges-are-synchronous.md", Title: "Charges are synchronous", Status: evidence.DecisionAccepted, Date: "2025-02-10", Change: evidence.FileDeleted},
		{Path: "docs/decisions/0002-payments-own-the-ledger.md", Title: "Payments own the ledger", Status: evidence.DecisionAccepted, Date: "2025-03-04", Scope: []string{"src/payments"}, Governs: []string{"src/payments/charge.go"}},
		{Path: "docs/decisions/0003-one-queue-per-domain.md", Title: "One queue per domain", Status: evidence.DecisionSuperseded, Scope: []string{"src/**/queue/*.go", "docs/queues.md"}, Governs: []string{"src/billing/queue/worker.go"}},
	}
	if !reflect.DeepEqual(o.Analysis.Decisions, want) {
		t.Errorf("decisions =\n%+v\nwant\n%+v", o.Analysis.Decisions, want)
	}
	doc := Evidence(o, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	if problems := evidence.ValidateDocument(doc); diag.HasErrors(problems) {
		t.Fatalf("the evidence document does not validate: %s", diag.ErrorText(problems))
	}
	if !reflect.DeepEqual(doc.Decisions, want) {
		t.Errorf("evidence decisions = %+v", doc.Decisions)
	}
	var text bytes.Buffer
	if err := WriteText(&text, o); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  decisions  3 decisions\n",
		"    accepted   docs/decisions/0001-charges-are-synchronous.md: Charges are synchronous (deleted by the change)\n",
		"    accepted   docs/decisions/0002-payments-own-the-ledger.md: Payments own the ledger (governs 1 changed file)\n",
		"    superseded docs/decisions/0003-one-queue-per-domain.md: One queue per domain (governs 1 changed file)\n",
		"  warning observe.decision_deleted: the change deletes the accepted decision docs/decisions/0001-charges-are-synchronous.md (Charges are synchronous)\n",
	} {
		if !strings.Contains(text.String(), line) {
			t.Errorf("the summary lacks %q:\n%s", line, text.String())
		}
	}
	if strings.Contains(text.String(), "README") {
		t.Errorf("the summary lists the README as a decision:\n%s", text.String())
	}
}

func TestDeletingAnAcceptedDecisionIsFlagged(t *testing.T) {
	spectest.Proves(t, decisionsFeature, "deleted-accepted-decision", "deleting-an-accepted-decision-is-flagged")
	o := observeDecisionsDemo(t, nil)
	var flagged []string
	for _, d := range o.Diagnostics {
		if d.Code == analyze.CodeDecisionDeleted {
			flagged = append(flagged, d.Field)
			if d.Severity != diag.Warning {
				t.Errorf("severity = %s, want warning", d.Severity)
			}
		}
	}
	if want := []string{"docs/decisions/0001-charges-are-synchronous.md"}; !reflect.DeepEqual(flagged, want) {
		t.Errorf("flagged = %v, want %v", flagged, want)
	}
	doc := Evidence(o, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	found := false
	for _, d := range doc.Diagnostics {
		found = found || d.Code == analyze.CodeDecisionDeleted
	}
	if !found {
		t.Error("the evidence document does not carry observe.decision_deleted")
	}
}

func TestTheSummaryListsTheDecisions(t *testing.T) {
	spectest.Proves(t, decisionsFeature, "decisions-summary", "the-summary-lists-the-decisions")
	var b strings.Builder
	writeDecisions(&b, []evidence.Decision{
		{Path: "adr/0004-retry.md", Title: "Retry", Status: evidence.DecisionProposed, Governs: []string{"a", "b"}, Change: evidence.FileAdded},
		{Path: "adr/0005-old.md", Title: "Old", Status: evidence.DecisionDeprecated, Change: evidence.FileRenamed},
	})
	want := "  decisions  2 decisions\n" +
		"    proposed   adr/0004-retry.md: Retry (governs 2 changed files, added by the change)\n" +
		"    deprecated adr/0005-old.md: Old (renamed by the change)\n"
	if b.String() != want {
		t.Errorf("summary =\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	writeDecisions(&b, nil)
	if b.String() != "" {
		t.Errorf("no decision writes %q, want nothing", b.String())
	}
}

func TestARepositoryWithoutDecisionsObservesAsBefore(t *testing.T) {
	spectest.Proves(t, decisionsFeature, "decisions-in-the-evidence", "a-repository-without-decisions-observes-as-before")
	// The go demo keeps no decision record: no decision, no decision
	// diagnostic, no decisions line and no decisions member.
	root, base := demoRepository(t, "go")
	env := demos["go"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(context.Background(), opts)
	if len(o.Analysis.Decisions) != 0 {
		t.Errorf("decisions = %+v, want none", o.Analysis.Decisions)
	}
	for _, d := range o.Diagnostics {
		if strings.HasPrefix(d.Code, "observe.decision") {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	var text bytes.Buffer
	if err := WriteText(&text, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "decisions") {
		t.Errorf("the summary has a decisions line:\n%s", text.String())
	}
	var data bytes.Buffer
	if err := WriteJSON(&data, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data.String(), `"decisions"`) {
		t.Error("the observation carries a decisions member")
	}
	// Named directories replace the default ones: the decisions demo read
	// from another directory lists no decision.
	if other := observeDecisionsDemo(t, []string{"docs/architecture"}); len(other.Analysis.Decisions) != 0 {
		t.Errorf("decisions under docs/architecture = %+v, want none", other.Analysis.Decisions)
	}
}
