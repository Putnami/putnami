package jobs

import (
	"context"
	"maps"
	"slices"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/envkeys"
)

// A native publication run exports one loopback registry route per registry
// kind, named PUTNAMI_REGISTRY_<KIND>_URL (the extension SDK's privatebroker),
// which a publication job uses when it uploads itself. A job that packs into
// an outbox receives none of them, whatever the kind. PUTNAMI_REGISTRY_URL,
// which names no kind, is the download mirror and is not a route.
const (
	registryRouteEnvPrefix = "PUTNAMI_REGISTRY_"
	registryRouteEnvSuffix = "_URL"
)

// outboxJobWithheldEnv is every other variable through which a publication
// job could reach a registry or cloud credential. A job that packs into an
// outbox receives none of them.
var outboxJobWithheldEnv = []string{
	extproto.CloudTokenEnv,
	extproto.JobCredentialFDEnv,
	extproto.CloudCapabilityAfterEnv,
	InternalReleasePlanCallbackEnv,
	InternalReleaseSetProviderCapabilityEnv,
	runtimeproto.ReleaseSetPublishedImagesFileEnv,
	runtimeproto.ReleaseSetMembersFileEnv,
}

// withheldFromOutboxJob reports whether a job that packs into an outbox is
// denied the variable name: a registry route of any kind, or a variable
// outboxJobWithheldEnv lists. Names compare the way the platform's process
// environment compares them.
func withheldFromOutboxJob(name string) bool {
	canonical := envkeys.Host.Canonical(name)
	if len(canonical) > len(registryRouteEnvPrefix)+len(registryRouteEnvSuffix) &&
		strings.HasPrefix(canonical, registryRouteEnvPrefix) && strings.HasSuffix(canonical, registryRouteEnvSuffix) {
		return true
	}
	return slices.ContainsFunc(outboxJobWithheldEnv, func(key string) bool {
		return envkeys.Host.Canonical(key) == canonical
	})
}

// PublicationOutboxes maps the key of each job that packs into an outbox to
// its outbox directory.
type PublicationOutboxes map[string]string

// publicationOutboxesContextKey is private so repository-controlled code
// cannot name an outbox through a job context. The outboxes travel only in
// the orchestrator's in-memory context.Context.
type publicationOutboxesContextKey struct{}

// WithPublicationOutboxes returns ctx carrying a copy of outboxes. At the exec
// seam, a job outboxes names receives its directory in
// PUTNAMI_PUBLICATION_OUTBOX and no credential or registry route
// (scopeProcessCapabilities). A later change to outboxes does not reach ctx.
func WithPublicationOutboxes(ctx context.Context, outboxes PublicationOutboxes) context.Context {
	return context.WithValue(ctx, publicationOutboxesContextKey{}, maps.Clone(outboxes))
}

// publicationOutboxesFrom is the outboxes ctx carries, nil when it carries
// none.
func publicationOutboxesFrom(ctx context.Context) PublicationOutboxes {
	outboxes, _ := ctx.Value(publicationOutboxesContextKey{}).(PublicationOutboxes)
	return outboxes
}

// publicationOutboxFor is the outbox job packs into, "" when it packs into
// none.
func publicationOutboxFor(ctx context.Context, job *ScheduledJob) string {
	if ctx == nil || job == nil || job.JobDef == nil || job.Project == nil {
		return ""
	}
	return publicationOutboxesFrom(ctx)[job.Key()]
}

// publicationOutboxEnv is env without any credential or registry route, and
// with the outbox the job packs into as its only PUTNAMI_PUBLICATION_OUTBOX.
func publicationOutboxEnv(env []string, outbox string) []string {
	kept := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); !withheldFromOutboxJob(name) {
			kept = append(kept, entry)
		}
	}
	return envkeys.Host.Set(kept, extproto.PublicationOutboxEnv, outbox)
}
