package finalize

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

type finalizerGit struct {
	branch, head, fingerprint string
	baseSHA                   string
	clean, staged, ahead      bool
	paths                     []string
	commits                   []CommitMessage
	pushes, seeds             int
	pushHook                  func()
}

func (g *finalizerGit) Branch() (string, error)                        { return g.branch, nil }
func (g *finalizerGit) Head() (string, error)                          { return g.head, nil }
func (g *finalizerGit) BaseRef(base string) (string, error)            { return "origin/" + base, nil }
func (g *finalizerGit) BaseSHA(string) (string, error)                 { return g.baseSHA, nil }
func (g *finalizerGit) Clean() (bool, error)                           { return g.clean, nil }
func (g *finalizerGit) Staged() (bool, error)                          { return g.staged, nil }
func (g *finalizerGit) Ahead(string) (bool, error)                     { return g.ahead, nil }
func (g *finalizerGit) ChangedPaths(string) ([]string, error)          { return g.paths, nil }
func (g *finalizerGit) CommitMessages(string) ([]CommitMessage, error) { return g.commits, nil }
func (g *finalizerGit) Seed(title string) error {
	g.seeds++
	g.ahead = true
	g.head = "seeded-head"
	g.commits = append(g.commits, CommitMessage{Text: title, Empty: true})
	return nil
}
func (g *finalizerGit) Push(string) error {
	g.pushes++
	if g.pushHook != nil {
		g.pushHook()
	}
	return nil
}
func (g *finalizerGit) Fingerprint() (string, error) { return g.fingerprint, nil }

type finalizerProvider struct {
	proposal                                   collab.Proposal
	task                                       collab.Task
	checks                                     collab.Checks
	proposalRepository                         string
	upserts, transitions, reviews, statusReads int
	findReads                                  int
	findHeadCommits                            []string
	findStates                                 [][]collab.ProposalState
	settled                                    []collab.Proposal
	findPageSize                               int
	findExisting                               bool
	rejectRevision                             bool
	upsertRevision                             string
	unresolved                                 bool
	dropLabels                                 bool
	statusHook                                 func(*collab.Proposal)
}

// call is a Provider: every operation reaches the fake provider.
func (p *finalizerProvider) call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	return p.envelope(ctx, contract, operation, input), nil
}

func (p *finalizerProvider) envelope(_ context.Context, contract, operation string, input json.RawMessage) collab.Envelope {
	answer := func(value any) collab.Envelope {
		data, _ := json.Marshal(value)
		return collab.Envelope{Outcome: collab.OutcomeOK, Result: data}
	}
	if operation == collab.OperationCapabilities {
		names := []string{collab.OperationFind, collab.OperationUpsert, collab.OperationStatus, collab.OperationGet, collab.OperationTransition}
		operations := make([]collab.OperationCapability, 0, len(names))
		for _, name := range names {
			operations = append(operations, collab.OperationCapability{Name: name, Supported: true, Preconditions: collab.PreconditionsChecked})
		}
		capabilities := collab.Capabilities{Status: collab.BindingStatusBound, Operations: operations}
		if contract == collab.ContractProposals {
			repository := p.proposalRepository
			if repository == "" {
				repository = "acme/app"
			}
			settings, _ := json.Marshal(map[string]string{"repository": repository})
			capabilities.Provider = &collab.ProviderIdentity{Name: "@putnami/github-collaboration"}
			capabilities.Binding = &collab.Binding{Provider: "@putnami/github-collaboration", Version: 1, Settings: settings}
		}
		return answer(capabilities)
	}
	switch contract + "." + operation {
	case "tasks.get":
		return answer(collab.TaskResult{Task: p.task})
	case "tasks.transition":
		p.transitions++
		var request collab.TaskTransitionInput
		_ = json.Unmarshal(input, &request)
		if request.ExpectedRevision != p.task.Revision {
			return collab.Envelope{Outcome: collab.OutcomeConflict, Error: &collab.Error{Message: "missing revision"}}
		}
		p.task.State = request.State
		p.task.Revision = "next"
		return answer(collab.TaskResult{Task: p.task})
	case "proposals.find":
		p.findReads++
		var request collab.ProposalFindInput
		_ = json.Unmarshal(input, &request)
		p.findHeadCommits = append(p.findHeadCommits, request.Change.HeadCommit)
		p.findStates = append(p.findStates, request.States)
		var items []collab.Proposal
		if p.findExisting {
			items = append(items, p.proposal)
		}
		items = append(items, p.settled...)
		if len(request.States) > 0 {
			items = slices.DeleteFunc(items, func(item collab.Proposal) bool { return !slices.Contains(request.States, item.State) })
		}
		page, next := 0, ""
		if request.Page != nil && request.Page.Cursor != "" {
			page, _ = strconv.Atoi(request.Page.Cursor)
		}
		if p.findPageSize > 0 {
			start := min(page*p.findPageSize, len(items))
			end := min(start+p.findPageSize, len(items))
			if end < len(items) {
				next = strconv.Itoa(page + 1)
			}
			items = items[start:end]
		}
		return answer(collab.ProposalListResult{Items: items, Page: collab.Page{Next: next}})
	case "proposals.upsert":
		p.upserts++
		var request collab.ProposalUpsertInput
		_ = json.Unmarshal(input, &request)
		p.upsertRevision = request.ExpectedRevision
		if p.rejectRevision || p.findExisting && request.ExpectedRevision != p.proposal.Revision {
			return collab.Envelope{Outcome: collab.OutcomeConflict, Error: &collab.Error{Reason: collab.ReasonRevisionConflict, Message: "revision changed before the single upsert"}}
		}
		if p.unresolved {
			return collab.Envelope{Outcome: collab.OutcomeUnresolved, Error: &collab.Error{Message: "unknown effect", Reconcile: "proposals.status"}}
		}
		p.proposal = collab.Proposal{Ref: collab.Ref{Source: "local:proposal", ID: "pr"}, Revision: "pr-revision", Change: request.Change,
			Title: request.Title, Body: request.Body, Labels: []string{"parent"}}
		p.proposal.Change.Repository = "repository"
		if request.Draft != nil && *request.Draft {
			p.proposal.State = collab.ProposalStateDraft
		} else {
			p.proposal.State = collab.ProposalStateOpen
		}
		return answer(collab.ProposalUpsertResult{Proposal: p.proposal})
	case "proposals.status":
		p.statusReads++
		read := p.proposal
		if p.statusHook != nil {
			p.statusHook(&read)
		}
		if p.dropLabels {
			read.Labels = nil
		}
		return answer(collab.ProposalStatusResult{Proposal: read, Checks: p.checks})
	case "proposals.review":
		p.reviews++
		return answer(collab.ProposalReviewResult{})
	default:
		return collab.Envelope{Outcome: collab.OutcomeUnsupported, Error: &collab.Error{Message: "unexpected call"}}
	}
}

