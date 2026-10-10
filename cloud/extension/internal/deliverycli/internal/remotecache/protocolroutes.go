package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The protocol/cache routes in this file have no cache-server operation:
// cache-server serves neither the direct-write grant nor the inline upload
// batch, and never advertises CapabilityDirectCASPut or CapabilityUploadBatch.
// The client calls them only on a server that advertises them, which is a
// third-party server at PUTNAMI_CACHE_URL. Their endpoint and contract belong
// to that server, so they speak protocol/cache over the pooled net/http client
// with the same bearer lifecycle as the generated control calls.

// UploadGrant fetches a direct-write grant (Phase 2 accelerator). It is used
// only when the server advertises CapabilityDirectCASPut; the returned grant
// lets the client PUT blobs straight to CAS without a find-missing round trip.
func (c *Client) UploadGrant(ctx context.Context) (*cache.UploadGrant, error) {
	data, status, err := c.doAuthed(ctx, "upload-grant", c.protocolAttempt("upload-grant", http.MethodGet, cache.UploadGrantPath, nil))
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

// UploadBatch stores several small blobs inline in one request (the analog of
// Bazel's BatchUpdateBlobs). Used only when the server advertises
// CapabilityUploadBatch.
func (c *Client) UploadBatch(ctx context.Context, req *cache.UploadBatchRequest) (*cache.UploadBatchResponse, error) {
	if diags := cache.ValidateUploadBatchRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid upload-batch request: %s", firstError(diags))
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal upload-batch request: %w", err)
	}

	data, status, err := c.doAuthed(ctx, "upload-batch", c.protocolAttempt("upload-batch", http.MethodPost, cache.UploadBatchPath, body))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, statusError("upload-batch", status, data)
	}

	out, diags := cache.ParseAndValidateUploadBatchResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid upload-batch response: %s", firstError(diags))
	}
	return out, nil
}

// protocolAttempt is one bearer-carrying exchange on a protocol/cache route
// that only a third-party server serves. A nil body sends no entity. The body
// is re-read per attempt: a replay must send the same bytes, not an
// already-drained reader.
func (c *Client) protocolAttempt(op, method, path string, body []byte) func(context.Context, *resolvedBearer) ([]byte, int, error) {
	return func(ctx context.Context, cred *resolvedBearer) ([]byte, int, error) {
		var entity io.Reader
		if body != nil {
			entity = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, entity)
		if err != nil {
			return nil, 0, fmt.Errorf("build %s request: %w", op, err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
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
}
