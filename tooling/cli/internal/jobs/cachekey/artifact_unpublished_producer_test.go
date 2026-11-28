package cachekey

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// A producer that published neither its artifact's digest nor its key, such as
// a skipped step whose digest could not be read, leaves the consumer's key
// matching no stored entry: two such computations, between which a
// dependency may have changed, never share a key.
func TestArtifactInput_AProducerThatPublishedNothingSharesNoKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	consumer := artifactConsumer()
	first := artifactKey(t, consumer, map[string]string{})
	second := artifactKey(t, consumer, map[string]string{})
	if first == second {
		t.Fatalf("two keys with an unpublished producer are both %s", first)
	}
	published := map[string]string{ArtifactDigestKey(emitKey, "manifest"): "artifact-a"}
	once, again := artifactKey(t, consumer, published), artifactKey(t, consumer, published)
	if once != again {
		t.Error("a published digest no longer yields a stable key")
	}
}
