package oci

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sync/errgroup"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// This file holds the registry-facing publish helpers (as opposed to the
// assembly helpers in assemble.go/base.go): pushing an assembled image to a
// registry, assigning session refs (via the protocol/oci tag-digest fast path or
// the standard distribution API), resolving the published digest, and the small
// ref/auth helpers they share. The push/apply/resolve helpers take a
// *jsonl.Emitter so progress and diagnostics stream to the run — the same
// emitter the TS and Go publishers both already use, so this stays portable
// across ecosystems while keeping behavior identical to the original publisher.

// BuildRef joins a registry, image name, and tag into a full reference,
// e.g. ("registry.example.com", "team/app", "1.2.3") →
// "registry.example.com/team/app:1.2.3". An empty registry yields a
// registry-less "team/app:1.2.3".
func BuildRef(registry, imageName, tag string) string {
	ref := imageName + ":" + tag
	if registry != "" {
		registry = strings.TrimRight(registry, "/")
		return registry + "/" + ref
	}
	return ref
}

// UniqueStrings returns s with duplicates removed, preserving first-seen order.
func UniqueStrings(s []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}

// WrapAuthError returns a neutral, cloud-sourced error when a registry rejects
// the credentials and @putnami/cloud supplied a hint; otherwise it returns err
// unchanged. The framework holds no registry host knowledge — the actionable
// guidance comes from the cloud's own message (hint), so a core-only install is
// never pointed at a command it does not ship.
func WrapAuthError(err error, hint string) error {
	if err == nil || hint == "" || !IsAuthError(err) {
		return err
	}
	return fmt.Errorf("%s: %w", hint, err)
}

// IsAuthError reports whether err looks like a registry authentication failure.
func IsAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401 unauthorized") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "invalid credentials") ||
		// A token endpoint that answers an unauthorized push with a 200 carrying an
		// empty token makes go-containerregistry surface "no token in bearer
		// response". Classify it as auth so the cloud's own guidance (credHint)
		// reaches the user instead of this internals string.
		strings.Contains(msg, "no token in bearer response")
}

// execStderr extracts stderr from an exec result, returning "" if result is nil.
func execStderr(result *exec.Result) string {
	if result != nil {
		return result.Stderr
	}
	return ""
}

// PushOptions holds the one per-call transport override supported by the OCI
// layout publisher. A nil Transport preserves the standard registry transport.
// This stays per call so a private publication broker cannot change process-wide
// registry routing or affect another publisher running in the same process.
type PushOptions struct {
	Transport http.RoundTripper
}

// transportOf returns the single per-call transport override, or nil for the
// standard registry transport. More than one options value is a caller bug.
func transportOf(options []PushOptions) (http.RoundTripper, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("registry helpers accept at most one options value")
	}
	if len(options) == 1 {
		return options[0].Transport, nil
	}
	return nil, nil
}

// craneOptions builds the crane options for one registry call: the keychain
// always, plus the per-call transport when one is set.
func craneOptions(keychain authn.Keychain, transport http.RoundTripper) []crane.Option {
	options := []crane.Option{crane.WithAuthFromKeychain(keychain)}
	if transport != nil {
		options = append(options, crane.WithTransport(transport))
	}
	return options
}

// layoutUploadJobs bounds the concurrent blob uploads of one image push. It is
// go-containerregistry's default, stated here so the load one publisher puts on
// a shared registry is a reviewed choice rather than a library detail.
const layoutUploadJobs = 4

// PushContent pushes the content once, unless it already exists in the registry.
// Packages assembled in-process carry an OCI layout (layout != "") — the image is
// pushed from it directly (no docker daemon, and the digest is preserved
// bit-for-bit). Legacy daemon-built packages (layout == "") fall back to docker
// tag/push. It returns an error (after emitting a diagnostic and ending the
// docker-push phase as failed) on failure.
func PushContent(emit *jsonl.Emitter, wsRoot, dockerDir, layout, localRef, contentRef string, contentExists bool, keychain authn.Keychain, credHint string, options ...PushOptions) error {
	_, err := PushContentWithDigest(emit, wsRoot, dockerDir, layout, localRef, contentRef, contentExists, keychain, credHint, options...)
	return err
}

