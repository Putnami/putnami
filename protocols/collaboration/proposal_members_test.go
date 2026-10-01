package collaboration

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProposalMembersKeepAbsentApartFromEmpty: an answer without labels or
// assignees says the provider does not report them, and an empty list says
// the proposal has none. The difference survives a parse and a re-encoding,
// which is what the orchestrator does to every result it forwards.
func TestProposalMembersKeepAbsentApartFromEmpty(t *testing.T) {
	const proposal = `"ref":{"source":"local:x","id":"P-1"},"revision":"r1","change":{"repository":"r","base":"main","head":"topic"},"title":"t","state":"open"`
	for name, tc := range map[string]struct {
		members string
		want    []string
		absent  []string
	}{
		"not reported": {members: "", absent: []string{`"labels"`, `"assignees"`}},
		"none":         {members: `,"labels":[],"assignees":[]`, want: []string{`"labels":[]`, `"assignees":[]`}},
		"some":         {members: `,"labels":["group/cli"],"assignees":["octocat"]`, want: []string{`"labels":["group/cli"]`, `"assignees":["octocat"]`}},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, diags := ParseResult(ContractProposals, 1, OperationStatus,
				[]byte(`{"proposal":{`+proposal+tc.members+`},"checks":{"state":"none"}}`))
			if diags != nil {
				t.Fatalf("the result does not parse: %v", diags)
			}
			encoded, err := json.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range tc.want {
				if !strings.Contains(string(encoded), member) {
					t.Errorf("%s lost %s", encoded, member)
				}
			}
			for _, member := range tc.absent {
				if strings.Contains(string(encoded), member) {
					t.Errorf("%s gained %s", encoded, member)
				}
			}
		})
	}
}
