package configcli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// metadataIDTokenURL is the GCP metadata endpoint that mints an ID token for the
// workload's attached service account. It is a well-known URL, not a credential.
// It mirrors the remote cache's secretless cache-token path: the config-drift
// check authenticates to the config plane as the runner's Cloud Run SA the same
// way.
//
//nolint:gosec // G101: URL constant, not a credential
const metadataIDTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// metadataResolveTimeout bounds a single metadata-server round trip so a
// misbehaving or absent metadata source cannot hang the drift step.
const metadataResolveTimeout = 2 * time.Second

// maxMetadataTokenBytes caps the token body read from the metadata server.
const maxMetadataTokenBytes = 64 << 10

// metadataIDToken is the seam the drift command mints its Cloud Run SA identity
// token through; overridable in tests so no real metadata server is required.
var metadataIDToken = fetchMetadataIDToken

func fetchMetadataIDToken(ctx context.Context, audience string) (string, error) {
	return fetchMetadataIDTokenFrom(ctx, audience, metadataIDTokenURL, http.DefaultClient)
}

func fetchMetadataIDTokenFrom(ctx context.Context, audience, endpoint string, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataResolveTimeout)
	defer cancel()

	query := url.Values{}
	query.Set("audience", audience)
	query.Set("format", "full")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req) //nolint:gosec // G704: production passes the fixed GCP metadata endpoint; tests pass loopback
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataTokenBytes))
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}