// PushContentWithDigest is PushContent that also returns the digest of the
// manifest it wrote from an OCI layout: the SHA-256 of the exact bytes the
// registry accepted in the manifest PUT. It returns "" when it wrote no layout
// manifest — the content already exists, or the legacy daemon path pushed it.
//
// A layout push never asks the registry whether the manifest exists. The
// caller's cache lookup already did, and a manifest HEAD that misses is the most
// expensive request a registry serves; remote.Write would send it again. Blob
// uploads keep their own existence checks, so shared layers still transfer once.
func PushContentWithDigest(emit *jsonl.Emitter, wsRoot, dockerDir, layout, localRef, contentRef string, contentExists bool, keychain authn.Keychain, credHint string, options ...PushOptions) (string, error) {
	transport, err := transportOf(options)
	if err != nil {
		return "", err
	}
	if contentExists {
		emit.Info(fmt.Sprintf("Content %s already exists in registry, skipping push", contentRef))
		return "", nil
	}
	if layout != "" {
		img, err := LoadFromLayout(filepath.Join(dockerDir, layout))
		if err != nil {
			emit.Diagnostic("error", "Failed to read OCI layout: "+err.Error(), "", 0)
			emit.PhaseEnd("docker-push", "failed")
			return "", fmt.Errorf("read OCI layout: %w", err)
		}
		ref, err := name.ParseReference(contentRef)
		if err != nil {
			emit.Diagnostic("error", "Invalid content ref "+contentRef+": "+err.Error(), "", 0)
			emit.PhaseEnd("docker-push", "failed")
			return "", fmt.Errorf("parse content ref: %w", err)
		}
		digest, err := pushLayoutImage(ref, img, keychain, transport)
		if err != nil {
			err = WrapAuthError(err, credHint)
			emit.Diagnostic("error", fmt.Sprintf("Failed to push %s: %v", contentRef, err), "", 0)
			emit.PhaseEnd("docker-push", "failed")
			return "", fmt.Errorf("push %s: %w", contentRef, err)
		}
		emit.Info("Pushed: " + contentRef)
		return digest, nil
	}

	// Legacy daemon path: the image lives in the local docker daemon.
	if localRef != contentRef {
		tagResult, _ := exec.Run("docker", []string{"tag", localRef, contentRef}, exec.Dir(wsRoot))
		if tagResult == nil || !tagResult.Success {
			emit.Diagnostic("error", fmt.Sprintf("Failed to tag %s → %s:\n%s", localRef, contentRef, execStderr(tagResult)), "", 0)
			emit.PhaseEnd("docker-push", "failed")
			return "", fmt.Errorf("failed to tag %s as %s: %s", localRef, contentRef, execStderr(tagResult))
		}
	}
	pushResult, _ := exec.Run("docker", []string{"push", contentRef}, exec.Dir(wsRoot))
	if pushResult == nil || !pushResult.Success {
		emit.Diagnostic("error", fmt.Sprintf("Failed to push %s:\n%s", contentRef, execStderr(pushResult)), "", 0)
		emit.PhaseEnd("docker-push", "failed")
		return "", fmt.Errorf("failed to push %s: %s", contentRef, execStderr(pushResult))
	}
	emit.Info("Pushed: " + contentRef)
	return "", nil
}

// pushLayoutImage uploads the image's blobs, then PUTs its manifest at ref, and
// returns the manifest digest. It is remote.Write without the manifest HEAD
// remote.Write always sends first: every blob goes through one Pusher, so the
// registry handshake happens once and a shared blob is uploaded once.
func pushLayoutImage(ref name.Reference, img v1.Image, keychain authn.Keychain, transport http.RoundTripper) (string, error) {
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("computing manifest digest: %w", err)
	}
	// A digest reference names the only bytes it may hold. Refuse a mismatch
	// before uploading anything instead of trusting the registry to reject it.
	if byDigest, ok := ref.(name.Digest); ok && byDigest.DigestStr() != digest.String() {
		return "", fmt.Errorf("layout manifest digest %s does not match target digest %s", digest, byDigest.DigestStr())
	}
	blobs, err := imageBlobs(img)
	if err != nil {
		return "", err
	}

	// The errgroup below bounds the uploads; WithJobs holds any fan-out inside
	// the library to the same bound.
	options := []remote.Option{remote.WithAuthFromKeychain(keychain), remote.WithJobs(layoutUploadJobs)}
	if transport != nil {
		options = append(options, remote.WithTransport(transport))
	}
	pusher, err := remote.NewPusher(options...)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	var uploads errgroup.Group
	uploads.SetLimit(layoutUploadJobs)
	for _, blob := range blobs {
		uploads.Go(func() error { return pusher.Upload(ctx, ref.Context(), blob) })
	}
	if err := uploads.Wait(); err != nil {
		return "", err
	}
	// The manifest goes last so the registry never holds a manifest whose blobs
	// are missing. Put sends the PUT alone, with no existence check.
	if err := pusher.Put(ctx, ref, img); err != nil {
		return "", err
	}
	return digest.String(), nil
}

