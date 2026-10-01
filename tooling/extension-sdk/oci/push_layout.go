package oci

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// LayoutTarget names where PushLayout publishes an OCI layout and the manifest
// the layout must hold.
type LayoutTarget struct {
	// Repository is the registry host and the repository path, such as
	// "oci.example.dev/team/app". The host is explicit.
	Repository string
	// Digest is the expected manifest digest: "sha256:" and 64 lowercase hex
	// characters.
	Digest string
	// Tags are assigned to Digest in Repository once the manifest is there.
	Tags []string
}

// PushedLayout is what PushLayout published.
type PushedLayout struct {
	// Digest is the manifest digest the registry holds. It always equals the
	// target digest.
	Digest string
	// Reused reports that the registry already held the manifest, so no blob
	// and no manifest was uploaded.
	Reused bool
}

var (
	layoutDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// layoutTagPattern is the OCI distribution tag grammar.
	layoutTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// PushLayout publishes the image of the OCI layout at layoutDir whose manifest
// digest is target.Digest to target.Repository, then assigns target.Tags to it.
//
// Before any request it refuses a layout that holds a symbolic link or another
// non-regular entry, a layout that does not index target.Digest, and a manifest
// whose bytes do not hash to target.Digest. The manifest bytes it hashes are the
// bytes it uploads. When the registry already holds the digest, nothing is
// uploaded and Reused is set.
//
// bearer is sent only to the registry host of target.Repository. No request
// goes through an ambient proxy unless options supplies a transport, and no
// error carries the bearer. An empty bearer sends anonymous requests.
func PushLayout(ctx context.Context, layoutDir string, target LayoutTarget, bearer string, options ...PushOptions) (PushedLayout, error) {
	pushed, err := pushLayout(ctx, layoutDir, target, bearer, options)
	if err != nil {
		return PushedLayout{}, redactBearer(err, bearer)
	}
	return pushed, nil
}

func pushLayout(ctx context.Context, layoutDir string, target LayoutTarget, bearer string, options []PushOptions) (PushedLayout, error) {
	transport, err := transportOf(options)
	if err != nil {
		return PushedLayout{}, err
	}
	if transport == nil {
		if transport, err = directTransport(); err != nil {
			return PushedLayout{}, err
		}
	}
	repo, err := name.NewRepository(target.Repository, name.StrictValidation)
	if err != nil || repo.Name() != target.Repository {
		return PushedLayout{}, fmt.Errorf("OCI repository %q is not a canonical repository with an explicit registry host", target.Repository)
	}
	if !layoutDigestPattern.MatchString(target.Digest) {
		return PushedLayout{}, fmt.Errorf("expected manifest digest %q is not sha256: followed by 64 lowercase hex characters", target.Digest)
	}
	want, err := v1.NewHash(target.Digest)
	if err != nil {
		return PushedLayout{}, fmt.Errorf("expected manifest digest: %w", err)
	}
	tags, err := layoutTags(repo, target.Tags)
	if err != nil {
		return PushedLayout{}, err
	}

	if err := checkLayoutTree(layoutDir); err != nil {
		return PushedLayout{}, err
	}
	index, err := layout.ImageIndexFromPath(layoutDir)
	if err != nil {
		return PushedLayout{}, fmt.Errorf("read OCI layout: %w", err)
	}
	img, err := index.Image(want)
	if err != nil {
		return PushedLayout{}, fmt.Errorf("OCI layout does not hold manifest %s: %w", want, err)
	}
	mediaType, err := img.MediaType()
	if err != nil {
		return PushedLayout{}, fmt.Errorf("read layout manifest media type: %w", err)
	}
	if !mediaType.IsImage() {
		return PushedLayout{}, fmt.Errorf("layout manifest %s has media type %q, not an image manifest", want, mediaType)
	}
	got, err := img.Digest()
	if err != nil {
		return PushedLayout{}, fmt.Errorf("hash layout manifest: %w", err)
	}
	if got != want {
		return PushedLayout{}, fmt.Errorf("layout manifest digest %s does not match expected digest %s", got, want)
	}

	keychain := bearerKeychain{host: repo.RegistryStr(), token: bearer}
	ref := repo.Digest(want.String())
	remoteOptions := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain), remote.WithTransport(transport)}

	reused := false
	if descriptor, err := remote.Head(ref, remoteOptions...); err == nil && descriptor.Digest == want {
		reused = true
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return PushedLayout{}, ctxErr
	}
	if !reused {
		pushed, err := pushLayoutImage(ctx, ref, img, keychain, transport)
		if err != nil {
			return PushedLayout{}, fmt.Errorf("push %s: %w", ref, err)
		}
		if pushed != want.String() {
			return PushedLayout{}, fmt.Errorf("pushed manifest digest %s does not match expected digest %s", pushed, want)
		}
	}

	if len(tags) > 0 && !tagByDigest(ctx, ref, want.String(), tags, keychain, transport) {
		for _, tag := range tags {
			if err := remote.Tag(repo.Tag(tag), img, remoteOptions...); err != nil {
				return PushedLayout{}, fmt.Errorf("tag %s as %s: %w", ref, tag, err)
			}
		}
	}
	return PushedLayout{Digest: want.String(), Reused: reused}, nil
}

