package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestProposalMembersKeepAbsentApartFromEmpty: an answer without labels or
// assignees says the provider does not report them, and an empty list says
// the proposal has none. The difference survives a parse and a re-encoding,
// which is what the orchestrator does to every result it forwards.
func TestProposalMembersKeepAbsentApartFromEmpty(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "absent-is-not-empty", "absent-and-empty-members-survive-a-re-encoding")
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

// TestProposalPageURLIsAnOptionalDisplayLink: pageUrl links a proposal's
// change in the provider's own console. A provider without one leaves it
// absent, a link survives a parse and a re-encoding, and a link that is not an
// absolute http or https URL, or that carries credentials, is refused like url.
func TestProposalPageURLIsAnOptionalDisplayLink(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-page-url", "page-url-is-an-optional-display-link")
	const proposal = `"ref":{"source":"local:x","id":"P-1"},"revision":"r1","url":"https://example.com/pull/1","change":{"repository":"r","base":"main","head":"topic"},"title":"t","state":"open"`
	parse := func(t *testing.T, members string) (*ProposalStatusResult, []string) {
		t.Helper()
		parsed, diags := ParseResult(ContractProposals, 1, OperationStatus,
			[]byte(`{"proposal":{`+proposal+members+`},"checks":{"state":"none"}}`))
		var codes []string
		for _, d := range diags {
			codes = append(codes, d.Code+"@"+d.Field)
		}
		result, _ := parsed.(*ProposalStatusResult)
		return result, codes
	}
	t.Run("absent", func(t *testing.T) {
		result, codes := parse(t, "")
		if codes != nil {
			t.Fatalf("the result does not parse: %v", codes)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"pageUrl"`) {
			t.Errorf("%s gained a pageUrl", encoded)
		}
	})
	t.Run("present", func(t *testing.T) {
		const link = "https://putnami.app/acme/app/changes/7"
		result, codes := parse(t, `,"pageUrl":"`+link+`"`)
		if codes != nil {
			t.Fatalf("the result does not parse: %v", codes)
		}
		if result.Proposal.PageURL != link || result.Proposal.URL != "https://example.com/pull/1" {
			t.Fatalf("pageUrl %q, url %q", result.Proposal.PageURL, result.Proposal.URL)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"pageUrl":"`+link+`"`) {
			t.Errorf("%s lost the pageUrl", encoded)
		}
	})
	for name, link := range map[string]string{
		"credentials":  "https://user:secret@putnami.app/changes/7",
		"relative":     "/changes/7",
		"other scheme": "ftp://putnami.app/changes/7",
	} {
		t.Run(name, func(t *testing.T) {
			_, codes := parse(t, `,"pageUrl":"`+link+`"`)
			if !slices.Equal(codes, []string{ErrorCodeInvalidURL + "@proposal.pageUrl"}) {
				t.Fatalf("codes = %v, want %s at proposal.pageUrl", codes, ErrorCodeInvalidURL)
			}
		})
	}
}
