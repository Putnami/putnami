package finalize

import (
	"context"
	"encoding/json"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const testChangePage = "https://putnami.app/acme/app/changes/7"

// TestTheResultCarriesTheChangePage: a draft and a ready run carry the
// proposal's pageUrl as changeUrl when the provider answers one, and leave it
// absent otherwise.
func TestTheResultCarriesTheChangePage(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "finalize-prints-the-change-page-after-the-proposal")
	for _, tc := range []struct {
		name  string
		draft bool
		page  string
	}{
		{name: "ready", page: testChangePage},
		{name: "draft", draft: true, page: testChangePage},
		{name: "ready without a page"},
		{name: "draft without a page", draft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, git, provider, opts := finalizerFixture(t)
			if tc.draft {
				opts.Draft, opts.VerificationFile, opts.TaskSource, opts.TaskID = true, "", "", ""
				git.ahead = false
			}
			if tc.page != "" {
				provider.statusHook = func(proposal *collab.Proposal) { proposal.PageURL = tc.page }
			}
			result, err := f.Run(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.ChangeURL != tc.page {
				t.Fatalf("changeUrl %q, want %q", result.ChangeURL, tc.page)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var members map[string]any
			if err := json.Unmarshal(encoded, &members); err != nil {
				t.Fatal(err)
			}
			if got, present := members["changeUrl"]; present != (tc.page != "") || (present && got != tc.page) {
				t.Fatalf("result %s, want changeUrl %q", encoded, tc.page)
			}
		})
	}
}

// TestTheCommandPrintsTheChangePageAfterTheProposal: the text output prints
// CHANGE_URL right after PROPOSAL_URL when the provider answers a pageUrl, and
// the JSON output carries it as changeUrl.
func TestTheCommandPrintsTheChangePageAfterTheProposal(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "finalize-prints-the-change-page-after-the-proposal")
	const proposalURL = "https://example.invalid/proposals/pr"
	for _, output := range []string{"text", "json"} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			contextFile := writeContext(t, map[string]any{"output": output}, map[string]any{"mode": "all", "scoped": false, "projects": []string{}})
			command, h, stdout, stderr, args := draftCommand(t, contextFile)
			h.provider.statusHook = func(proposal *collab.Proposal) {
				proposal.URL, proposal.PageURL = proposalURL, testChangePage
			}
			if code := command.Run(context.Background(), args); code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if output == "json" {
				var result Result
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.ChangeURL != testChangePage || result.ProposalURL != proposalURL {
					t.Fatalf("stdout %q, want changeUrl %q", stdout, testChangePage)
				}
				return
			}
			want := `PROPOSAL_REF={"source":"local:proposal","id":"pr"}` + "\nPROPOSAL_URL=" + proposalURL + "\nCHANGE_URL=" + testChangePage + "\n"
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q", stdout, want)
			}
		})
	}
}

// TestTheHostedGateTakesTheChangePageOfTheFinalRead: a provider that answers
// the pageUrl only once the hosted checks run still gives the ready run its
// change page.
func TestTheHostedGateTakesTheChangePageOfTheFinalRead(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "finalize-prints-the-change-page-after-the-proposal")
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile = "ci", ""
	provider.checks = collab.Checks{Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	provider.statusHook = func(proposal *collab.Proposal) {
		if provider.statusReads > 1 {
			proposal.PageURL = testChangePage
		}
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.ChangeURL != testChangePage {
		t.Fatalf("changeUrl %q, want %q; err=%v", result.ChangeURL, testChangePage, err)
	}
}

// TestASettledProposalReportsItsChangePage: a branch whose proposal already
// merged reports its change page after its URL, before its state.
func TestASettledProposalReportsItsChangePage(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "finalize-prints-the-change-page-after-the-proposal")
	contextFile := writeContext(t, map[string]any{"output": "text"}, map[string]any{"mode": "all", "scoped": false, "projects": []string{}})
	command, h, stdout, _, args := draftCommand(t, contextFile)
	merged := settledProposal(collab.ProposalStateMerged, "7", "0123456789012345678901234567890123456789")
	merged.PageURL = testChangePage
	h.provider.settled = []collab.Proposal{merged}
	if code := command.Run(context.Background(), args); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := `PROPOSAL_REF={"source":"github:acme/app","id":"7"}` + "\nPROPOSAL_URL=https://github.com/acme/app/pull/7\nCHANGE_URL=" + testChangePage + "\nPROPOSAL_STATE=merged\n"
	if stdout.String() != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
}
