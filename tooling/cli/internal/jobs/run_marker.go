package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// RunMarkerWorkspaceID returns an opaque, stable identity for remote run
// markers. Origin URL is preferred because it survives fresh CI checkouts; the
// raw URL is never sent to the cache server.
func RunMarkerWorkspaceID(ws *workspace.Workspace) string {
	if ws == nil {
		return ""
	}
	seed := git.RemoteURL(ws.Root, "origin")
	if seed == "" {
		seed = ws.Root
	}
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LookupRunMarker reads the remote successful-run marker. Failures are
// best-effort and reported as a miss so marker lookup can never break a build.
func (r *RemoteCache) LookupRunMarker(ctx context.Context, workspaceID, branch string, commands []string, paramsHash string) (string, bool) {
	if r == nil || !r.trust.AllowsRemoteRunMarkers() || strings.TrimSpace(workspaceID) == "" {
		return "", false
	}
	sess, ok := r.ensureProvider(ctx, nil, nil)
	if !ok {
		return "", false
	}
	resp, err := sess.LookupMarker(ctx, &cache.MarkerLookupParams{
		Workspace:  workspaceID,
		Branch:     branch,
		Commands:   commands,
		ParamsHash: paramsHash,
		Selection:  cache.RunMarkerSelectionAll,
	})
	if err != nil {
		slog.Debug("remote cache: provider run marker lookup failed; using local selection", "error", err)
		return "", false
	}
	if resp == nil || !resp.Found || resp.Marker == nil || strings.TrimSpace(resp.Marker.SHA) == "" {
		return "", false
	}
	return strings.TrimSpace(resp.Marker.SHA), true
}

// PublishRunMarker publishes the remote successful-run marker. It is
// best-effort; errors and benign race losses never fail the completed build.
func (r *RemoteCache) PublishRunMarker(ctx context.Context, workspaceID, branch string, commands []string, paramsHash, sha, observedSHA string) {
	if r == nil || strings.TrimSpace(workspaceID) == "" {
		return
	}
	sess, ok := r.ensureProvider(ctx, nil, nil)
	if !ok {
		return
	}
	resp, err := sess.WriteMarker(ctx, &cache.MarkerWriteParams{
		Workspace:   workspaceID,
		Branch:      branch,
		Commands:    commands,
		ParamsHash:  paramsHash,
		Selection:   cache.RunMarkerSelectionAll,
		SHA:         sha,
		ObservedSHA: observedSHA,
	})
	if err != nil {
		slog.Debug("remote cache: provider run marker publish failed", "error", err)
		return
	}
	if resp != nil && !resp.Published {
		slog.Debug("remote cache: provider run marker publish skipped", "reason", "marker not advanced")
	}
}