func existingDraftProposal(p *finalizerProvider, head string) {
	p.findExisting = true
	p.proposal = collab.Proposal{
		Ref: collab.Ref{Source: "local:proposal", ID: "pr"}, Revision: "before-push",
		Change: collab.Change{Repository: "repository", Base: "main", Head: "topic", HeadCommit: head},
		Title:  "fix: earlier proposal", Body: "Earlier body.", State: collab.ProposalStateDraft,
		ProviderState: "draft", Labels: []string{"parent"}, Assignees: []string{}, UpdatedAt: "2026-10-12T00:00:00Z",
	}
}

func TestOwnPushRefreshesExistingProposalRevisionBeforeSingleUpsert(t *testing.T) {
	t.Parallel()
	f, git, provider, opts := finalizerFixture(t)
	opts.Draft, opts.VerificationFile = true, ""
	existingDraftProposal(provider, "prior-head")
	git.pushHook = func() {
		provider.proposal.Change.HeadCommit = git.head
		provider.proposal.Revision = "after-own-push"
		provider.proposal.UpdatedAt = "2026-10-13T00:00:00Z"
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.State != collab.ProposalStateDraft || provider.findReads != 2 ||
		provider.upserts != 1 || provider.upsertRevision != "after-own-push" || git.pushes != 1 {
		t.Fatalf("own push was not reconciled before one upsert: result=%+v err=%v finds=%d upserts=%d expected=%q pushes=%d",
			result, err, provider.findReads, provider.upserts, provider.upsertRevision, git.pushes)
	}
	if !slices.Equal(provider.findHeadCommits, []string{git.head, git.head}) {
		t.Fatalf("finds did not name the pushed HEAD, so a provider cannot await its proposal head: %v", provider.findHeadCommits)
	}
}

func TestStoredProposalHeadAdvancesOnlyOnUpsert(t *testing.T) {
	t.Parallel()
	f, git, provider, opts := finalizerFixture(t)
	opts.Draft, opts.VerificationFile = true, ""
	existingDraftProposal(provider, "prior-head")
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.State != collab.ProposalStateDraft || provider.findReads != 2 ||
		provider.upserts != 1 || provider.upsertRevision != "before-push" ||
		provider.proposal.Change.HeadCommit != git.head || result.HeadSHA != git.head {
		t.Fatalf("stored proposal did not advance on one checked upsert: result=%+v err=%v finds=%d upserts=%d revision=%q proposal=%+v",
			result, err, provider.findReads, provider.upserts, provider.upsertRevision, provider.proposal)
	}
}

func TestPostPushProposalDriftStopsBeforeUpsert(t *testing.T) {
	cases := []struct {
		name   string
		change func(*collab.Proposal)
	}{
		{name: "title", change: func(p *collab.Proposal) { p.Title = "fix: concurrent title" }},
		{name: "body", change: func(p *collab.Proposal) { p.Body = "Concurrent body." }},
		{name: "state", change: func(p *collab.Proposal) { p.State, p.ProviderState = collab.ProposalStateOpen, "open" }},
		{name: "labels", change: func(p *collab.Proposal) { p.Labels = []string{"parent", "new"} }},
		{name: "assignees", change: func(p *collab.Proposal) { p.Assignees = []string{"another"} }},
		{name: "identity", change: func(p *collab.Proposal) { p.Ref.ID = "other" }},
		{name: "repository", change: func(p *collab.Proposal) { p.Change.Repository = "other/repo" }},
		{name: "base", change: func(p *collab.Proposal) { p.Change.Base = "other" }},
		{name: "head", change: func(p *collab.Proposal) { p.Change.Head = "other" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, git, provider, opts := finalizerFixture(t)
			opts.Draft, opts.VerificationFile = true, ""
			existingDraftProposal(provider, "prior-head")
			git.pushHook = func() {
				provider.proposal.Change.HeadCommit = git.head
				provider.proposal.Revision = "after-concurrent-edit"
				test.change(&provider.proposal)
			}
			if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != 0 || provider.findReads != 2 {
				t.Fatalf("%s drift reached proposal write: err=%v upserts=%d finds=%d", test.name, err, provider.upserts, provider.findReads)
			}
		})
	}
}

func TestPostPushRevisionWithoutHeadChangeOrNewProposalStopsBeforeUpsert(t *testing.T) {
	for _, test := range []struct {
		name      string
		appearing bool
		oldHead   bool
	}{{name: "revision-only current head"}, {name: "revision-only stored head", oldHead: true}, {name: "concurrent creation", appearing: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, git, provider, opts := finalizerFixture(t)
			opts.Draft, opts.VerificationFile = true, ""
			if !test.appearing {
				head := git.head
				if test.oldHead {
					head = "prior-head"
				}
				existingDraftProposal(provider, head)
			}
			git.pushHook = func() {
				if test.appearing {
					existingDraftProposal(provider, git.head)
				} else {
					provider.proposal.Revision = "concurrent-revision"
					provider.proposal.UpdatedAt = "2026-10-13T00:00:00Z"
				}
			}
			if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != 0 || provider.findReads != 2 {
				t.Fatalf("%s reached proposal write: err=%v upserts=%d finds=%d", test.name, err, provider.upserts, provider.findReads)
			}
		})
	}
}

