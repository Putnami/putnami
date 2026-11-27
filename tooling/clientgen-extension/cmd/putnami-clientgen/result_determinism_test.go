package main

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/clientgen/extension/internal/workspaceclient"
)

// TestTheGuardResultIsAFunctionOfTheTree pins that two runs of the guard on
// one tree return the same result bytes. The guard is keyed on `git:**`, so
// its result is what a cache hit replays and what a reader compares across
// runs: a wall time in it made every run's result differ from the last,
// although the verdict and the tree had not moved. The phase timings still
// exist, as metric events, and the report keeps them in memory for that.
func TestTheGuardResultIsAFunctionOfTheTree(t *testing.T) {
	root := t.TempDir()
	writeMainTestFile(t, root, "services/catalog/putnami.json", `{"name":"catalog"}`)
	members := []pctx.ProjectRef{{Name: "catalog", Path: "services/catalog"}}

	run := func() []byte {
		t.Helper()
		status, data, err := runCheck(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: members}, jsonl.NewForVersion(2), nil)
		if status != "OK" || err != nil {
			t.Fatalf("guard result = status %q err %v", status, err)
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	first, second := run(), run()
	if string(first) != string(second) {
		t.Fatalf("two runs of one tree returned different results:\n%s\n%s", first, second)
	}
	if strings.Contains(string(first), "timings") {
		t.Fatalf("the guard result carries timings: %s", first)
	}
	// Two runs of a tiny fixture can record the same wall time, so equal bytes
	// alone would not catch a clock field. The report's members are pinned
	// instead: a new member, whatever its name, is a decision this test makes
	// someone take about whether it is a function of the tree.
	var decoded struct {
		Report map[string]json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(decoded.Report))
	for name := range decoded.Report {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	wantFields := []string{"adaptationQueue", "externalContracts", "findings", "frameworkTransports", "mode", "pendingTransports", "protocolVersion", "providers"}
	if !slices.Equal(fields, wantFields) {
		t.Fatalf("the guard report's members = %v, want %v", fields, wantFields)
	}

	report := workspaceclient.InspectCommitted(root, memberPaths(members))
	if len(report.Timings) == 0 {
		t.Fatal("the report recorded no phase timing, so the guard has no metric to emit")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "timings") {
		t.Fatalf("the serialized report carries timings: %s", encoded)
	}
}
