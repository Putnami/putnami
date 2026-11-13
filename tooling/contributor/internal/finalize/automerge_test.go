package finalize

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const autoMergeFeature = "tooling/contributor-executable"

// withPublication rewrites the fixture policy with publication as its
// publication member.
func withPublication(t *testing.T, f Finalizer, publication string) {
	t.Helper()
	policy := `{"options":{"@putnami/contributor":{"version":1,"proposals":{"source":"github:acme/app"},"states":{"delivered":"blocked"},` +
		`"verification":{"language":"en","ciGate":{"checks":["Putnami CI"]}},"publication":` + publication + `}}}`
	if err := os.WriteFile(filepath.Join(f.Root, "putnami.workspace.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
}

// autoMergeFixture is a ready, complete-outcome publication of task 42 with
// publication.autoMerge set to "squash", through a provider that offers
// automerge.
func autoMergeFixture(t *testing.T) (Finalizer, *finalizerGit, *finalizerProvider, Options) {
	t.Helper()
	f, git, provider, opts := finalizerFixture(t)
	withPublication(t, f, `{"taskReference":"Closes {url}","autoMerge":"squash"}`)
	provider.autoMergeOffered = true
	opts.TaskSource, opts.TaskID = provider.task.Ref.Source, provider.task.Ref.ID
	return f, git, provider, opts
}

func TestPolicyAutoMergeTakesSquashOrFalse(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "the-policy-takes-squash-or-false")
	f, _, _, _ := finalizerFixture(t)
	for publication, want := range map[string]AutoMerge{`{}`: "", `{"autoMerge":false}`: "", `{"autoMerge":"squash"}`: "squash"} {
		withPublication(t, f, publication)
		policy, err := ReadPolicy(f.Root)
		if err != nil || policy.Publication.AutoMerge != want {
			t.Fatalf("%s: autoMerge %q, %v; want %q", publication, policy.Publication.AutoMerge, err, want)
		}
	}
	for _, publication := range []string{`{"autoMerge":true}`, `{"autoMerge":"merge"}`, `{"autoMerge":"rebase"}`, `{"autoMerge":"Squash"}`, `{"autoMerge":1}`} {
		withPublication(t, f, publication)
		_, err := ReadPolicy(f.Root)
		if err == nil || !strings.Contains(err.Error(), `publication.autoMerge must be "squash" or false`) {
			t.Fatalf("%s was accepted: %v", publication, err)
		}
	}
}

func TestPolicyAutoMergeRefusalStopsTheRunBeforeAnyWrite(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "the-policy-takes-squash-or-false")
	f, git, provider, opts := autoMergeFixture(t)
	withPublication(t, f, `{"autoMerge":"merge"}`)
	if _, err := f.Run(context.Background(), opts); err == nil || git.pushes != 0 || provider.upserts != 0 {
		t.Fatalf("an invalid autoMerge published: err=%v pushes=%d upserts=%d", err, git.pushes, provider.upserts)
	}
}

func TestFinalizerTurnsAutoMergeOnAfterTheReadyPublication(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "auto-merge-follows-the-ready-publication")
	f, git, provider, opts := autoMergeFixture(t)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.autoMerges) != 1 {
		t.Fatalf("auto-merge requests: %d, want 1", len(provider.autoMerges))
	}
	request := provider.autoMerges[0]
	if request.Ref != result.ProposalRef || request.ExpectedHeadCommit != git.head || request.Method != collab.MergeMethodSquash {
		t.Fatalf("auto-merge request %+v for %+v at %s", request, result.ProposalRef, git.head)
	}
	ready := slices.Index(provider.log, "proposals.upsert")
	transitioned := slices.Index(provider.log, "tasks.transition")
	requested := slices.Index(provider.log, "proposals.automerge")
	if ready < 0 || transitioned < 0 || requested != len(provider.log)-1 || requested < ready || requested < transitioned {
		t.Fatalf("auto-merge is not the last call after the ready upsert and the transition: %v", provider.log)
	}
	if result.State != collab.ProposalStateOpen || result.AutoMerge == nil || !result.AutoMerge.Enabled ||
		result.AutoMerge.Method != collab.MergeMethodSquash || result.AutoMerge.Refusal != "" {
		t.Fatalf("result %+v, auto-merge %+v", result, result.AutoMerge)
	}
}

func TestFinalizerSendsNoAutoMergeWithoutTheSetting(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "no-auto-merge-without-the-setting")
	for _, publication := range []string{`{"taskReference":"Closes {url}"}`, `{"taskReference":"Closes {url}","autoMerge":false}`} {
		f, _, provider, opts := autoMergeFixture(t)
		withPublication(t, f, publication)
		result, err := f.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(provider.autoMerges) != 0 || result.AutoMerge != nil || provider.transitions != 1 || result.State != collab.ProposalStateOpen {
			t.Fatalf("%s: auto-merge requests %d, report %+v, transitions %d", publication, len(provider.autoMerges), result.AutoMerge, provider.transitions)
		}
	}
}

func TestFinalizerSendsNoAutoMergeForADraftOrAPartial(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "no-auto-merge-for-a-draft-or-a-partial")
	cases := map[string]func(*testing.T, Finalizer, *Options){
		"publication.draft": func(t *testing.T, f Finalizer, _ *Options) {
			withPublication(t, f, `{"taskReference":"Closes {url}","draft":true,"autoMerge":"squash"}`)
		},
		"--draft": func(_ *testing.T, _ Finalizer, opts *Options) {
			opts.Draft, opts.VerificationFile = true, ""
		},
		"--partial-delivery": func(t *testing.T, _ Finalizer, opts *Options) {
			opts.PartialDelivery = true
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, provider, opts := autoMergeFixture(t)
			setup(t, f, &opts)
			result, err := f.Run(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.autoMerges) != 0 || result.AutoMerge != nil || provider.upserts != 1 {
				t.Fatalf("auto-merge requests %d, report %+v, upserts %d", len(provider.autoMerges), result.AutoMerge, provider.upserts)
			}
		})
	}
}

func TestFinalizerReportsARefusedAutoMergeAndKeepsTheDelivery(t *testing.T) {
	spectest.Proves(t, autoMergeFeature, "authorized-auto-merge", "a-refused-auto-merge-is-reported-and-the-delivery-stands")
	f, _, provider, opts := autoMergeFixture(t)
	provider.autoMergeRefusal = &collab.Error{Reason: "automerge.refused", Message: "Pull request Auto merge is not allowed for this repository"}
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a refused auto-merge failed the delivery: %v", err)
	}
	if len(provider.autoMerges) != 1 || provider.transitions != 1 || result.State != collab.ProposalStateOpen {
		t.Fatalf("requests %d, transitions %d, state %s", len(provider.autoMerges), provider.transitions, result.State)
	}
	if result.AutoMerge == nil || result.AutoMerge.Enabled || !strings.Contains(result.AutoMerge.Refusal, "Auto merge is not allowed for this repository") {
		t.Fatalf("refusal report %+v", result.AutoMerge)
	}

	f, _, provider, opts = autoMergeFixture(t)
	provider.autoMergeOffered = false
	result, err = f.Run(context.Background(), opts)
	if err != nil || len(provider.autoMerges) != 0 || result.AutoMerge == nil || result.AutoMerge.Enabled ||
		!strings.Contains(result.AutoMerge.Refusal, "does not offer automerge") {
		t.Fatalf("a provider without automerge: err=%v requests=%d report=%+v", err, len(provider.autoMerges), result.AutoMerge)
	}
}