// imageBlobs lists the blobs a manifest PUT of img requires: its layers, then
// its config.
func imageBlobs(img v1.Image) ([]v1.Layer, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("reading layout layers: %w", err)
	}
	config, err := partial.ConfigLayer(img)
	if err != nil {
		return nil, fmt.Errorf("reading layout config: %w", err)
	}
	return append(append([]v1.Layer(nil), layers...), config), nil
}

// ApplySessionRefs assigns the session's tags to the pushed content. When the
// registry is a putnami OCI registry and the digest is known, all tags are
// assigned in one batched digest-addressed call (protocol/oci tag-digest/v1);
// otherwise (or when the fast path fails) each tag is a crane manifest-only PUT
// in parallel — no layer re-upload either way. srcRef addresses the content (by
// digest when available, by content tag otherwise). It returns an error (after
// emitting a diagnostic and ending the docker-push phase as failed) when any tag
// fails. The optional transport applies to every registry call made here, the
// tag-digest probe included, so a private publication broker sees all of them.
func ApplySessionRefs(emit *jsonl.Emitter, registry, imageName, srcRef, contentDigest string, sessionTags []string, keychain authn.Keychain, credHint string, options ...PushOptions) error {
	transport, err := transportOf(options)
	if err != nil {
		return err
	}
	tagsToApply := make([]string, 0, len(sessionTags))
	for _, tag := range UniqueStrings(sessionTags) {
		if BuildRef(registry, imageName, tag) == srcRef {
			continue
		}
		tagsToApply = append(tagsToApply, tag)
	}
	if len(tagsToApply) == 0 {
		return nil
	}

	if contentDigest != "" {
		if applied := tryPutnamiTagDigest(emit, registry, imageName, srcRef, contentDigest, tagsToApply, keychain, transport); applied {
			return nil
		}
	}

	g, _ := errgroup.WithContext(context.Background())
	for _, sessionTag := range tagsToApply {
		g.Go(func() error {
			if err := crane.Tag(srcRef, sessionTag, craneOptions(keychain, transport)...); err != nil {
				return fmt.Errorf("tag %s as %s: %w", srcRef, sessionTag, WrapAuthError(err, credHint))
			}
			emit.Info("Tagged: " + BuildRef(registry, imageName, sessionTag))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		emit.Diagnostic("error", fmt.Sprintf("Failed to apply session refs: %v", err), "", 0)
		emit.PhaseEnd("docker-push", "failed")
		return fmt.Errorf("apply session refs: %w", err)
	}
	return nil
}

// tryPutnamiTagDigest attempts the putnami registry's batched tag-digest fast
// path. It reports whether the tags were applied; any failure — registry without
// the capability, credential trouble, endpoint error — leaves tagging to the
// standard distribution fallback.
func tryPutnamiTagDigest(emit *jsonl.Emitter, registry, imageName, srcRef, contentDigest string, tags []string, keychain authn.Keychain, transport http.RoundTripper) bool {
	ctx := context.Background()
	client, err := newOCITagClient(ctx, srcRef, keychain, transport)
	if err != nil {
		return false
	}
	if !client.supportsTagDigest(ctx) {
		return false
	}
	if err := client.tagDigest(ctx, contentDigest, tags); err != nil {
		emit.Info(fmt.Sprintf("putnami tag-digest fast path failed (%v), falling back to manifest PUTs", err))
		return false
	}
	for _, tag := range tags {
		emit.Info("Tagged: " + BuildRef(registry, imageName, tag))
	}
	emit.Info(fmt.Sprintf("Applied %d refs in one tag-digest call", len(tags)))
	return true
}

// ResolveDigest best-effort resolves the immutable image digest for a ref. It
// returns "" (emitting an info line) when the registry is empty or the lookup
// fails; this never fails the publish.
func ResolveDigest(emit *jsonl.Emitter, registry, versionRef string, keychain authn.Keychain, options ...PushOptions) string {
	if registry == "" {
		return ""
	}
	transport, err := transportOf(options)
	if err != nil {
		emit.Info(fmt.Sprintf("Could not resolve image digest for %s: %v", versionRef, err))
		return ""
	}
	digest, derr := crane.Digest(versionRef, craneOptions(keychain, transport)...)
	if derr != nil {
		emit.Info(fmt.Sprintf("Could not resolve image digest for %s: %v", versionRef, derr))
		return ""
	}
	return digest
}
