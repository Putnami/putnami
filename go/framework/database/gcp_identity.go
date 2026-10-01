package database

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// Native GCP identity for Cloud SQL sockets.
//
// A Cloud SQL connection over the mounted Unix socket authenticates with IAM:
// the user is the workload's service-account email and the password is a
// short-lived OAuth access token. Both come from the GCP metadata server —
// the same Workload Identity path the storage package's GCS backend already
// uses natively. buildPoolConfig wires these defaults only for sockets under
// the /cloudsql mount, where IAM is the only sensible interpretation of an
// absent user/password; every other host shape keeps demanding an explicit
// hook, because a bare Unix socket may legitimately mean peer auth.

// gcpMetadataBase is the metadata-server endpoint for the default service
// account. A var rather than a const so tests can point it at an
// httptest.Server.
var gcpMetadataBase = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default"

const (
	gcpMetadataEmailTimeout = 3 * time.Second
	gcpMetadataTokenTimeout = 5 * time.Second
	gcpGcloudTimeout        = 5 * time.Second
)

// isCloudSQLSocket reports whether host is a socket under the Cloud SQL mount
// directory — the one host shape where IAM auth is implied rather than
// configured.
func isCloudSQLSocket(host string) bool {
	return strings.HasPrefix(host, cloudSQLSocketDir+"/")
}

// gcpIdentityResolver returns the active GCP IAM principal email. It tries the
// metadata server first (Cloud Run / GCE / Cloud Build), then falls back to
// the gcloud CLI so a developer laptop connecting through a Cloud SQL proxy
// socket resolves its user the same way.
func gcpIdentityResolver(ctx context.Context) (string, error) {
	var attempts []string
	email, err := gcpMetadataEmail(ctx)
	if err == nil {
		return email, nil
	}
	attempts = append(attempts, "metadata: "+err.Error())
	email, err = gcloudActiveAccount(ctx)
	if err == nil {
		return email, nil
	}
	attempts = append(attempts, "gcloud: "+err.Error())
	return "", errors.Newf(CodeConnection,
		"cannot auto-resolve user — set user explicitly, run on a workload with the GCP metadata server, or 'gcloud auth login'. Tried: %s",
		strings.Join(attempts, "; "))
}

// gcpAccessTokenFetcher returns a short-lived OAuth access token for the
// active service account from the GCP metadata server, used as the Postgres
// password on each Cloud SQL socket connection.
func gcpAccessTokenFetcher(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gcpMetadataTokenTimeout)
	defer cancel()
	body, err := gcpMetadataGet(cctx, "/token")
	if err != nil {
		return "", err
	}
	var payload struct {
		AccessToken string `json:"access_token"` //nolint:gosec // G117: GCP token-response field, not a hardcoded secret
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errors.Wrapf(err, CodeConnection, "decode metadata token")
	}
	if payload.AccessToken == "" {
		return "", errors.Newf(CodeConnection, "metadata server returned empty access_token")
	}
	return payload.AccessToken, nil
}

func gcpMetadataEmail(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gcpMetadataEmailTimeout)
	defer cancel()
	body, err := gcpMetadataGet(cctx, "/email")
	if err != nil {
		return "", err
	}
	email := strings.TrimSpace(string(body))
	if email == "" {
		return "", errors.Newf(CodeConnection, "metadata server returned empty email")
	}
	return email, nil
}

// gcpMetadataGet fetches a metadata-server path with the mandatory
// Metadata-Flavor header and returns the raw body of a 200 response.
func gcpMetadataGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gcpMetadataBase+path, nil)
	if err != nil {
		return nil, errors.Wrapf(err, CodeConnection, "build metadata request")
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: gcpMetadataBase is the fixed GCP metadata endpoint (test-swappable var), not user-tainted
	if err != nil {
		return nil, errors.Wrapf(err, CodeConnection, "request metadata server")
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Newf(CodeConnection, "metadata server returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, CodeConnection, "read metadata response")
	}
	return body, nil
}

func gcloudActiveAccount(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gcpGcloudTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "gcloud", "config", "get-value", "account").Output()
	if err != nil {
		return "", errors.Wrapf(err, CodeConnection, "run gcloud config get-value account")
	}
	account := strings.TrimSpace(string(out))
	if account == "" || account == "(unset)" {
		return "", errors.Newf(CodeConnection, "no active gcloud account")
	}
	return account, nil
}
