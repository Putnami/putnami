package finalize

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const queueFeature = "tooling/contributor-executable"

// withCIGate rewrites the fixture policy with ciGate as its
// verification.ciGate member and publication.autoMerge set to "squash".
func withCIGate(t *testing.T, f Finalizer, ciGate string) {
	t.Helper()
	policy := `{"options":{"@putnami/contributor":{"version":1,"proposals":{"source":"github:acme/app"},"states":{"delivered":"blocked"},` +
		`"verification":{"language":"en","ciGate":` + ciGate + `},"publication":{"taskReference":"Closes {url}","autoMerge":"squash"}}}}`
	if err := os.WriteFile(filepath.Join(f.Root, "putnami.workspace.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
}

// queueFixture is a hosted ready publication with no task, through a provider
// that offers automerge, on a base that merges through a queue: the head
// carries only the queue's gating check, which stays pending until the
// proposal enters the queue.
func queueFixture(t *testing.T) (Finalizer, *finalizerGit, *finalizerProvider, Options) {
	t.Helper()
	f, git, provider, opts := finalizerFixture(t)
	withCIGate(t, f, `{"checks":["Putnami CI"],"mergeQueue":["main"]}`)
	provider.autoMergeOffered = true
	provider.checks = collab.Checks{State: collab.ChecksStatePending, Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePending}}}
	opts.Gate, opts.VerificationFile = "ci", ""
	f.Verify = func(context.Context, string, string) (Verified, error) {
		t.Fatal("the hosted route called the local verifier")
		return Verified{}, nil
	}
	f.Wait = func(context.Context, time.Duration) error {
		t.Fatal("a queue gate waited for a check on the head")
		return nil
	}
	return f, git, provider, opts
}

func TestPolicyListsTheMergeQueueBases(t *testing.T) {
	spectest.Proves(t, queueFeature, "merge-queue-gate", "the-policy-lists-the-merge-queue-bases")
	f, git, provider, opts := queueFixture(t)
	for value, want := range map[string][]string{``: nil, `,"mergeQueue":null`: nil, `,"mergeQueue":[]`: {},
		`,"mergeQueue":["main"]`: {"main"}, `,"mergeQueue":["main","release/1.x"]`: {"main", "release/1.x"}} {
		withCIGate(t, f, `{"checks":["Putnami CI"]`+value+`}`)
		policy, err := ReadPolicy(f.Root)
		if err != nil || !slices.Equal(policy.Verification.CIGate.MergeQueue, want) {
			t.Fatalf("%s: mergeQueue %v, %v; want %v", value, policy.Verification.CIGate.MergeQueue, err, want)
		}
	}
	for _, value := range []string{`true`, `false`, `"main"`, `1`, `{}`, `[""]`, `[" "]`, `["main "]`, `["main","main"]`, `[1]`, `[null]`} {
		withCIGate(t, f, `{"checks":["Putnami CI"],"mergeQueue":`+value+`}`)
		if _, err := ReadPolicy(f.Root); err == nil {
			t.Fatalf("mergeQueue %s was accepted", value)
		}
		if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
			t.Fatalf("mergeQueue %s published: err=%v pushes=%d upserts=%d", value, err, git.pushes, provider.upserts)
		}
	}
}

func TestQueueGateTurnsAutoMergeOnWithoutHeadChecks(t *testing.T) {
	spectest.Proves(t, queueFeature, "merge-queue-gate", "a-queue-gate-turns-auto-merge-on-without-head-checks")
	f, git, provider, opts := queueFixture(t)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != collab.ProposalStateOpen || result.Gate != "merge queue of main" {
		t.Fatalf("state %s, gate %q", result.State, result.Gate)
	}
	if len(provider.autoMerges) != 1 || result.AutoMerge == nil || !result.AutoMerge.Enabled ||
		provider.autoMerges[0].ExpectedHeadCommit != git.head || provider.autoMerges[0].Ref != result.ProposalRef {
		t.Fatalf("auto-merge requests %+v, report %+v", provider.autoMerges, result.AutoMerge)
	}
	if requested := slices.Index(provider.log, "proposals.automerge"); requested != len(provider.log)-1 ||
		requested < slices.Index(provider.log, "proposals.upsert") {
		t.Fatalf("auto-merge is not the last call after the ready upsert: %v", provider.log)
	}

	// The readback before the request names another head: the request is
	// not sent, and the delivery stands. The reads are the upsert's, the
	// gate's, then the request's own.
	f, _, provider, opts = queueFixture(t)
	provider.statusHook = func(read *collab.Proposal) {
		if provider.statusReads > 2 {
			read.Change.HeadCommit = "moved"
		}
	}
	result, err = f.Run(context.Background(), opts)
	if err != nil || len(provider.autoMerges) != 0 || result.AutoMerge == nil || result.AutoMerge.Enabled ||
		!strings.HasPrefix(result.AutoMerge.Refusal, "not sent: queued proposal") {
		t.Fatalf("a moved head reached auto-merge: err=%v requests=%d report=%+v", err, len(provider.autoMerges), result.AutoMerge)
	}

	// A local dossier publication ignores the member.
	f, git, provider, opts = queueFixture(t)
	opts.Gate, opts.VerificationFile = "", "dossier.json"
	f.Verify = func(context.Context, string, string) (Verified, error) {
		return Verified{Fingerprint: git.fingerprint, HeadSHA: git.head, BaseSHA: git.baseSHA, GateSession: "session-id"}, nil
	}
	result, err = f.Run(context.Background(), opts)
	if err != nil || result.Gate != "dossier dossier.json, session session-id" || len(provider.autoMerges) != 1 {
		t.Fatalf("a dossier publication under a queue policy: err=%v result=%+v", err, result)
	}
}

