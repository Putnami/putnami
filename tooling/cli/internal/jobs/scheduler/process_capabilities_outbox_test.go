package scheduler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A job that packs into an outbox is denied the loopback registry route of
// every kind, including a kind no publisher reads yet, and every listed
// credential variable. The download mirror and ordinary variables stay.
func TestOutboxJobIsDeniedEveryRegistryRoute(t *testing.T) {
	for _, name := range []string{
		"PUTNAMI_REGISTRY_NPM_URL",
		"PUTNAMI_REGISTRY_GOMOD_URL",
		"PUTNAMI_REGISTRY_OCI_URL",
		"PUTNAMI_REGISTRY_PUT_URL",
		"PUTNAMI_REGISTRY_FUTUREKIND_URL",
		extensionproto.CloudTokenEnv,
		extensionproto.JobCredentialFDEnv,
		InternalReleasePlanCallbackEnv,
	} {
		if !WithheldFromOutboxJob(name) {
			t.Errorf("%s reaches a job that packs into an outbox", name)
		}
	}
	for _, name := range []string{"PUTNAMI_REGISTRY_URL", "PUTNAMI_REGISTRY_NPM", "PATH", "HOME"} {
		if WithheldFromOutboxJob(name) {
			t.Errorf("%s is withheld from a job that packs into an outbox", name)
		}
	}
}

// outboxTestJob is a publish job of project id, the shape of a job that packs
// into an outbox.
func outboxTestJob(t *testing.T, id string) *ScheduledJob {
	t.Helper()
	return capabilityTestJob(
		t.TempDir(),
		&workspace.Project{ID: "/" + id, Name: id, Path: id},
		"publish",
		"publish",
		"/unused",
		extension.SideEffectsRegistry,
	)
}

// The exec seam finds a job's outbox in the context the scheduler passes it,
// with no release-set run in that context: the job receives its outbox as its
// only PUTNAMI_PUBLICATION_OUTBOX and none of the withheld variables. A job
// the context does not name, and a context with no outboxes, give no outbox.
func TestExecSeamReadsThePublicationOutboxFromTheContextAlone(t *testing.T) {
	t.Parallel()
	publish := outboxTestJob(t, "web")
	other := outboxTestJob(t, "api")
	outbox := filepath.Join(t.TempDir(), "0")
	ctx := WithPublicationOutboxes(context.Background(), PublicationOutboxes{publish.Key(): outbox})
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/ci",
		"PUTNAMI_REGISTRY_URL=https://mirror.example.test",
		"PUTNAMI_REGISTRY_NPM_URL=http://127.0.0.1:4873",
		"PUTNAMI_REGISTRY_FUTUREKIND_URL=http://127.0.0.1:4874",
		extensionproto.CloudTokenEnv + "=ambient-token",
		extensionproto.JobCredentialFDEnv + "=3",
		InternalReleasePlanCallbackEnv + "=hostile",
		extensionproto.PublicationOutboxEnv + "=/inherited-outbox",
	}

	scoped := scopeProcessCapabilities(ctx, env, publish)
	if got := envValues(scoped, extensionproto.PublicationOutboxEnv); len(got) != 1 || got[0] != outbox {
		t.Fatalf("the publication job's %s = %q, want [%q]", extensionproto.PublicationOutboxEnv, got, outbox)
	}
	for _, entry := range scoped {
		if name, _, _ := strings.Cut(entry, "="); WithheldFromOutboxJob(name) {
			t.Errorf("the publication job received %s", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "PUTNAMI_REGISTRY_URL"} {
		if len(envValues(scoped, name)) != 1 {
			t.Errorf("the publication job lost %s", name)
		}
	}

	if got := envValues(scopeProcessCapabilities(ctx, env, other), extensionproto.PublicationOutboxEnv); len(got) != 0 {
		t.Fatalf("a job the context does not name received %s = %q", extensionproto.PublicationOutboxEnv, got)
	}
	if got := envValues(scopeProcessCapabilities(context.Background(), env, publish), extensionproto.PublicationOutboxEnv); len(got) != 0 {
		t.Fatalf("a context with no outboxes gave %s = %q", extensionproto.PublicationOutboxEnv, got)
	}
}

// The context holds its own copy of the outboxes: a change to the caller's
// map after WithPublicationOutboxes reaches no job.
func TestPublicationOutboxesContextHoldsItsOwnCopy(t *testing.T) {
	t.Parallel()
	publish := outboxTestJob(t, "web")
	other := outboxTestJob(t, "api")
	outboxes := PublicationOutboxes{publish.Key(): "/outboxes/0"}
	ctx := WithPublicationOutboxes(context.Background(), outboxes)
	outboxes[publish.Key()] = "/outboxes/changed"
	outboxes[other.Key()] = "/outboxes/added"

	if got := publicationOutboxFor(ctx, publish); got != "/outboxes/0" {
		t.Fatalf("the publication job's outbox = %q, want %q", got, "/outboxes/0")
	}
	if got := publicationOutboxFor(ctx, other); got != "" {
		t.Fatalf("a job added after the context was built has outbox %q", got)
	}
}
