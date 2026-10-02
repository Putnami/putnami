package jobs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A publication job served from cache runs no process in this run, so it
// packs nothing into this run's outbox, and the published-member event its
// entry replays names an upload this run did not make. The release refuses
// with a named error, and no channel moves.
func TestCachedPublicationJobThatPackedNothingFailsTheRelease(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-reused-publication-job-fails-the-release")
	h := newPublicationHarness(t, providertest.Config{})
	h.cache = store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
	publish := h.addMember("npm", "@putnami/web", "1.2.3", "web", nil, fixtureScript{})
	h.publisher.Tasks = map[string]modelextension.TaskDefinition{
		"publish-npm": {Kind: "command", Declares: &modelextension.TaskDeclaration{}},
	}
	publish.Step = &modelextension.PipelineStep{ID: "npm", Task: "publish-npm"}
	publish.JobDef.Cache = true
	publish.JobDef.Env = nil
	fixtureTask(t, publish.JobDef, fixtureScript{
		{"print", publishedMemberRuntimeLine(h.members[0], digestFor('e'))},
		{"print", releaseSetSuccessLine},
	})

	planned, internal := h.plan(nil, fixedAncestry{testRevision}, nil)
	cold := h.execute(context.Background(), planned, internal, nil)
	if result := cold[publish.Key()]; result == nil || result.ReuseKind().Reused() {
		t.Fatalf("cold publication job: %s", resultMessage(result))
	}
	if outcome := cold[releaseSetResultKey]; outcome == nil || outcome.Status != "success" {
		t.Fatalf("cold release: %s", resultMessage(outcome))
	}

	h.provider, h.session = newSessionPublication(t, providertest.Config{})
	planned, internal = h.plan(nil, fixedAncestry{testRevision}, nil)
	warm := h.execute(context.Background(), planned, internal, nil)
	if result := warm[publish.Key()]; result == nil || result.ReuseKind() != ReuseLocalCache {
		t.Fatalf("warm publication job was not served from cache: %s", resultMessage(result))
	}
	if upload := warm[h.uploadKey(publish)]; upload == nil || len(upload.Events) != 0 {
		t.Fatalf("upload of the outbox a cached job left empty: %s", resultMessage(upload))
	}
	outcome := warm[releaseSetResultKey]
	if outcome == nil || outcome.Status != "failed" || outcome.Error == nil ||
		!strings.Contains(outcome.Error.Message, "reused instead of executed") || !strings.Contains(outcome.Error.Message, publish.Key()) {
		t.Fatalf("release after a cached publication job: %s", resultMessage(outcome))
	}
	if _, moved := h.provider.Head("putnami", "canary"); moved || h.provider.Releases() != 0 {
		t.Fatal("a release after a cached publication job moved the channel")
	}
}