func TestQueueGatePromotesNoUnit(t *testing.T) {
	spectest.Proves(t, queueFeature, "merge-queue-gate", "a-queue-gate-promotes-no-unit")
	f, _, provider, opts := queueFixture(t)
	opts.TaskSource, opts.TaskID, opts.WorkflowFile = provider.task.Ref.Source, provider.task.Ref.ID, ".context/workflow.json"
	eligible := true
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		if request.Status != nil {
			t.Fatal("a queue gate gave the workflow verifier a hosted status")
		}
		reason := ""
		if !eligible {
			reason = "workflow ready needs native producer proof"
		}
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", WouldCompleteAfterReady: eligible, ReadyUnits: []string{"current"}, IncompleteReason: reason}, nil
	}
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if provider.transitions != 1 || provider.task.State != collab.TaskStateBlocked || len(result.ReadyUnits) != 0 ||
		result.HostedWorkflowReceipt != nil || result.WorkflowSHA256 != "workflow-sha" || result.AutoMerge == nil || !result.AutoMerge.Enabled {
		t.Fatalf("queue delivery: transitions=%d task=%s result=%+v", provider.transitions, provider.task.State, result)
	}
	if transitioned, requested := slices.Index(provider.log, "tasks.transition"), slices.Index(provider.log, "proposals.automerge"); transitioned < 0 || requested < transitioned {
		t.Fatalf("auto-merge did not follow the delivered transition: %v", provider.log)
	}

	// The native verifier still decides completeness before any write.
	f, git, provider, opts2 := queueFixture(t)
	opts2.TaskSource, opts2.TaskID, opts2.WorkflowFile = opts.TaskSource, opts.TaskID, opts.WorkflowFile
	eligible = false
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", IncompleteReason: "workflow ready needs native producer proof"}, nil
	}
	if _, err := f.Run(context.Background(), opts2); err == nil || git.pushes != 0 || provider.upserts != 0 ||
		!strings.Contains(err.Error(), "cannot complete the parent") {
		t.Fatalf("an incomplete parent reached a queue publication: %v", err)
	}

	// A partial delivery publishes with no unit, no transition and no request.
	f, _, provider, opts3 := queueFixture(t)
	opts3.TaskSource, opts3.TaskID, opts3.WorkflowFile, opts3.PartialDelivery = opts.TaskSource, opts.TaskID, opts.WorkflowFile, true
	f.Workflow = func(_ context.Context, request HostedWorkflowRequest) (HostedWorkflowResult, error) {
		if request.Status != nil {
			t.Fatal("a queue gate gave the workflow verifier a hosted status")
		}
		return HostedWorkflowResult{WorkflowSHA256: "workflow-sha", ReadyUnits: []string{"current"}}, nil
	}
	result, err = f.Run(context.Background(), opts3)
	if err != nil || provider.transitions != 0 || len(result.ReadyUnits) != 0 || result.HostedWorkflowReceipt != nil ||
		result.AutoMerge != nil || result.Gate != "merge queue of main" {
		t.Fatalf("queue partial: err=%v transitions=%d result=%+v", err, provider.transitions, result)
	}
}

func TestWithoutAQueueTheHostedGateWaitsForTheHead(t *testing.T) {
	spectest.Proves(t, queueFeature, "merge-queue-gate", "without-a-queue-the-hosted-gate-waits-for-the-head")
	for _, queues := range []string{``, `,"mergeQueue":["release/1.x"]`} {
		t.Run("mergeQueue"+queues, func(t *testing.T) { hostedGateWaitsForTheHead(t, queues) })
	}
}

// hostedGateWaitsForTheHead publishes toward main under a policy whose
// mergeQueue member is queues: it waits for the head's checks, times out while
// they are pending, and turns auto-merge on once they pass.
func hostedGateWaitsForTheHead(t *testing.T, queues string) {
	f, git, provider, opts := queueFixture(t)
	withCIGate(t, f, `{"checks":["Putnami CI"],"timeoutMinutes":1,"pollSeconds":30`+queues+`}`)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	waits := 0
	f.Now = func() time.Time { return now }
	f.Wait = func(_ context.Context, interval time.Duration) error {
		waits++
		now = now.Add(interval)
		return nil
	}
	_, err := f.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "hosted checks pending at timeout") || waits == 0 || len(provider.autoMerges) != 0 {
		t.Fatalf("pending head checks without a queue: err=%v waits=%d requests=%d", err, waits, len(provider.autoMerges))
	}
	provider.checks = collab.Checks{State: collab.ChecksStatePassing, Commit: git.head,
		Items: []collab.Check{{Name: "Putnami CI", State: collab.CheckStatePassing}}}
	result, err := f.Run(context.Background(), opts)
	if err != nil || result.Gate != "hosted checks passed on "+git.head || len(provider.autoMerges) != 1 || !result.AutoMerge.Enabled {
		t.Fatalf("passing head checks without a queue: err=%v result=%+v requests=%d", err, result, len(provider.autoMerges))
	}
}