func TestPostPushUnrelatedHeadOrLateRevisionConflictNeverRetriesWrite(t *testing.T) {
	for _, test := range []struct {
		name         string
		lateConflict bool
	}{{name: "unrelated head"}, {name: "late revision conflict", lateConflict: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, git, provider, opts := finalizerFixture(t)
			opts.Draft, opts.VerificationFile = true, ""
			existingDraftProposal(provider, "prior-head")
			git.pushHook = func() {
				provider.proposal.Change.HeadCommit = "unrelated-head"
				if test.lateConflict {
					provider.proposal.Change.HeadCommit = git.head
				}
				provider.proposal.Revision = "after-push"
			}
			if test.lateConflict {
				provider.rejectRevision = true
			}
			_, err := f.Run(context.Background(), opts)
			wantUpserts := 0
			if test.lateConflict {
				wantUpserts = 1
			}
			if err == nil || provider.upserts != wantUpserts || provider.findReads != 2 || provider.transitions != 0 {
				t.Fatalf("unrelated head or late conflict retried publication: err=%v upserts=%d finds=%d transitions=%d",
					err, provider.upserts, provider.findReads, provider.transitions)
			}
		})
	}
}

func TestPostPushHeadChangeWithoutNewRevisionStopsBeforeUpsert(t *testing.T) {
	t.Parallel()
	f, git, provider, opts := finalizerFixture(t)
	opts.Draft, opts.VerificationFile = true, ""
	existingDraftProposal(provider, "prior-head")
	git.pushHook = func() { provider.proposal.Change.HeadCommit = git.head }
	if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != 0 || provider.findReads != 2 {
		t.Fatalf("head moved without a new revision: err=%v upserts=%d finds=%d", err, provider.upserts, provider.findReads)
	}
}

func settledProposal(state collab.ProposalState, id, head string) collab.Proposal {
	return collab.Proposal{
		Ref: collab.Ref{Source: "github:acme/app", ID: id}, Revision: "settled-" + id, URL: "https://github.com/acme/app/pull/" + id,
		Change: collab.Change{Repository: "acme/app", Base: "main", Head: "topic", HeadCommit: head},
		Title:  "fix: publish native proposal", State: state, ProviderState: string(state),
	}
}

// TestASettledProposalOfTheBranchOpensNoReplacement pins the lookup of every
// state: a merged proposal at HEAD is reported and nothing is written, and a
// closed one, or a merged one at another head, fails and names itself.
func TestASettledProposalOfTheBranchOpensNoReplacement(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "a-settled-proposal-of-the-branch-opens-no-replacement")
	for _, test := range []struct {
		name    string
		draft   bool
		settled func(head string) []collab.Proposal
		want    collab.Ref // the reported proposal, or the zero Ref for a failure
		errHas  []string
	}{
		{name: "merged at HEAD", settled: func(head string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateMerged, "7", head)}
		}, want: collab.Ref{Source: "github:acme/app", ID: "7"}},
		{name: "merged at HEAD, draft run", draft: true, settled: func(head string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateMerged, "7", head)}
		}, want: collab.Ref{Source: "github:acme/app", ID: "7"}},
		{name: "closed earlier, merged at HEAD later", settled: func(head string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateClosed, "5", "old"), settledProposal(collab.ProposalStateMerged, "7", head)}
		}, want: collab.Ref{Source: "github:acme/app", ID: "7"}},
		{name: "closed", settled: func(head string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateClosed, "5", head)}
		}, errHas: []string{"closed github:acme/app/5 (https://github.com/acme/app/pull/5)", "opens no replacement"}},
		{name: "merged at another head", settled: func(string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateMerged, "7", "before-reuse")}
		}, errHas: []string{"merged github:acme/app/7 (https://github.com/acme/app/pull/7) at before-reuse", "opens no replacement"}},
		{name: "closed and merged at another head", settled: func(string) []collab.Proposal {
			return []collab.Proposal{settledProposal(collab.ProposalStateClosed, "5", "x"), settledProposal(collab.ProposalStateMerged, "7", "y")}
		}, errHas: []string{"closed github:acme/app/5", "merged github:acme/app/7"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, git, provider, opts := finalizerFixture(t)
			opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
			if test.draft {
				opts.Draft, opts.VerificationFile, opts.TaskSource, opts.TaskID = true, "", "", ""
				git.ahead = false
			}
			provider.settled = test.settled(git.head)
			result, err := f.Run(context.Background(), opts)
			if provider.upserts != 0 || provider.transitions != 0 || git.pushes != 0 || git.seeds != 0 || provider.reviews != 0 {
				t.Fatalf("a settled proposal reached a write: upserts=%d transitions=%d pushes=%d seeds=%d reviews=%d",
					provider.upserts, provider.transitions, git.pushes, git.seeds, provider.reviews)
			}
			if len(provider.findStates) != 1 || len(provider.findStates[0]) != 0 {
				t.Fatalf("the lookup restricted the states it read: %v", provider.findStates)
			}
			if test.want != (collab.Ref{}) {
				if err != nil || result.ProposalRef != test.want || result.State != collab.ProposalStateMerged || result.HeadSHA != git.head ||
					result.ProposalURL != "https://github.com/acme/app/pull/"+test.want.ID {
					t.Fatalf("merged proposal was not reported: result=%+v err=%v", result, err)
				}
				return
			}
			for _, want := range test.errHas {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error %v does not name %q", err, want)
				}
			}
		})
	}
}