// layoutTags returns tags without duplicates, refusing one that is not a valid
// tag of repo.
func layoutTags(repo name.Repository, tags []string) ([]string, error) {
	unique := UniqueStrings(tags)
	for _, tag := range unique {
		if _, err := name.NewTag(repo.Name()+":"+tag, name.StrictValidation); err != nil || !layoutTagPattern.MatchString(tag) {
			return nil, fmt.Errorf("OCI tag %q is invalid", tag)
		}
	}
	return unique, nil
}

// tagByDigest assigns tags through the Putnami registry's tag-digest call and
// reports whether it did. Any failure leaves tagging to manifest PUTs.
func tagByDigest(ctx context.Context, ref name.Digest, digest string, tags []string, keychain bearerKeychain, transport http.RoundTripper) bool {
	client, err := newOCITagClient(ctx, ref.String(), keychain, transport)
	if err != nil || !client.supportsTagDigest(ctx) {
		return false
	}
	return client.tagDigest(ctx, digest, tags) == nil
}

// checkLayoutTree refuses a layout directory that is itself a symbolic link or
// holds an entry other than a directory or a regular file, so every byte read
// from the layout comes from inside it.
func checkLayoutTree(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("read OCI layout: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("OCI layout %s is not a directory", dir)
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("read OCI layout: %w", err)
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		return fmt.Errorf("OCI layout entry %s is not a regular file or a directory", filepath.ToSlash(rel))
	})
}

// directTransport returns the default transport without a proxy, so the bearer
// has no second recipient.
func directTransport() (http.RoundTripper, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("OCI registry transport is unavailable")
	}
	transport := base.Clone()
	transport.Proxy = nil
	return transport, nil
}

var bearerCredential = regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`)

// redactedError carries a message with every bearer value removed. It unwraps
// only to a context error, whose text holds no credential.
type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// redactBearer returns err with bearer, its query escaping and every
// "Bearer <value>" removed from its text. A registry error can echo the
// request's Authorization header, so the original error is not kept.
func redactBearer(err error, bearer string) error {
	message := err.Error()
	if bearer != "" {
		message = strings.ReplaceAll(message, bearer, "[redacted]")
		if escaped := url.QueryEscape(bearer); escaped != bearer {
			message = strings.ReplaceAll(message, escaped, "[redacted]")
		}
	}
	message = bearerCredential.ReplaceAllString(message, "Bearer [redacted]")
	var cause error
	switch {
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	}
	return &redactedError{message: message, cause: cause}
}
