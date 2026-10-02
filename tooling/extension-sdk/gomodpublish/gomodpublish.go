// Package gomodpublish uploads one Go module version to a registry that
// implements the gomod-write protocol (go.putnami.dev/protocol/gomod), and
// reads the standard Go proxy projections back to prove the registry serves
// the uploaded bytes.
//
// Every function takes the bearer as an argument and attaches it only to
// requests for the registry URL it was given. No function reads a credential
// from the environment or starts a process. No error or report carries the
// bearer or a registry response body.
package gomodpublish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/mod/module"

	gomod "go.putnami.dev/protocol/gomod"
)

// MaxResponseBytes bounds a blob-upload or version-PUT response body.
const MaxResponseBytes = 1 << 20

// MaxZipBytes is the largest module zip the Go toolchain accepts. A served zip
// is hashed up to one byte past it, so a longer body never matches a digest.
const MaxZipBytes = 500 << 20

// Reporter receives the diagnostics and log lines of an upload. A
// *jsonl.Emitter satisfies it. A nil Reporter discards them.
type Reporter interface {
	Diagnostic(severity, message, file string, line int)
	Log(level, message string)
}

type discard struct{}

func (discard) Diagnostic(string, string, string, int) {}
func (discard) Log(string, string)                     {}

func reporterOrDiscard(reporter Reporter) Reporter {
	if reporter == nil {
		return discard{}
	}
	return reporter
}

// Module is one packed Go module version.
type Module struct {
	// Path is the module path.
	Path string
	// Version is the module version.
	Version string
	// Zip is the module zip.
	Zip []byte
	// Mod is the go.mod the version PUT submits.
	Mod []byte
}

