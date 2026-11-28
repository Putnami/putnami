package remotecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
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
	if r == nil || !r.Trust.AllowsRemoteRunMarkers() || strings.TrimSpace(workspaceID) == "" {
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
//
// It returns the witness of the scope the provider named at initialize when
// the provider settled the marker of sha: it published it, answered that the
// marker in effect is sha's, or refused it as read-only. Any other answer, a
// provider that names no scope (cache.CapabilityMarkerScope) included, returns
// nil, and the next run at the same commit publishes again (tooling/cli ADR
// 0138).
func (r *RemoteCache) PublishRunMarker(ctx context.Context, workspaceID, branch string, commands []string, paramsHash, sha, observedSHA string) *cacheprovider.ScopeWitness {
	if r == nil || strings.TrimSpace(workspaceID) == "" {
		return nil
	}
	sess, ok := r.ensureProvider(ctx, nil, nil)
	if !ok {
		return nil
	}
	if sess.ReadOnly() {
		return sess.ScopeWitness()
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
		return nil
	}
	if resp == nil {
		return nil
	}
	if !resp.Published {
		slog.Debug("remote cache: provider run marker publish skipped", "reason", "marker not advanced")
	}
	// A write that did not publish settles the marker only when the marker in
	// effect is this commit's: any other answer may be a failure.
	if !resp.Published && (resp.Marker == nil || resp.Marker.SHA != sha) {
		return nil
	}
	return sess.ScopeWitness()
}

// ProviderIdentity names the provider that would answer the run marker,
// without starting it: its extension and the runtime it runs. It is empty for
// a cache no provider backs.
func (r *RemoteCache) ProviderIdentity() string {
	if !r.IsProviderBacked() {
		return ""
	}
	return r.provider.extensionName + "\x00" + r.provider.extensionVersion + "\x00" + r.provider.runtime
}
