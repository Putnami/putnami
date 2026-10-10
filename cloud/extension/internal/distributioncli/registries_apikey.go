package distributioncli

import (
	"context"
	"net/http"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// revokeAPIKey revokes the key through auth-server's generated client
// (DELETE /apikeys/{id}). 404 is treated as success — the key was already
// gone, so the user's intent (no longer present) is already satisfied.
func revokeAPIKey(client *http.Client, authBaseURL, accessToken, id string) error {
	return clicore.RevokeAPIKey(context.Background(), client, authBaseURL, accessToken, id)
}

// keyStatus is what `registries status` reports per stored KeyRef. The
// auth-server's /introspect requires an OAuth client_id+secret that the
// CLI does not hold; the user-scoped GET /apikeys/{id} carries the same
// active/revoked/expires_at information without needing the introspect
// client credentials, so we use that instead.
type keyStatus struct {
	Active     bool
	RevokedAt  string
	ExpiresAt  string
	LastUsedAt string
	NotFound   bool
}

// getAPIKeyStatus reads the key through auth-server's generated client
// (GET /apikeys/{id}). A 404 reports a key auth-server no longer knows.
func getAPIKeyStatus(ctx context.Context, client *http.Client, authBaseURL, accessToken, id string, now time.Time) (keyStatus, error) {
	key, err := clicore.ReadAPIKey(ctx, client, authBaseURL, accessToken, id)
	if err != nil {
		return keyStatus{}, err
	}
	if key.NotFound {
		return keyStatus{NotFound: true}, nil
	}
	st := keyStatus{
		RevokedAt:  key.RevokedAt,
		ExpiresAt:  key.ExpiresAt,
		LastUsedAt: key.LastUsedAt,
	}
	st.Active = st.RevokedAt == "" && !isExpired(st.ExpiresAt, now)
	return st, nil
}

func isExpired(iso string, now time.Time) bool {
	if iso == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		// Tolerate the non-nano variant the server may return.
		t, err = time.Parse(time.RFC3339, iso)
		if err != nil {
			return false
		}
	}
	return !t.After(now)
}
