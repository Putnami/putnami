package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// FetchBase resolves the platform image for a digest-pinned base ref, caching
// it as a self-contained OCI layout under cacheDir so repeat packaging works
// offline. The cache key is the base digest plus platform — content-addressed,
// so entries are immutable and never stale.
func FetchBase(refStr, platformStr, cacheDir string) (v1.Image, error) {
	return FetchBaseWithKeychain(refStr, platformStr, cacheDir, authn.DefaultKeychain)
}

// FetchBaseWithKeychain resolves and caches a digest-pinned base using the
// caller's registry credentials. A nil keychain preserves FetchBase's Docker
// default-keychain behavior.
func FetchBaseWithKeychain(refStr, platformStr, cacheDir string, keychain authn.Keychain) (v1.Image, error) {
	ref, err := ParseDigestReference(refStr)
	if err != nil {
		return nil, fmt.Errorf("parsing base ref: %w", err)
	}
	digestRef := ref
	platform, err := parsePlatform(platformStr)
	if err != nil {
		return nil, err
	}

	cacheKey := strings.TrimPrefix(digestRef.DigestStr(), "sha256:") + "-" + platform.OS + "-" + platform.Architecture
	dir := filepath.Join(cacheDir, cacheKey)
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		return LoadFromLayout(dir)
	}
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}

	desc, err := remote.Get(ref,
		remote.WithAuthFromKeychain(anonymousFallbackKeychain{keychain}),
		remote.WithPlatform(*platform))
	if err != nil {
		return nil, err
	}
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("resolving %s for %s: %w", refStr, platformStr, err)
	}

	// Write the cache entry atomically: a concurrent or interrupted package
	// run must never observe a half-written layout.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(cacheDir, cacheKey+".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if _, err := WriteLayout(tmp, img); err != nil {
		return nil, fmt.Errorf("caching base image: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, "index.json")); statErr != nil {
			return nil, fmt.Errorf("installing base cache entry: %w", err)
		}
		// A concurrent run won the rename; use its (identical) entry.
	}
	return LoadFromLayout(dir)
}

// ParseDigestReference accepts only immutable sha256 OCI references. Package
// lookup calls it before a registry-hit shortcut, so a moving base tag can
// never reuse content assembled from an older resolution of that tag.
func ParseDigestReference(refStr string) (name.Digest, error) {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return name.Digest{}, err
	}
	digestRef, ok := ref.(name.Digest)
	if !ok || !strings.HasPrefix(digestRef.DigestStr(), "sha256:") || len(digestRef.DigestStr()) != len("sha256:")+64 {
		return name.Digest{}, fmt.Errorf("base ref %s is not digest-pinned — a moving tag would make the assembled digest irreproducible", refStr)
	}
	return digestRef, nil
}

// anonymousFallbackKeychain degrades credential-helper failures to anonymous
// access. Public base images must pull on machines whose docker config
// references a helper that is not installed (e.g. docker-credential-gcloud
// configured for gcr.io without the gcloud SDK on PATH).
type anonymousFallbackKeychain struct {
	inner authn.Keychain
}

func (k anonymousFallbackKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	auth, err := k.inner.Resolve(r)
	if err != nil {
		return authn.Anonymous, nil
	}
	return auth, nil
}

func parsePlatform(s string) (*v1.Platform, error) {
	osName, arch, ok := strings.Cut(s, "/")
	if !ok || osName == "" || arch == "" {
		return nil, fmt.Errorf("invalid platform %q (want os/arch)", s)
	}
	return &v1.Platform{OS: osName, Architecture: arch}, nil
}

func parseTag(ref string) (name.Tag, error) {
	tag, err := name.NewTag(ref)
	if err != nil {
		return name.Tag{}, fmt.Errorf("parsing tag ref %q: %w", ref, err)
	}
	return tag, nil
}
