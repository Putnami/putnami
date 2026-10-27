package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestACommentNamesItsReviewAndItsFileLine: a comment carries the review it
// belongs to and the path, line and commit of the file line it is anchored
// to, and keeps them through a parse and a re-encoding, which is what the
// orchestrator does to every result it forwards. A comment whose line is gone
// keeps its path, a comment on no file has no path, and a line never comes
// without the file it belongs to.
func TestACommentNamesItsReviewAndItsFileLine(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-comments", "a-comment-names-its-review-and-its-file-line")
	const ref = `"ref":{"source":"github:acme/app","id":"line-1"}`
	const review = `"review":{"source":"github:acme/app","id":"review-1"}`
	page := func(comment string) []byte { return []byte(`{"items":[{` + comment + `}],"page":{}}`) }
	reencoded := func(t *testing.T, comment string) (ProposalComment, string) {
		t.Helper()
		parsed, diags := ParseResult(ContractProposals, 1, OperationComments, page(comment))
		if diags != nil {
			t.Fatalf("the comment does not parse: %v", diags)
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.(*ProposalCommentListResult).Items[0], string(encoded)
	}

	anchored, encoded := reencoded(t, ref+","+review+`,"path":"src/a.go","line":12,"commit":"abcdef1","body":"Rename this."`)
	if anchored.Review == nil || *anchored.Review != (Ref{Source: "github:acme/app", ID: "review-1"}) ||
		anchored.Path != "src/a.go" || anchored.Line == nil || *anchored.Line != 12 || anchored.Commit != "abcdef1" {
		t.Fatalf("a line comment lost its review or its file line: %+v", anchored)
	}
	for _, member := range []string{review, `"path":"src/a.go"`, `"line":12`, `"commit":"abcdef1"`} {
		if !strings.Contains(encoded, member) {
			t.Errorf("%s lost %s", encoded, member)
		}
	}

	outdated, encoded := reencoded(t, ref+","+review+`,"path":"src/a.go","body":"This line moved."`)
	if outdated.Path != "src/a.go" || outdated.Line != nil || strings.Contains(encoded, `"line"`) {
		t.Errorf("a comment whose line is gone: %+v, %s", outdated, encoded)
	}
	conversation, encoded := reencoded(t, ref+`,"body":"Rebased."`)
	if conversation.Review != nil || conversation.Path != "" || strings.Contains(encoded, `"path"`) || strings.Contains(encoded, `"review"`) {
		t.Errorf("a conversation comment gained a review or a file: %+v, %s", conversation, encoded)
	}

	for name, tc := range map[string]struct {
		comment string
		code    string
	}{
		"a line without its file": {ref + `,"line":3,"body":"x"`, ErrorCodeRequired},
		"line zero":               {ref + `,"path":"a.go","line":0,"body":"x"`, ErrorCodeInvalidValue},
		"a path on two lines":     {ref + `,"path":"a\nb.go","body":"x"`, ErrorCodeInvalidValue},
		"a path above its bound":  {ref + `,"path":"` + strings.Repeat("a", MaxLocatorLength+1) + `","body":"x"`, ErrorCodeTooLarge},
		"no body":                 {ref + `,"body":" "`, ErrorCodeRequired},
		"a body above its bound":  {ref + `,"body":"` + strings.Repeat("b", MaxBodyBytes+1) + `"`, ErrorCodeTooLarge},
		"a malformed review":      {ref + `,"review":{"source":"acme","id":"1"},"body":"x"`, ErrorCodeInvalidReference},
		"a malformed commit":      {ref + `,"commit":"HEAD","body":"x"`, ErrorCodeInvalidValue},
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseResult(ContractProposals, 1, OperationComments, page(tc.comment))
			if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
				t.Fatalf("codes %v, want %s (%v)", got, tc.code, diags)
			}
		})
	}
	if _, diags := ParseRequest(ContractProposals, 1, OperationComments, []byte(`{"ref":{"source":"github:acme/app","id":"7"},"page":{"size":101}}`)); !slices.Equal(distinctCodes(diags), []string{ErrorCodeInvalidPage}) {
		t.Errorf("a comments page above 100: %v", diags)
	}
}
