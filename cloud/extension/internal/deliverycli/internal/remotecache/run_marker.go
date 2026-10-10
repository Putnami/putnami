package remotecache

import (
	"context"
	"fmt"
	"net/http"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
	diag "go.putnami.dev/protocol/diagnostic"
)

// BuildRunMarkerRequest constructs the lookup request for a successful
// full-target run marker.
func (c *Client) BuildRunMarkerRequest(workspace, branch string, commands []string, paramsHash string) *cache.RunMarkerRequest {
	return cache.NormalizeRunMarkerRequest(&cache.RunMarkerRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Workspace:       workspace,
		Branch:          branch,
		Commands:        commands,
		ParamsHash:      paramsHash,
		Selection:       cache.RunMarkerSelectionAll,
	})
}

// LookupRunMarker reads the last successful full-target run marker. A server
// that has not implemented the endpoint yet is treated as a marker miss.
func (c *Client) LookupRunMarker(ctx context.Context, req *cache.RunMarkerRequest) (*cache.RunMarkerResponse, error) {
	if diags := cache.ValidateRunMarkerRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid run marker request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "run-marker lookup", req, (*cacheserverclient.CacheClient).CreateV1CacheRunMarkerLookup)
	if IsStatus(err, http.StatusNotFound) {
		return &cache.RunMarkerResponse{ProtocolVersion: cache.ProtocolVersion}, nil
	}
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidateRunMarkerResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid run marker response: %s", firstError(diags))
	}
	return out, nil
}

// BuildPublishRunMarkerRequest constructs the publish request for a successful
// full-target run marker.
func (c *Client) BuildPublishRunMarkerRequest(workspace, branch string, commands []string, paramsHash, sha, observedSHA string) *cache.PublishRunMarkerRequest {
	return cache.NormalizePublishRunMarkerRequest(&cache.PublishRunMarkerRequest{
		ProtocolVersion: cache.ProtocolVersion,
		Workspace:       workspace,
		Branch:          branch,
		Commands:        commands,
		ParamsHash:      paramsHash,
		Selection:       cache.RunMarkerSelectionAll,
		SHA:             sha,
		ObservedSHA:     observedSHA,
	})
}

// PublishRunMarker publishes a successful full-target run marker. A server that
// has not implemented the endpoint yet is treated as an ignored best-effort
// publish.
func (c *Client) PublishRunMarker(ctx context.Context, req *cache.PublishRunMarkerRequest) (*cache.PublishRunMarkerResponse, error) {
	if diags := cache.ValidatePublishRunMarkerRequest(req); diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid publish run marker request: %s", firstError(diags))
	}

	data, err := postControl(ctx, c, "run-marker publish", req, (*cacheserverclient.CacheClient).CreateV1CacheRunMarkerPublish)
	if IsStatus(err, http.StatusNotFound) {
		return &cache.PublishRunMarkerResponse{ProtocolVersion: cache.ProtocolVersion}, nil
	}
	if err != nil {
		return nil, err
	}

	out, diags := cache.ParseAndValidatePublishRunMarkerResponse(data)
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("invalid publish run marker response: %s", firstError(diags))
	}
	return out, nil
}
