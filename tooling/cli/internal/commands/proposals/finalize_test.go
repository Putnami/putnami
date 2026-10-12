package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

type finalizerGit struct {
	branch, head, fingerprint string
	baseTip, baseSHA          string
	clean, staged, ahead      bool
	paths                     []string
	commits                   []CommitMessage
	pushes, seeds             int
	pushHook                  func()
}

func (g *finalizerGit) Branch() (string, error)                        { return g.branch, nil }
func (g *finalizerGit) Head() (string, error)                          { return g.head, nil }
func (g *finalizerGit) BaseRef(base string) (string, error)            { return "origin/" + base, nil }
func (g *finalizerGit) BaseTip(string) (string, error)                 { return g.baseTip, nil }
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
	unresolved                                 bool
	dropLabels                                 bool
	statusHook                                 func(*collab.Proposal)
}

func (p *finalizerProvider) call(_ context.Context, contract, operation string, input json.RawMessage) collab.Envelope {
	answer := func(value any) collab.Envelope {
		data, _ := json.Marshal(value)
		return collab.Envelope{Outcome: collab.OutcomeOK, Result: data}
	}
	if operation == collab.OperationCapabilities {
		operations := []collab.OperationCapability{}
		for _, name := range []string{collab.OperationFind, collab.OperationUpsert, collab.OperationStatus, collab.OperationGet, collab.OperationTransition} {
			operations = append(operations, collab.OperationCapability{Name: name, Supported: true, Preconditions: collab.PreconditionsChecked})
		}
		capabilities := collab.Capabilities{Status: collab.BindingStatusBound, Operations: operations}
		if contract == collab.ContractProposals {
			repository := p.proposalRepository
			if repository == "" {
				repository = "Putnami/putnami"
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
		return answer(collab.ProposalListResult{Items: nil})
	case "proposals.upsert":
		p.upserts++
		if p.unresolved {
			return collab.Envelope{Outcome: collab.OutcomeUnresolved, Error: &collab.Error{Message: "unknown effect", Reconcile: "proposals.status"}}
		}
		var request collab.ProposalUpsertInput
		_ = json.Unmarshal(input, &request)
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

func finalizerFixture(t *testing.T) (Finalizer, *finalizerGit, *finalizerProvider, Options) {
	t.Helper()
	root := t.TempDir()
	policy := `{"options":{"@putnami/contributor":{"version":1,"proposals":{"source":"github:putnami/putnami"},"states":{"delivered":"blocked"},"verification":{"language":"en","ciGate":{"checks":["Putnami CI"]}},"publication":{"taskReference":"Closes #{id}","bodyMaxBytes":1500}}}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	bodyFile := filepath.Join(root, "body.txt")
	if err := os.WriteFile(bodyFile, []byte("Explain the reason\nfor the change."), 0600); err != nil {
		t.Fatal(err)
	}
	git := &finalizerGit{branch: "topic", head: "0123456789012345678901234567890123456789", fingerprint: "tree-fingerprint", baseTip: "base-tip", baseSHA: "base-sha", clean: true, ahead: true, paths: []string{"source.go"}}
	provider := &finalizerProvider{task: collab.Task{Ref: collab.Ref{Source: "github:putnami/putnami", ID: "42"}, Revision: "task-revision", URL: "https://github.com/putnami/putnami/issues/42", State: collab.TaskStateInProgress}}
	verifications := 0
	f := Finalizer{Root: root, Git: git, Call: provider.call, Verify: func(context.Context, string, string) (Verified, error) {
		verifications++
		return Verified{Fingerprint: git.fingerprint, HeadSHA: git.head, BaseSHA: git.baseSHA, GateSession: "session-id", WorkflowPresent: true, OutcomeComplete: true}, nil
	}}
	_ = verifications
	return f, git, provider, Options{Base: "main", Branch: "topic", Title: "fix: publish native proposal", BodyFile: bodyFile, VerificationFile: "dossier.json"}
}

func TestFinalizerPartialNeverClosesOrTransitions(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "partial-delivery-never-closes-parent")
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-finalizer-publishes-through-the-contracts")
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
	if result.State != collab.ProposalStateOpen || !strings.Contains(provider.proposal.Body, "Refs https://github.com/putnami/putnami/issues/42") {
		t.Fatalf("unexpected partial proposal: %+v", provider.proposal)
	}
	for _, body := range []string{"Closes #42", "Closes\n #42", "Fixes https://github.com/putnami/putnami/issues/42"} {
		git.pushes = 0
		_ = os.WriteFile(opts.BodyFile, []byte(body), 0600)
		if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 {
			t.Fatalf("closing body %q was published: %v", body, err)
		}
	}
}

func TestDraftSeedAndLegacyEmptyCommitAllowLaterPartial(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "a-draft-opens-before-the-first-edit")
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
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "the-read-back-carries-what-the-upsert-answered")
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
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-publication", "an-explicit-hosted-gate-waits-without-local-execution")
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

func TestHostedParentRequiresNativeWorkflowEligibilityAndStatus(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "hosted-parent-workflow-completeness")
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
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", WouldCompleteAfterReady: eligible,
			OutcomeComplete: eligible && request.Status != nil, ReadyUnits: readyUnits}, nil
	}
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
		t.Fatalf("incomplete parent reached publication: %v", err)
	}
	opts.PartialDelivery = true
	readyUnits = nil
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
		t.Fatalf("partial hosted workflow without an eligible unit reached publication: %v", err)
	}
	opts.PartialDelivery = false
	readyUnits = []string{"current"}
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

func TestHostedRequiredCheckNamesFailClosed(t *testing.T) {
	head := strings.Repeat("a", 40)
	checks := collab.Checks{Commit: head, State: collab.ChecksStateFailing,
		Items: []collab.Check{{Name: "required", State: collab.CheckStatePassing}, {Name: "unrelated", State: collab.CheckStateFailing}}}
	if got := hostedCheckVerdict(checks, []string{"required"}, head); got != "passing" {
		t.Fatalf("unrelated failure masked required check: %s", got)
	}
	checks.Items = append(checks.Items, collab.Check{Name: "required", State: collab.CheckStateFailing})
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

func TestChangedHeadBaseOrBodyAfterPushStopsBeforeProviderWrite(t *testing.T) {
	for _, changed := range []string{"head", "base", "body"} {
		t.Run(changed, func(t *testing.T) {
			f, git, provider, opts := finalizerFixture(t)
			git.pushHook = func() {
				if changed == "head" {
					git.head = "another-commit"
				} else if changed == "base" {
					git.baseTip = "another-base-tip"
				} else {
					_ = os.WriteFile(opts.BodyFile, []byte("Changed after push"), 0600)
				}
			}
			if _, err := f.Run(context.Background(), opts); err == nil || provider.upserts != 0 {
				t.Fatalf("changed %s reached provider upsert: %v", changed, err)
			}
		})
	}
}

func TestPolicyAndTaskReferenceAuthority(t *testing.T) {
	if err := ValidateTitle("style: align the published layout", []string{"layout.css"}); err != nil {
		t.Fatalf("repository-authorized Conventional Commits style type was refused: %v", err)
	}
	spectest.Proves(t, "cli/contributor-workflows", "repository-policy", "the-policy-selects-the-delivered-state-reference-and-language")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "the-body-is-the-squash-message")
	spectest.Proves(t, "cli/contributor-workflows", "squash-ready-publication", "no-publication-names-an-agent")
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
	policy.Proposals.Source = "github:putnami/putnami"
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
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnvironment(os.Environ())
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run(root, "init", "-q")
	run(root, "config", "user.name", "Test")
	run(root, "config", "user.email", "test@example.com")
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
