package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

const pageURLProposal = `"ref":{"source":"local:x","id":"P-1"},"revision":"r1","change":{"repository":"r","base":"main","head":"topic"},"title":"t","state":"open"`

// TestPageURL_SurvivesAReEncoding: a proposal answered with a pageUrl keeps
// it through a parse and a re-encoding, and one answered without it stays
// without it, on every operation that answers a proposal.
func TestPageURL_SurvivesAReEncoding(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-page-url", "a-proposal-keeps-its-page-url-through-a-re-encoding")
	const page = `"pageUrl":"https://console.example.com/acme/app/changes/7"`
	results := map[string]func(members string) string{
		OperationStatus: func(m string) string { return `{"proposal":{` + pageURLProposal + m + `},"checks":{"state":"none"}}` },
		OperationUpsert: func(m string) string { return `{"proposal":{` + pageURLProposal + m + `},"created":true}` },
		OperationFind:   func(m string) string { return `{"items":[{` + pageURLProposal + m + `}],"page":{}}` },
		OperationMerge:  func(m string) string { return `{"proposal":{` + pageURLProposal + m + `}}` },
	}
	for operation, result := range results {
		for name, members := range map[string]string{"with": "," + page, "without": ""} {
			t.Run(operation+" "+name, func(t *testing.T) {
				parsed, diags := ParseResult(ContractProposals, 1, operation, []byte(result(members)))
				if diags != nil {
					t.Fatalf("the result does not parse: %v", diags)
				}
				encoded, err := json.Marshal(parsed)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(string(encoded), page); got != (members != "") {
					t.Errorf("%s: pageUrl kept = %v, want %v", encoded, got, members != "")
				}
			})
		}
	}
}

// TestPageURL_InvalidIsRefused: a pageUrl that is not an absolute http or
// https link, or that carries credentials, is refused like any display link.
func TestPageURL_InvalidIsRefused(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-page-url", "an-invalid-page-url-is-refused")
	for name, value := range map[string]string{
		"relative":         "/changes/7",
		"no scheme":        "console.example.com/changes/7",
		"another scheme":   "ftp://console.example.com/changes/7",
		"with credentials": "https://user:secret@console.example.com/changes/7",
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseResult(ContractProposals, 1, OperationStatus,
				[]byte(`{"proposal":{`+pageURLProposal+`,"pageUrl":`+quoteJSON(t, value)+`},"checks":{"state":"none"}}`))
			if got := distinctCodes(diags); !slices.Equal(got, []string{ErrorCodeInvalidURL}) {
				t.Fatalf("codes = %v, want [%s]", got, ErrorCodeInvalidURL)
			}
		})
	}
}

// TestReadProviderOffer_DeclaredPageURL: a proposals provider offers pageUrl
// on the operations that answer a proposal, and the member on another
// operation withdraws the contract version.
func TestReadProviderOffer_DeclaredPageURL(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-page-url", "only-the-operations-that-answer-a-proposal-declare-page-url")
	declare := func(members map[string][]string) map[string]extension.ProviderDeclaration {
		proposals := fullProvider(ContractProposals)
		for operation, list := range members {
			declared, ok := proposals.Operations[operation]
			if !ok {
				declared = providerOperation(ContractProposals, operation)
			}
			declared.Members = list
			proposals.Operations[operation] = declared
		}
		return map[string]extension.ProviderDeclaration{ContractProposals: proposals}
	}
	page := []string{ProposalMemberPageURL}
	answering := []string{OperationAutoMerge, OperationFind, OperationMerge, OperationStatus, OperationUpsert}
	members := map[string][]string{}
	for _, operation := range answering {
		members[operation] = page
	}
	offer, diags := ReadProviderOffer(declare(members))
	if diag.HasErrors(diags) {
		t.Fatalf("declared pageUrl was refused: %v", diags)
	}
	ops, _ := offer.Operations(ContractProposals, 1)
	for _, operation := range answering {
		if got := ops[operation].Members(); !slices.Equal(got, page) {
			t.Errorf("%s offers members %v, want [pageUrl]", operation, got)
		}
	}
	if got := ops[OperationReview].Members(); got != nil {
		t.Errorf("review offers members %v, want none", got)
	}
	misplaced, _ := ReadProviderOffer(declare(map[string][]string{OperationReview: page}))
	if len(misplaced.Versions(ContractProposals)) != 0 {
		t.Fatal("pageUrl declared on proposals.review is still offered")
	}
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