// TestAnOpenProposalWinsOverSettledOnesAcrossPages pins that the open or draft
// proposal of the branch is the one to update, whatever settled proposals
// precede it, and that the lookup follows every page.
func TestAnOpenProposalWinsOverSettledOnesAcrossPages(t *testing.T) {
	t.Parallel()
	f, git, provider, opts := finalizerFixture(t)
	opts.Draft, opts.VerificationFile = true, ""
	existingDraftProposal(provider, "prior-head")
	provider.settled = []collab.Proposal{settledProposal(collab.ProposalStateClosed, "5", "a"), settledProposal(collab.ProposalStateMerged, "6", git.head)}
	provider.findPageSize = 1
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.State != collab.ProposalStateDraft || provider.upserts != 1 || provider.upsertRevision != "before-push" || git.pushes != 1 {
		t.Fatalf("the open proposal was not updated: result=%+v err=%v upserts=%d revision=%q pushes=%d",
			result, err, provider.upserts, provider.upsertRevision, git.pushes)
	}
}

// TestAMergedProposalOnALaterPageIsFound pins that a settled proposal beyond
// the first page still stops the run.
func TestAMergedProposalOnALaterPageIsFound(t *testing.T) {
	t.Parallel()
	f, git, provider, opts := finalizerFixture(t)
	provider.settled = []collab.Proposal{settledProposal(collab.ProposalStateClosed, "4", "a"), settledProposal(collab.ProposalStateClosed, "5", "b"),
		settledProposal(collab.ProposalStateMerged, "6", git.head)}
	provider.findPageSize = 1
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.ProposalRef.ID != "6" || result.State != collab.ProposalStateMerged || provider.findReads != 3 || provider.upserts != 0 || git.pushes != 0 {
		t.Fatalf("the merged proposal on page 3 was missed: result=%+v err=%v finds=%d upserts=%d pushes=%d", result, err, provider.findReads, provider.upserts, git.pushes)
	}
}

