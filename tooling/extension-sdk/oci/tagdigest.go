package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	ociproto "go.putnami.dev/protocol/oci"
)

// Putnami OCI registry fast path (protocol/oci tag-digest/v1): assign every
// session ref to a digest in one authenticated, digest-addressed call instead of
// one (GET + PUT) manifest round trip per tag. Capability discovery mirrors the
// remote-cache protocol: probe the capabilities endpoint, take the fast path when
// the registry advertises tag-digest/v1, fall back to the standard distribution
// API anywhere else (gcr, ghcr, …). The wire contract — endpoint paths, the
// capability string, the request body — is owned by protocol/oci.
const (
	ociProbeTimeout = 10 * time.Second
	ociTagTimeout   = 30 * time.Second
)

// ociTagClient talks to a putnami OCI registry's tag-digest endpoint with the
// same credentials and token scope as regular pushes to the repository.
type ociTagClient struct {
	httpClient *http.Client
	baseURL    string
	repoPath   string
}

// newOCITagClient builds a client for the registry that hosts ref. It fails when
// credentials cannot be resolved — callers treat any error as "no fast path" and
// fall back to the distribution API. A nil base uses the default transport; a
// private publication broker supplies its own so the probe, the token
// exchange, and the tag call never reach the registry host directly.
func newOCITagClient(ctx context.Context, refStr string, keychain authn.Keychain, base http.RoundTripper) (*ociTagClient, error) {
	ref, err := name.ParseReference(refStr)
	if err != nil {
		return nil, err
	}
	repo := ref.Context()
	auth, err := keychain.Resolve(repo.Registry)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = http.DefaultTransport
	}
	tr, err := transport.NewWithContext(ctx, repo.Registry, auth, base, []string{repo.Scope(transport.PushScope)})
	if err != nil {
		return nil, err
	}
	return &ociTagClient{
		httpClient: &http.Client{Transport: tr},
		baseURL:    fmt.Sprintf("%s://%s", repo.Registry.Scheme(), repo.RegistryStr()),
		repoPath:   repo.RepositoryStr(),
	}, nil
}

// supportsTagDigest probes the capabilities endpoint. Any failure — 404 on a
// standard registry, network error, malformed body — means no fast path.
func (c *ociTagClient) supportsTagDigest(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, ociProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+ociproto.CapabilitiesPath, nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req) //nolint:gosec // URL host is the user-configured publish registry by design
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var caps ociproto.CapabilitiesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&caps); err != nil {
		return false
	}
	return caps.SupportsTagDigest()
}

// tagDigest assigns tags to digest in one call.
func (c *ociTagClient) tagDigest(ctx context.Context, digest string, tags []string) error {
	ctx, cancel := context.WithTimeout(ctx, ociTagTimeout)
	defer cancel()
	body, err := json.Marshal(ociproto.TagDigestRequest{
		Repository: c.repoPath,
		Digest:     digest,
		Tags:       tags,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+ociproto.TagDigestPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req) //nolint:gosec // URL host is the user-configured publish registry by design
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("tag-digest returned %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}
