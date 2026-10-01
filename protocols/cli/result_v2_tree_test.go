package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The gated-tree fingerprint.
//
// These pin the three properties a consumer that TRUSTS a record depends on: a
// session file carrying the tree block round-trips without losing a member, a
// session written before the block existed still validates (additive by
// absence), and a fingerprint that is not a lowercase hex sha256 is rejected
// rather than compared as a string against one that is.

const treeFingerprintFixture = "6f1b2b0c2a9a7a1c9e5f3d4b8a7c6e5d4c3b2a1908f7e6d5c4b3a29180706f5e"

func sessionFileWithTree() SessionFile {
	return SessionFile{
		ProtocolVersion: ResultProtocolVersion,
		SessionID:       "20260913-101500-abc123",
		StartTime:       "2026-09-13T10:15:00.000Z",
		EndTime:         "2026-09-13T10:15:03.000Z",
		Commands:        []string{"lint", "test", "build", "validate"},
		Git:             &SessionGit{Branch: "fix/3616-session-tree-fingerprint"},
		Tree: &SessionTree{
			Fingerprint: treeFingerprintFixture,
			Dirty:       true,
			HeadSHA:     "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1",
		},
		Run: RunSummary{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{Total: 1, Succeeded: 1},
			DurationMs: 3000,
		},
	}
}

func TestSessionFileTreeRoundTrip(t *testing.T) {
	want := sessionFileWithTree()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SessionFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost a member:\n got: %+v\nwant: %+v", got.Tree, want.Tree)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionFileTreeIsOptional keeps the change ADDITIVE: every session already
// on disk was written without the block and must keep validating. A producer
// that could not determine the tree says nothing rather than claiming a clean
// one, so absence is the only spelling of "unknown".
func TestSessionFileTreeIsOptional(t *testing.T) {
	file := sessionFileWithTree()
	file.Tree = nil
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "\"tree\"") {
		t.Errorf("an absent tree still emits a member: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionFileTreeDirtyIsKnownWhenPresent pins the *bool decision recorded on
// SessionTree.Dirty: a present block always states dirtiness, so `false` must
// survive marshaling instead of being elided the way an omitempty member is.
func TestSessionFileTreeDirtyIsKnownWhenPresent(t *testing.T) {
	file := sessionFileWithTree()
	file.Tree.Dirty = false
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "\"dirty\":false") {
		t.Errorf("a clean tree dropped its dirty member: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

func TestSessionFileTreeRejectsMalformedMembers(t *testing.T) {
	cases := []struct {
		name  string
		tree  map[string]any
		wants []Violation
	}{
		{
			name:  "an abbreviated fingerprint",
			tree:  map[string]any{"fingerprint": treeFingerprintFixture[:12], "dirty": true, "headSHA": "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1"},
			wants: []Violation{{Code: ViolationInvalidValue, Path: "tree.fingerprint"}},
		},
		{
			name:  "an uppercase fingerprint",
			tree:  map[string]any{"fingerprint": strings.ToUpper(treeFingerprintFixture), "dirty": true, "headSHA": "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1"},
			wants: []Violation{{Code: ViolationInvalidValue, Path: "tree.fingerprint"}},
		},
		{
			name:  "a symbolic head",
			tree:  map[string]any{"fingerprint": treeFingerprintFixture, "dirty": false, "headSHA": "HEAD"},
			wants: []Violation{{Code: ViolationInvalidValue, Path: "tree.headSHA"}},
		},
		{
			name:  "a missing dirty verdict",
			tree:  map[string]any{"fingerprint": treeFingerprintFixture, "headSHA": "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1"},
			wants: []Violation{{Code: ViolationMissingField, Path: "tree.dirty"}},
		},
		{
			name: "an unknown member",
			tree: map[string]any{
				"fingerprint": treeFingerprintFixture, "dirty": false,
				"headSHA": "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1", "algorithm": "sha1",
			},
			wants: []Violation{{Code: ViolationUnknownField, Path: "tree.algorithm"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			document := map[string]any{}
			data, err := json.Marshal(sessionFileWithTree())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			document["tree"] = testCase.tree
			mutated, err := json.Marshal(document)
			if err != nil {
				t.Fatalf("marshal mutated: %v", err)
			}
			if got := ValidateDocument(DocumentSessionFile, mutated); !reflect.DeepEqual(got, testCase.wants) {
				t.Errorf("ValidateDocument = %v, want %v", got, testCase.wants)
			}
		})
	}
}