// Digest returns "sha256:" and the lowercase hex SHA-256 of data.
func Digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// ValidateRegistryURL returns raw as an absolute HTTPS URL, or an HTTP URL on a
// loopback host, with no trailing slash. A URL that carries userinfo, a query
// or a fragment is refused, so no credential travels in a URL.
func ValidateRegistryURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("go registry URL must be an absolute HTTP(S) URL without credentials")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("go registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "https":
	case "http":
		hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(hostname)
		loopback := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
		if !loopback {
			return "", fmt.Errorf("go registry URL must use HTTPS (HTTP is allowed only for loopback)")
		}
	default:
		return "", fmt.Errorf("go registry URL must use HTTP(S)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// NewHTTPClient returns the client a module publication sends through: no
// proxy, so the bearer has no second recipient, no redirect, and a five-minute
// timeout.
func NewHTTPClient() (*http.Client, error) {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("go registry transport is unavailable")
	}
	transport := baseTransport.Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     transport,
		CheckRedirect: refuseRedirect,
	}, nil
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// withoutRedirects returns a copy of client that never follows a redirect, so
// a request carrying the bearer reaches the registry it names and no other.
func withoutRedirects(client *http.Client) (*http.Client, error) {
	if client == nil {
		return nil, errors.New("go module publication requires an HTTP client")
	}
	bounded := *client
	bounded.CheckRedirect = refuseRedirect
	return &bounded, nil
}

func authorize(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// UploadZip uploads zip to the blob endpoint of modulePath and returns the
// blob digest, which equals the digest of zip. A registry that answers another
// digest is refused.
func UploadZip(ctx context.Context, client *http.Client, reporter Reporter, registryURL, token, modulePath string, zip []byte) (string, error) {
	reporter = reporterOrDiscard(reporter)
	client, err := withoutRedirects(client)
	if err != nil {
		return "", err
	}
	localDigest := Digest(zip)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registryURL+gomod.BlobUploadPath(modulePath), bytes.NewReader(zip))
	if err != nil {
		reporter.Diagnostic("error", "Failed to create blob upload request: "+err.Error(), "", 0)
		return "", err
	}
	req.Header.Set("Content-Type", gomod.BlobContentType)
	authorize(req, token)

	resp, err := client.Do(req) //nolint:gosec // registryURL is validated publish configuration
	if err != nil {
		reporter.Diagnostic("error", "Blob upload failed: "+err.Error(), "", 0)
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readBoundedResponse(resp.Body)
	if err != nil {
		reporter.Diagnostic("error", "Invalid blob upload response: "+err.Error(), "", 0)
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		reporter.Diagnostic("error", fmt.Sprintf("Blob upload returned %d", resp.StatusCode), "", 0)
		return "", fmt.Errorf("blob upload returned %d", resp.StatusCode)
	}

	result, diagnostics := gomod.ParseAndValidateBlobUploadResponse(body)
	if result == nil || len(diagnostics) > 0 {
		// The response is untrusted and can reflect the Authorization value in
		// its digest field, so its protocol diagnostics stay out of the error
		// and the report.
		err := fmt.Errorf("invalid blob upload response")
		reporter.Diagnostic("error", err.Error(), "", 0)
		return "", err
	}
	if result.Digest != localDigest {
		err := fmt.Errorf("registry blob digest does not match uploaded zip digest")
		reporter.Diagnostic("error", err.Error(), "", 0)
		return "", err
	}
	reporter.Log("info", fmt.Sprintf("Uploaded blob (%d bytes)", len(zip)))
	return result.Digest, nil
}

// PublishVersion sends the version PUT for modulePath@version, with no dist
// tag, and reports whether the registry already held that version. A 409 is a
// reuse only when allowExisting is set, and only VerifyZip and VerifyMod prove
// the existing version holds the same bytes.
func PublishVersion(ctx context.Context, client *http.Client, reporter Reporter, registryURL, token, modulePath, version string, goMod []byte, zipDigest string, allowExisting bool) (reused bool, err error) {
	reporter = reporterOrDiscard(reporter)
	client, err = withoutRedirects(client)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(gomod.PublishVersionRequest{GoMod: string(goMod), ZipDigest: zipDigest})
	if err != nil {
		return false, fmt.Errorf("encode publish request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, registryURL+gomod.VersionPath(modulePath, version), bytes.NewReader(payload))
	if err != nil {
		reporter.Diagnostic("error", "Failed to create publish request: "+err.Error(), "", 0)
		return false, err
	}
	req.Header.Set("Content-Type", gomod.VersionContentType)
	authorize(req, token)

	resp, err := client.Do(req) //nolint:gosec // registryURL is validated publish configuration
	if err != nil {
		reporter.Diagnostic("error", "Publish request failed: "+err.Error(), "", 0)
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := readBoundedResponse(resp.Body); err != nil {
		reporter.Diagnostic("error", "Invalid publish response: "+err.Error(), "", 0)
		return false, err
	}
	if resp.StatusCode != http.StatusCreated && (!allowExisting || resp.StatusCode != http.StatusConflict) {
		reporter.Diagnostic("error", fmt.Sprintf("Publish returned %d", resp.StatusCode), "", 0)
		return false, fmt.Errorf("publish returned %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusConflict {
		reporter.Log("info", fmt.Sprintf("Module %s@%s already exists; verifying immutable registry bytes", modulePath, version))
		return true, nil
	}
	reporter.Log("info", fmt.Sprintf("Published %s@%s", modulePath, version))
	return false, nil
}

func readBoundedResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		return nil, fmt.Errorf("registry response exceeds %d bytes", MaxResponseBytes)
	}
	return data, nil
}

// proxyURL returns the standard Go proxy URL of modulePath@version's file with
// the given extension.
func proxyURL(registryURL, modulePath, version, extension string) (string, error) {
	escapedPath, err := module.EscapePath(modulePath)
	if err != nil {
		return "", fmt.Errorf("escape module path: %w", err)
	}
	escapedVersion, err := module.EscapeVersion(version)
	if err != nil {
		return "", fmt.Errorf("escape module version: %w", err)
	}
	return registryURL + "/" + escapedPath + "/@v/" + escapedVersion + extension, nil
}

// download GETs a standard Go proxy projection and returns the open response
// when it answers 200.
func download(ctx context.Context, client *http.Client, target, token, name string) (*http.Response, error) {
	client, err := withoutRedirects(client)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	authorize(req, token)
	resp, err := client.Do(req) //nolint:gosec // the URL is built from validated publish configuration
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s download returned %d", name, resp.StatusCode)
	}
	return resp, nil
}

// VerifyZip downloads the module zip the registry serves to consumers and
// refuses it unless its digest is wantDigest. A blob-upload answer alone is
// no proof: the version could point at other bytes.
func VerifyZip(ctx context.Context, client *http.Client, registryURL, token, modulePath, version, wantDigest string) error {
	target, err := proxyURL(registryURL, modulePath, version, ".zip")
	if err != nil {
		return err
	}
	resp, err := download(ctx, client, target, token, "module zip")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(resp.Body, MaxZipBytes+1)); err != nil {
		return fmt.Errorf("hash downloaded module zip: %w", err)
	}
	gotDigest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if gotDigest != wantDigest {
		return fmt.Errorf("downloaded module zip digest %q does not match uploaded digest %q", gotDigest, wantDigest)
	}
	return nil
}

// VerifyMod downloads the go.mod the registry serves to consumers and refuses
// it unless it equals want byte for byte. The zip digest does not prove this
// separately served file.
func VerifyMod(ctx context.Context, client *http.Client, registryURL, token, modulePath, version string, want []byte) error {
	target, err := proxyURL(registryURL, modulePath, version, ".mod")
	if err != nil {
		return err
	}
	resp, err := download(ctx, client, target, token, "go.mod")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(want))+1))
	if err != nil {
		return fmt.Errorf("read downloaded go.mod: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("downloaded go.mod does not match submitted bytes")
	}
	return nil
}

// Publish uploads mod's zip, sends its version PUT, and reads the served zip
// and go.mod back. It returns the zip digest and whether the registry already
// held the version. An existing version is a reuse only when the registry
// serves the same zip and go.mod; any other bytes are an error.
func Publish(ctx context.Context, client *http.Client, reporter Reporter, registryURL, token string, mod Module) (zipDigest string, reused bool, err error) {
	zipDigest, err = UploadZip(ctx, client, reporter, registryURL, token, mod.Path, mod.Zip)
	if err != nil {
		return "", false, fmt.Errorf("upload module zip: %w", err)
	}
	reused, err = PublishVersion(ctx, client, reporter, registryURL, token, mod.Path, mod.Version, mod.Mod, zipDigest, true)
	if err != nil {
		return "", false, fmt.Errorf("publish version: %w", err)
	}
	if err := VerifyZip(ctx, client, registryURL, token, mod.Path, mod.Version, zipDigest); err != nil {
		return "", false, fmt.Errorf("verify published module zip: %w", err)
	}
	if err := VerifyMod(ctx, client, registryURL, token, mod.Path, mod.Version, mod.Mod); err != nil {
		return "", false, fmt.Errorf("verify published go.mod: %w", err)
	}
	return zipDigest, reused, nil
}
