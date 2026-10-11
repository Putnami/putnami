package remotecache

import (
	"context"
	"fmt"
	"io"
	"net/http"

	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The protocol/cache route in this file has no cache-server operation:
// cache-server does not serve the direct-write grant and never advertises
// CapabilityDirectCASPut. The client calls it only on a server that advertises
// it, which is a third-party server at PUTNAMI_CACHE_URL. Its endpoint and
// contract belong to that server, so it speaks protocol/cache over the pooled
// net/http client with the same bearer lifecycle as the generated control
// calls. upload-batch is not here: cache-server serves it, so it goes through
// the generated client (batch.go).

// UploadGrant fetches a direct-write grant (Phase 2 accelerator). It is used
// only when the server advertises CapabilityDirectCASPut; the returned grant
// lets the client PUT blobs straight to CAS without a find-missing round trip.
func (c *Client) UploadGrant(ctx context.Context) (*cache.UploadGrant, error) {
	data, status, err := c.doAuthed(ctx, "upload-grant", c.uploadGrantAttempt)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("upload-grant", status, data)
	}

	g, diags := cache.ParseAndValidateUploadGrant(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid upload grant: %s", firstError(diags))
	}
	return g, nil
}

// uploadGrantAttempt is one bearer-carrying GET of the direct-write grant, a
// route only a third-party server serves.
func (c *Client) uploadGrantAttempt(ctx context.Context, cred *resolvedBearer) ([]byte, int, error) {
	const op = "upload-grant"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+cache.UploadGrantPath, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s request: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	setUserAgent(req)
	if cred.Token != "" {
		req.Header.Set(cache.AuthorizationHeader, cache.AuthorizationValue(cred.Token))
	}

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: request targets the configured remote-cache endpoint (c.baseURL), not user-tainted input
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s response: %w", op, err)
	}
	if len(data) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("read %s response: response exceeds %d bytes", op, maxResponseBytes)
	}
	return data, resp.StatusCode, nil
}
