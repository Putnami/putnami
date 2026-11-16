package collaboration

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const screenshotsTarget = `"ref":{"source":"github:acme/app","id":"7"},"commit":"0123456789abcdef0123456789abcdef01234567"`

// TestAScreenshotsRequestNamesItsCommitAndItsManifest: proposals.screenshots
// is an optional mutation whose request is the proposal, the commit captured
// and the path of the manifest file, on one line of at most MaxFilePathBytes.
func TestAScreenshotsRequestNamesItsCommitAndItsManifest(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-screenshots", "a-screenshots-request-names-its-commit-and-its-manifest")
	spec, _ := Lookup(ContractProposals, 1)
	operation, ok := spec.Operation(OperationScreenshots)
	if !ok || operation.Required || operation.Access != AccessMutating || !operation.Destructive {
		t.Fatalf("proposals.screenshots = %+v, want an optional destructive mutation", operation)
	}
	parsed, diags := ParseRequest(ContractProposals, 1, OperationScreenshots, []byte(`{`+screenshotsTarget+`,"manifest":".putnami/screenshots/apps-web/screenshots.json"}`))
	if len(diags) != 0 {
		t.Fatalf("a request with a ref, a commit and a manifest is refused: %v", diags)
	}
	if in := parsed.(*ProposalScreenshotsInput); in.Ref.ID != "7" || in.Manifest != ".putnami/screenshots/apps-web/screenshots.json" {
		t.Fatalf("request = %+v", in)
	}
	for _, tc := range []struct{ name, request, code string }{
		{"no manifest", `{` + screenshotsTarget + `}`, ErrorCodeRequired},
		{"a blank manifest", `{` + screenshotsTarget + `,"manifest":" "}`, ErrorCodeRequired},
		{"no commit", `{"ref":{"source":"github:acme/app","id":"7"},"manifest":"screenshots.json"}`, ErrorCodeRequired},
		{"a manifest of two lines", `{` + screenshotsTarget + `,"manifest":"a.json\nb.json"}`, ErrorCodeInvalidValue},
		{"a manifest above the bound", `{` + screenshotsTarget + `,"manifest":"` + strings.Repeat("a", MaxFilePathBytes+1) + `"}`, ErrorCodeTooLarge},
		{"an unknown member", `{` + screenshotsTarget + `,"manifest":"screenshots.json","files":[]}`, ErrorCodeUnknownField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseRequest(ContractProposals, 1, OperationScreenshots, []byte(tc.request))
			if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
				t.Fatalf("codes = %v, want %s", got, tc.code)
			}
		})
	}
}

// TestAScreenshotsResultCountsCapturesAndUploads: the result names the
// proposal, the commit and the project, and never reports more uploaded files
// than the manifest lists captures.
func TestAScreenshotsResultCountsCapturesAndUploads(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "proposal-screenshots", "a-screenshots-result-counts-captures-and-uploads")
	parsed, diags := ParseResult(ContractProposals, 1, OperationScreenshots, []byte(`{`+screenshotsTarget+`,"project":"apps/web","captures":8,"uploaded":0}`))
	if len(diags) != 0 {
		t.Fatalf("a result that uploaded nothing is refused: %v", diags)
	}
	if r := parsed.(*ProposalScreenshotsResult); r.Project != "apps/web" || r.Captures != 8 || r.Uploaded != 0 {
		t.Fatalf("result = %+v", r)
	}
	for _, tc := range []struct{ name, result, code string }{
		{"no project", `{` + screenshotsTarget + `,"project":"","captures":8,"uploaded":0}`, ErrorCodeRequired},
		{"no commit", `{"ref":{"source":"github:acme/app","id":"7"},"commit":"","project":"apps/web","captures":8,"uploaded":0}`, ErrorCodeRequired},
		{"negative captures", `{` + screenshotsTarget + `,"project":"apps/web","captures":-1,"uploaded":0}`, ErrorCodeInvalidValue},
		{"more uploads than captures", `{` + screenshotsTarget + `,"project":"apps/web","captures":8,"uploaded":9}`, ErrorCodeInvalidValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseResult(ContractProposals, 1, OperationScreenshots, []byte(tc.result))
			if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
				t.Fatalf("codes = %v, want %s", got, tc.code)
			}
		})
	}
}