func finalizerFixture(t *testing.T) (Finalizer, *finalizerGit, *finalizerProvider, Options) {
	t.Helper()
	root := t.TempDir()
	policy := `{"options":{"@putnami/contributor":{"version":1,"proposals":{"source":"github:acme/app"},"states":{"delivered":"blocked"},"verification":{"language":"en","ciGate":{"checks":["Putnami CI"]}},"publication":{"taskReference":"Closes #{id}","bodyMaxBytes":1500}}}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	bodyFile := filepath.Join(root, "body.txt")
	if err := os.WriteFile(bodyFile, []byte("Explain the reason\nfor the change."), 0600); err != nil {
		t.Fatal(err)
	}
	git := &finalizerGit{branch: "topic", head: "0123456789012345678901234567890123456789", fingerprint: "tree-fingerprint", baseSHA: "base-sha", clean: true, ahead: true, paths: []string{"source.go"}}
	provider := &finalizerProvider{task: collab.Task{Ref: collab.Ref{Source: "github:acme/app", ID: "42"}, Revision: "task-revision", URL: "https://github.com/acme/app/issues/42", State: collab.TaskStateInProgress}}
	verifications := 0
	f := Finalizer{Root: root, Git: git, Call: provider.call, Verify: func(context.Context, string, string) (Verified, error) {
		verifications++
		return Verified{Fingerprint: git.fingerprint, HeadSHA: git.head, BaseSHA: git.baseSHA, GateSession: "session-id", WorkflowPresent: true, OutcomeComplete: true}, nil
	}}
	_ = verifications
	return f, git, provider, Options{Base: "main", Branch: "topic", Title: "fix: publish native proposal", BodyFile: bodyFile, VerificationFile: "dossier.json"}
}

func TestFinalizerPartialNeverClosesOrTransitions(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "partial-delivery-never-closes-parent")
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "the-finalizer-publishes-through-the-contracts")
	f, git, provider, opts := finalizerFixture(t)
	opts.PartialDelivery, opts.TaskSource, opts.TaskID = true, provider.task.Ref.Source, provider.task.Ref.ID
	if err := os.WriteFile(opts.BodyFile, []byte("Explain the change.\n\nRefs #420"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if provider.transitions != 0 || git.pushes != 1 {
		t.Fatalf("pushes=%d transitions=%d", git.pushes, provider.transitions)
	}
	if result.State != collab.ProposalStateOpen || !strings.Contains(provider.proposal.Body, "Refs https://github.com/acme/app/issues/42") {
		t.Fatalf("unexpected partial proposal: %+v", provider.proposal)
	}
	for _, body := range []string{"Closes #42", "Closes\n #42", "Fixes https://github.com/acme/app/issues/42"} {
		git.pushes = 0
		_ = os.WriteFile(opts.BodyFile, []byte(body), 0600)
		if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 {
			t.Fatalf("closing body %q was published: %v", body, err)
		}
	}
}

func TestDraftSeedAndLegacyEmptyCommitAllowLaterPartial(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "a-draft-opens-before-the-first-edit")
	f, git, provider, opts := finalizerFixture(t)
	opts.Draft, opts.VerificationFile = true, ""
	opts.TaskSource, opts.TaskID, opts.PartialDelivery = provider.task.Ref.Source, provider.task.Ref.ID, true
	git.ahead = false
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if git.seeds != 1 || len(git.commits) != 1 || git.commits[0].Text != opts.Title {
		t.Fatalf("draft seed carried more than title: %+v", git.commits)
	}
	git.commits = append(git.commits, CommitMessage{Text: "Closes #42", Empty: true})
	opts.Draft, opts.VerificationFile = false, "dossier.json"
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if provider.transitions != 0 {
		t.Fatal("partial transitioned parent")
	}
	git.commits = append(git.commits, CommitMessage{Text: "Closes #42", Empty: false})
	if _, err := f.Run(context.Background(), opts); err == nil {
		t.Fatal("nonempty closing commit passed")
	}
}

func TestCurrentProofAndReadbackGuardWrites(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "the-read-back-carries-what-the-upsert-answered")
	spectest.Proves(t, "collaboration/provider-contract", "callers-never-repeat-an-unresolved-write", "the-contributor-stops-an-unresolved-write-without-a-retry")
	f, git, provider, opts := finalizerFixture(t)
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	provider.dropLabels = true
	if _, err := f.Run(context.Background(), opts); err == nil || provider.transitions != 0 {
		t.Fatalf("missing label readback passed: %v", err)
	}
	provider.dropLabels = false
	git.pushes = 0
	f.Verify = func(context.Context, string, string) (Verified, error) {
		return Verified{}, errors.New("stale dossier")
	}
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 {
		t.Fatalf("stale proof pushed: %v", err)
	}
	provider.unresolved = true
	f.Verify = func(context.Context, string, string) (Verified, error) {
		return Verified{Fingerprint: git.fingerprint, HeadSHA: git.head, BaseSHA: git.baseSHA, GateSession: "session"}, nil
	}
	before := provider.upserts
	if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != before+1 || provider.transitions != 0 {
		t.Fatalf("unresolved write retried/dependent write: %v", err)
	}
}

func TestDossierUsesResolvedRemoteBase(t *testing.T) {
	f, git, _, opts := finalizerFixture(t)
	var verifiedBase string
	f.Verify = func(_ context.Context, _ string, base string) (Verified, error) {
		verifiedBase = base
		return Verified{Fingerprint: git.fingerprint, HeadSHA: git.head, BaseSHA: git.baseSHA, GateSession: "session-id"}, nil
	}
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if verifiedBase != "origin/main" {
		t.Fatalf("dossier saw %q, expected the resolved remote base origin/main", verifiedBase)
	}
}

func TestExplicitHostedChecksUseNoLocalVerifierAndReturnFailedProposalToDraft(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "an-explicit-hosted-gate-waits-without-local-execution")
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile = "ci", ""
	f.Verify = func(context.Context, string, string) (Verified, error) {
		t.Fatal("hosted route called local verifier")
		return Verified{}, nil
	}
	provider.checks = collab.Checks{State: collab.ChecksStatePassing, Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	provider.checks = collab.Checks{State: collab.ChecksStateFailing, Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStateFailing}}}
	if _, err := f.Run(context.Background(), opts); err == nil || provider.proposal.State != collab.ProposalStateDraft {
		t.Fatalf("failing hosted checks did not restore draft: state=%s err=%v", provider.proposal.State, err)
	}
}

func TestUnsupportedHostedChecksReturnProposalToDraftWithoutPolling(t *testing.T) {
	f, _, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile, opts.WorkflowFile = "ci", "", ".context/workflow.json"
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	provider.checks = collab.Checks{State: collab.ChecksStateUnsupported, Detail: "the provider has no hosted checks"}
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		if request.Status != nil {
			t.Fatal("unsupported hosted checks reached native status-backed promotion")
		}
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", WouldCompleteAfterReady: true, ReadyUnits: []string{"current"}}, nil
	}
	waits := 0
	f.Wait = func(context.Context, time.Duration) error {
		waits++
		return nil
	}
	_, err := f.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "hosted checks unsupported; proposal returned to draft") ||
		waits != 0 || provider.upserts != 2 || provider.transitions != 0 || provider.proposal.State != collab.ProposalStateDraft {
		t.Fatalf("unsupported checks did not fail closed: err=%v waits=%d upserts=%d transitions=%d state=%s",
			err, waits, provider.upserts, provider.transitions, provider.proposal.State)
	}
}

func TestHostedParentRequiresNativeWorkflowEligibilityAndStatus(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "hosted-parent-workflow-completeness")
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile, opts.WorkflowFile = "ci", "", ".context/workflow.json"
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	provider.checks = collab.Checks{State: collab.ChecksStatePassing, Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	opts.WorkflowFile = ""
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 {
		t.Fatalf("hosted parent without workflow reached a write: %v", err)
	}
	opts.WorkflowFile = ".context/workflow.json"
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 {
		t.Fatalf("missing native workflow verifier reached a write: %v", err)
	}
	before, after := 0, 0
	eligible := false
	readyUnits := []string{"current"}
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		if request.File != opts.WorkflowFile || request.HeadSHA != git.head || request.BaseSHA != git.baseSHA || request.Fingerprint != git.fingerprint {
			t.Fatalf("workflow binding %+v", request)
		}
		if request.Status == nil {
			before++
		} else {
			after++
			if request.ProposalRef != provider.proposal.Ref || request.Status.Proposal.Ref != provider.proposal.Ref {
				t.Fatalf("provider status was not bound to the proposal: %+v", request)
			}
		}
		reason := ""
		if !eligible {
			reason = "workflow ready needs native producer proof"
		}
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", WouldCompleteAfterReady: eligible,
			OutcomeComplete: eligible && request.Status != nil, ReadyUnits: readyUnits, IncompleteReason: reason}, nil
	}
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 ||
		!strings.Contains(err.Error(), "cannot complete the parent: workflow ready needs native producer proof;") ||
		!strings.Contains(err.Error(), "--partial-delivery") {
		t.Fatalf("incomplete parent reached publication or hid its native blocker: %v", err)
	}
	eligible = true
	result, err := f.Run(context.Background(), opts)
	if err != nil || provider.transitions != 1 || before < 2 || after != 2 || result.WorkflowSHA256 != "workflow-sha" || len(result.ReadyUnits) != 1 || result.HostedWorkflowReceipt == nil {
		t.Fatalf("hosted parent status was not promoted: result=%+v err=%v before=%d after=%d", result, err, before, after)
	}
	receipt := result.HostedWorkflowReceipt
	if receipt.Workflow.Path != opts.WorkflowFile || receipt.Workflow.SHA256 != "workflow-sha" ||
		receipt.BaseSHA != git.baseSHA || receipt.HeadSHA != git.head || receipt.Fingerprint != git.fingerprint ||
		receipt.ProposalRef != provider.proposal.Ref || receipt.Status.Proposal.Ref != provider.proposal.Ref ||
		len(receipt.RequiredChecks) != 1 || receipt.RequiredChecks[0] != "Putnami CI" {
		t.Fatalf("hosted receipt lost native binding or provider status: %+v", receipt)
	}
	git.pushes = 0
	provider.upserts = 0
	provider.transitions = 0
	before, after = 0, 0
	changing := false
	git.pushHook = func() { changing = true }
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		sha := "first"
		if changing {
			sha = "changed"
		}
		return HostedWorkflowResult{WorkflowSHA256: sha, WouldCompleteAfterReady: true}, nil
	}
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 1 || provider.upserts != 0 || provider.transitions != 0 {
		t.Fatalf("changed workflow reached provider write: %v", err)
	}
}

func TestHostedPolicyRejectsDuplicateChecksBeforeWrite(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile = "ci", ""
	path := filepath.Join(f.Root, "putnami.workspace.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range []string{`["Putnami CI","Putnami CI"]`, `["Putnami CI",""]`} {
		bad := strings.Replace(string(contents), `["Putnami CI"]`, names, 1)
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
			t.Fatalf("invalid hosted check policy reached write: names=%s err=%v", names, err)
		}
	}
}

func TestHostedPartialPromotesOnlyCurrentUnitsAndKeepsParentOpen(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile, opts.WorkflowFile = "ci", "", ".context/workflow.json"
	opts.TaskSource, opts.TaskID, opts.PartialDelivery = provider.task.Ref.Source, provider.task.Ref.ID, true
	provider.checks = collab.Checks{Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		return HostedWorkflowResult{WorkflowSHA256: "frozen-workflow", ReadyUnits: []string{"current"}}, nil
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil || provider.transitions != 0 || result.State != collab.ProposalStateOpen ||
		len(result.ReadyUnits) != 1 || result.ReadyUnits[0] != "current" || result.HostedWorkflowReceipt == nil {
		t.Fatalf("partial hosted publication changed parent or lost current unit: result=%+v task=%s err=%v", result, provider.task.State, err)
	}
	if strings.Contains(provider.proposal.Body, "Closes ") {
		t.Fatalf("partial publication has closing directive: %s", provider.proposal.Body)
	}
}

func TestHostedPartialWithoutAnEligibleUnitPublishesWithoutAReceipt(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "a-partial-without-an-eligible-unit-publishes-without-a-receipt")
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile, opts.WorkflowFile = "ci", "", ".context/workflow.json"
	opts.TaskSource, opts.TaskID, opts.PartialDelivery = provider.task.Ref.Source, provider.task.Ref.ID, true
	provider.checks = collab.Checks{Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	statuses := 0
	// The native verifier finds a valid workflow whose current unit is still
	// building, so it names no ready unit before or after the hosted checks.
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		if request.Status != nil {
			statuses++
		}
		return HostedWorkflowResult{WorkflowSHA256: "frozen-workflow"}, nil
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil || git.pushes != 1 || statuses == 0 || provider.transitions != 0 || provider.task.State != collab.TaskStateInProgress ||
		result.State != collab.ProposalStateOpen || result.Gate != "hosted checks passed on "+git.head ||
		result.WorkflowSHA256 != "frozen-workflow" || len(result.ReadyUnits) != 0 || result.HostedWorkflowReceipt != nil {
		t.Fatalf("partial without an eligible unit was not published as progress only: result=%+v statuses=%d err=%v", result, statuses, err)
	}
	if !strings.Contains(provider.proposal.Body, "Refs https://github.com/acme/app/issues/42") || strings.Contains(provider.proposal.Body, "Closes ") {
		t.Fatalf("partial lost its non-closing task reference: %s", provider.proposal.Body)
	}
	opts.PartialDelivery = false
	git.pushes = 0
	if _, err := f.Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "use --partial-delivery") || git.pushes != 0 {
		t.Fatalf("normal delivery without an eligible unit reached a write: %v", err)
	}
}

func TestHostedFeaturelessPartialUsesHostedChecksWithoutWorkflow(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "a-featureless-partial-needs-no-workflow")
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile = "ci", ""
	opts.TaskSource, opts.TaskID, opts.PartialDelivery = provider.task.Ref.Source, provider.task.Ref.ID, true
	git.paths = []string{"protocols/infra/schema.go"}
	provider.checks = collab.Checks{Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	refused := func(want string) {
		t.Helper()
		if _, err := f.Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), want) || git.pushes != 0 || provider.upserts != 0 {
			t.Fatalf("want refusal %q before a write, got %v (pushes=%d upserts=%d)", want, err, git.pushes, provider.upserts)
		}
	}
	refused("native project ownership unavailable")
	var ownership Ownership
	var ownershipErr error
	f.Owners = func(_ context.Context, base string, paths []string) (Ownership, error) {
		if base != git.baseSHA || !slices.Equal(paths, git.paths) {
			t.Fatalf("owners asked for %s %v", base, paths)
		}
		return ownership, ownershipErr
	}
	ownership.Projects = []string{"/tooling/cli"}
	refused("projects without featureAuthority.none or with authored features: /tooling/cli")
	ownership = Ownership{Unowned: []string{"README.md"}}
	refused("paths no project owns: README.md")
	ownership, ownershipErr = Ownership{}, errors.New("ownership failed")
	refused("ownership failed")
	ownershipErr, git.paths = nil, nil
	refused("has no changed path")
	git.paths = []string{"protocols/infra/schema.go"}
	opts.PartialDelivery = false
	refused("--gate ci with a parent task requires --workflow-file")
	opts.PartialDelivery = true
	result, err := f.Run(context.Background(), opts)
	if err != nil || git.pushes != 1 || provider.transitions != 0 || result.State != collab.ProposalStateOpen ||
		result.Gate != "hosted checks passed on "+git.head || result.WorkflowSHA256 != "" || len(result.ReadyUnits) != 0 ||
		result.HostedWorkflowReceipt != nil {
		t.Fatalf("featureless partial was not published on hosted checks: result=%+v err=%v", result, err)
	}
	if !strings.Contains(provider.proposal.Body, "Refs https://github.com/acme/app/issues/42") || strings.Contains(provider.proposal.Body, "Closes ") {
		t.Fatalf("featureless partial lost its non-closing task reference: %s", provider.proposal.Body)
	}
}

func TestBoundedListNamesTheRest(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	if got := boundedList(items); got != "a, b, c, d, e, f, g, h, i, j and 2 more" {
		t.Fatalf("bounded list %q", got)
	}
}

func TestHostedRequiredCheckNamesFailClosed(t *testing.T) {
	head := strings.Repeat("a", 40)
	checks := collab.Checks{Commit: head, State: collab.ChecksStateFailing,
		Items: []collab.Check{{Name: "required", State: collab.CheckStatePassing}, {Name: "unrelated", State: collab.CheckStateFailing}}}
	if got := hostedCheckVerdict(checks, []string{"required"}, head); got != "passing" {
		t.Fatalf("unrelated failure masked required check: %s", got)
	}
	checks.Items = append(checks.Items, collab.Check{Name: "required", State: collab.CheckStatePassing})
	if got := hostedCheckVerdict(checks, []string{"required"}, head); got != "passing" {
		t.Fatalf("duplicate passing check did not pass: %s", got)
	}
	checks.Items[2].State = collab.CheckStatePending
	if got := hostedCheckVerdict(checks, []string{"required"}, head); got != "pending" {
		t.Fatalf("duplicate pending check did not wait: %s", got)
	}
	checks.Items[2].State = collab.CheckStateFailing
	if got := hostedCheckVerdict(checks, []string{"required"}, head); got != "failing" {
		t.Fatalf("duplicate mixed check passed: %s", got)
	}
	checks.Items = checks.Items[:2]
	if got := hostedCheckVerdict(checks, []string{"required", "required"}, head); got != "unsupported" {
		t.Fatalf("duplicate policy check passed: %s", got)
	}
}

func TestHostedChecksRejectProposalThatReturnsToDraft(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	opts.Gate, opts.VerificationFile = "ci", ""
	provider.checks = collab.Checks{Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	provider.statusHook = func(proposal *collab.Proposal) {
		if provider.statusReads > 1 {
			proposal.State = collab.ProposalStateDraft
		}
	}
	if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != 1 || provider.transitions != 0 || provider.reviews != 0 {
		t.Fatalf("draft proposal accepted hosted proof: upserts=%d transitions=%d reviews=%d err=%v",
			provider.upserts, provider.transitions, provider.reviews, err)
	}
}

func TestChangedHeadMergeBaseOrBodyAfterPushStopsBeforeProviderWrite(t *testing.T) {
	for changed, message := range map[string]string{
		"head":       "HEAD changed during finalization",
		"merge base": "publication merge base moved during finalization",
		"body":       "body file changed during finalization",
	} {
		t.Run(changed, func(t *testing.T) {
			f, git, provider, opts := finalizerFixture(t)
			git.pushHook = func() {
				if changed == "head" {
					git.head = "another-commit"
				} else if changed == "merge base" {
					git.baseSHA = "another-merge-base"
				} else {
					_ = os.WriteFile(opts.BodyFile, []byte("Changed after push"), 0600)
				}
			}
			if _, err := f.Run(context.Background(), opts); err == nil || err.Error() != message || provider.upserts != 0 {
				t.Fatalf("changed %s reached provider upsert: %v", changed, err)
			}
		})
	}
}

// fetchingGit is a real repository whose push stands for another worktree of
// the same clone: it moves the shared origin/main ref while finalize runs.
type fetchingGit struct {
	NativeGit
	fetch func()
}

func (g fetchingGit) Push(string) error {
	g.fetch()
	return nil
}

func TestABaseFetchedDuringFinalizationFailsOnlyWhenTheMergeBaseMoves(t *testing.T) {
	for _, tc := range []struct {
		name, from string
		fails      bool
	}{
		{name: "advanced base", from: "origin/main"},
		{name: "rewritten base", from: "origin/main~1", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
				cmd.Env = gitEnvironment(os.Environ())
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			policy := `{"options":{"@putnami/contributor":{"version":1,"proposals":{"source":"github:acme/app"},"verification":{"language":"en"},"publication":{"bodyMaxBytes":1500}}}}`
			git("init", "-q", "-b", "main")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.com")
			git("config", "commit.gpgsign", "false")
			if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "putnami.workspace.json")
			git("commit", "-qm", "chore: workspace")
			git("commit", "-q", "--allow-empty", "-m", "chore: second")
			git("update-ref", "refs/remotes/origin/main", "HEAD")
			git("switch", "-q", "-c", "topic")
			if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "source.go")
			git("commit", "-qm", "fix: correct the source")
			bodyFile := filepath.Join(t.TempDir(), "body.txt")
			if err := os.WriteFile(bodyFile, []byte("Explain the reason for the change."), 0600); err != nil {
				t.Fatal(err)
			}
			native := NativeGit{Root: root}
			provider := &finalizerProvider{}
			f := Finalizer{Root: root, Call: provider.call,
				Git: fetchingGit{NativeGit: native, fetch: func() {
					moved := git("commit-tree", "-p", tc.from, "-m", "chore: merged elsewhere", tc.from+"^{tree}")
					git("update-ref", "refs/remotes/origin/main", moved)
				}},
				Verify: func(context.Context, string, string) (Verified, error) {
					head, _ := native.Head()
					fingerprint, _ := native.Fingerprint()
					baseSHA, _ := native.BaseSHA("origin/main")
					return Verified{Fingerprint: fingerprint, HeadSHA: head, BaseSHA: baseSHA, GateSession: "session-id"}, nil
				}}
			opts := Options{Base: "main", Branch: "topic", Title: "fix: correct the source", BodyFile: bodyFile, VerificationFile: "dossier.json"}
			_, err := f.Run(context.Background(), opts)
			if tc.fails {
				if err == nil || !strings.Contains(err.Error(), "merge base moved") || provider.upserts != 0 {
					t.Fatalf("a moved merge base reached the provider: upserts=%d err=%v", provider.upserts, err)
				}
				return
			}
			if err != nil || provider.upserts != 1 {
				t.Fatalf("a fetched base that keeps the merge base failed finalization: upserts=%d err=%v", provider.upserts, err)
			}
		})
	}
}

func TestPolicyAndTaskReferenceAuthority(t *testing.T) {
	if err := ValidateTitle("style: align the published layout", []string{"layout.css"}); err != nil {
		t.Fatalf("repository-authorized Conventional Commits style type was refused: %v", err)
	}
	spectest.Proves(t, "tooling/contributor-executable", "repository-policy", "the-policy-selects-the-delivered-state-reference-and-language")
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "the-body-is-the-squash-message")
	spectest.Proves(t, "tooling/contributor-executable", "squash-ready-publication", "no-publication-names-an-agent")
	f, _, provider, opts := finalizerFixture(t)
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	if _, err := f.Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if provider.task.State != collab.TaskStateBlocked || !strings.HasSuffix(provider.proposal.Body, "Closes #42") {
		t.Fatalf("policy not followed: %+v", provider)
	}
	if err := os.WriteFile(opts.BodyFile, []byte("Nous corrigeons le code"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(context.Background(), opts); err == nil {
		t.Fatal("French proposal passed")
	}
	if err := os.WriteFile(opts.BodyFile, []byte("Generated with Codex"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(context.Background(), opts); err == nil {
		t.Fatal("agent attribution passed")
	}
	policy := Policy{}
	policy.Proposals.Source = "github:acme/app"
	policy.Publication.TaskReference = "Closes #{id}"
	foreign := collab.Task{Ref: collab.Ref{Source: "github:elsewhere/repo", ID: "42"}, URL: "https://github.com/elsewhere/repo/issues/42"}
	if _, err := TaskReference(policy, foreign, false); err == nil {
		t.Fatal("cross-repository id-only closing reference passed")
	}
	partial, err := TaskReference(policy, foreign, true)
	if err != nil || partial != "Refs "+foreign.URL {
		t.Fatalf("cross-repository partial reference: %q %v", partial, err)
	}
	foreign.URL = ""
	partial, err = TaskReference(policy, foreign, true)
	if err != nil || strings.Contains(partial, "#42") {
		t.Fatalf("opaque fallback manufactured issue identity: %q %v", partial, err)
	}
	foreign.Ref.ID = "opaque-id"
	policy.Publication.TaskReference = "Closes {url}"
	if _, err := TaskReference(policy, foreign, false); err == nil {
		t.Fatal("missing URL passed")
	}
	if got := NormalizeBody("- Keep the leading-dash list\n  item together.\n\nPlain prose\nwraps here."); got != "- Keep the leading-dash list item together.\n\nPlain prose wraps here." {
		t.Fatalf("unexpected normalized body: %q", got)
	}
}

func TestIdOnlyClosingRequiresActualProposalBinding(t *testing.T) {
	for _, repository := range []string{"someone/else", ""} {
		t.Run(repository, func(t *testing.T) {
			f, git, provider, opts := finalizerFixture(t)
			provider.proposalRepository = repository
			if repository == "" {
				// A binding with no repository cannot prove the policy's source.
				provider.proposalRepository = "invalid/repository/shape"
			}
			opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
			if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
				t.Fatalf("mismatched binding reached mutation: pushes=%d upserts=%d err=%v", git.pushes, provider.upserts, err)
			}
		})
	}
}

func TestNativeGitIgnoresForeignGitDirectoryAndDraftSeed(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnvironment(os.Environ())
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	run(root, "init", "-q")
	run(root, "config", "user.name", "Test")
	run(root, "config", "user.email", "test@example.com")
	run(root, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(root, "readme"), []byte("test"), 0600)
	run(root, "add", "readme")
	run(root, "commit", "-qm", "chore: baseline")
	foreign := t.TempDir()
	run(foreign, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	git := NativeGit{Root: root}
	if _, err := git.Head(); err != nil {
		t.Fatal(err)
	}
	if fingerprint, err := git.Fingerprint(); err != nil || len(fingerprint) != 64 {
		t.Fatalf("foreign Git directory changed fingerprint root: %q %v", fingerprint, err)
	}
	if err := git.Seed("fix: seed title only"); err != nil {
		t.Fatal(err)
	}
	commits, err := git.CommitMessages("HEAD~1")
	if err != nil || len(commits) != 1 || !commits[0].Empty || commits[0].Text != "fix: seed title only" {
		t.Fatalf("seed: %+v %v", commits, err)
	}
}

func TestTransitionReasonKeepsWholeCharacters(t *testing.T) {
	reason := strings.Repeat("a", 255) + "é"
	got := truncateUTF8(reason, 256)
	if got != strings.Repeat("a", 255) || !utf8.ValidString(got) {
		t.Fatalf("truncated reason split a character: %q", got)
	}
	if got := truncateUTF8("short", 256); got != "short" {
		t.Fatalf("short reason changed: %q", got)
	}
}

// TestACallThatNeverReachesTheProviderStopsTheRun pins that a call the host
// channel cannot carry is an error that stops the run before any write: the
// executable has no envelope to read for it.
func TestACallThatNeverReachesTheProviderStopsTheRun(t *testing.T) {
	t.Parallel()
	f, git, _, opts := finalizerFixture(t)
	f.Call = func(context.Context, string, string, json.RawMessage) (collab.Envelope, error) {
		return collab.Envelope{}, errors.New("no host channel")
	}
	if _, err := f.Run(context.Background(), opts); err == nil || err.Error() != "no host channel" || git.pushes != 0 || git.seeds != 0 {
		t.Fatalf("error %v, pushes %d, seeds %d; want the call error before any write", err, git.pushes, git.seeds)
	}
}
