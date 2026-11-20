package finalize

import (
	"context"
	"encoding/json"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const changePage = "https://console.example.com/acme/app/changes/pr"

// TestTheResultCarriesTheChangePage: a draft and a ready publication carry
// the pageUrl the provider answers as the result's changeUrl, and carry none
// when the provider answers none.
func TestTheResultCarriesTheChangePage(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "change-finalize-prints-the-change-page")
	for _, draft := range []bool{true, false} {
		for _, page := range []string{changePage, ""} {
			name := "ready"
			if draft {
				name = "draft"
			}
			if page == "" {
				name += " without a change page"
			}
			t.Run(name, func(t *testing.T) {
				f, git, provider, opts := finalizerFixture(t)
				if draft {
					git.ahead = false
					opts.Draft, opts.VerificationFile = true, ""
				}
				provider.statusHook = func(proposal *collab.Proposal) { proposal.PageURL = page }
				result, err := f.Run(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if result.ChangeURL != page {
					t.Fatalf("changeUrl = %q, want %q", result.ChangeURL, page)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var members map[string]any
				if err := json.Unmarshal(encoded, &members); err != nil {
					t.Fatal(err)
				}
				if _, present := members["changeUrl"]; present != (page != "") {
					t.Fatalf("result %s: changeUrl present = %v, want %v", encoded, present, page != "")
				}
			})
		}
	}
}

// TestTheCommandPrintsTheChangePageAfterTheProposalURL: the text output
// prints CHANGE_URL right after PROPOSAL_URL when the provider answers a
// pageUrl, and no CHANGE_URL line otherwise.
func TestTheCommandPrintsTheChangePageAfterTheProposalURL(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "change-finalize-prints-the-change-page")
	const proposalURL = "https://example.invalid/proposals/pr"
	for name, tc := range map[string]struct {
		page string
		want string
	}{
		"with a change page":    {changePage, "PROPOSAL_URL=" + proposalURL + "\nCHANGE_URL=" + changePage + "\n"},
		"without a change page": {"", "PROPOSAL_URL=" + proposalURL + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			command, h, stdout, stderr, args := draftCommand(t, "")
			h.provider.statusHook = func(proposal *collab.Proposal) {
				proposal.URL, proposal.PageURL = proposalURL, tc.page
			}
			if code := command.Run(context.Background(), args); code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			want := `PROPOSAL_REF={"source":"local:proposal","id":"pr"}` + "\n" + tc.want
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q", stdout, want)
			}
		})
	}
}
