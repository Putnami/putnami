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
	"go.putnami.dev/sdk/extension/internal/artifacterr"
	"go.putnami.dev/sdk/extension/internal/retry"
)

// MaxResponseBytes bounds a blob-upload or version-PUT response body.
const MaxResponseBytes = 1 << 20

// MaxZipBytes is the largest module zip the Go toolchain accepts. A served zip
// is hashed up to one byte past it, so a longer body never matches a digest.
const MaxZipBytes = 500 << 20

// retryPolicy is how a blob upload, a version PUT that may find the version
// stored, and a verification read retry a transient failure.
var retryPolicy = retry.Default()

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
// digest is refused. The blob endpoint is content-addressed, so a transient
// failure (package retry) sends the same zip again, up to retry.Attempts
// times in all.
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

	sent := sendRetrying(ctx, retryPolicy, client, req, zip)
	if sent.err != nil {
		if sent.unreadable {
			reporter.Diagnostic("error", "Invalid blob upload response"+retry.After(sent.attempts)+": "+sent.err.Error(), "", 0)
		} else {
			reporter.Diagnostic("error", "Blob upload failed"+retry.After(sent.attempts)+": "+sent.err.Error(), "", 0)
		}
		return "", sent.failure("blob upload failed")
	}
	if status := sent.answer.Status; status != http.StatusCreated {
		note := retry.Note(sent.answer, sent.attempts)
		reporter.Diagnostic("error", fmt.Sprintf("Blob upload returned %d%s", status, note), "", 0)
		return "", fmt.Errorf("blob upload returned %d%s", status, note)
	}

	result, diagnostics := gomod.ParseAndValidateBlobUploadResponse(sent.body)
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
// the existing version holds the same bytes. With allowExisting set, a
// transient failure (package retry) sends the PUT again, up to retry.Attempts
// times in all, and a version a lost attempt stored answers 409. Without it,
// the PUT is sent once.
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

	policy := retryPolicy
	if !allowExisting {
		policy.Attempts = 1
	}
	sent := sendRetrying(ctx, policy, client, req, payload)
	if sent.err != nil {
		if sent.unreadable {
			reporter.Diagnostic("error", "Invalid publish response"+retry.After(sent.attempts)+": "+sent.err.Error(), "", 0)
		} else {
			reporter.Diagnostic("error", "Publish request failed"+retry.After(sent.attempts)+": "+sent.err.Error(), "", 0)
		}
		return false, sent.failure("publish failed")
	}
	status := sent.answer.Status
	if status != http.StatusCreated && (!allowExisting || status != http.StatusConflict) {
		note := retry.Note(sent.answer, sent.attempts)
		reporter.Diagnostic("error", fmt.Sprintf("Publish returned %d%s", status, note), "", 0)
		return false, fmt.Errorf("publish returned %d%s", status, note)
	}
	if status == http.StatusConflict {
		reporter.Log("info", fmt.Sprintf("Module %s@%s already exists; verifying immutable registry bytes", modulePath, version))
		return true, nil
	}
	reporter.Log("info", fmt.Sprintf("Published %s@%s", modulePath, version))
	return false, nil
}

// errResponseTooLarge is a registry answer above MaxResponseBytes.
var errResponseTooLarge = fmt.Errorf("registry response exceeds %d bytes", MaxResponseBytes)

func readBoundedResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		return nil, errResponseTooLarge
	}
	return data, nil
}

// outcome is what sendRetrying observed.
type outcome struct {
	// answer is the last answer, for retry.Note.
	answer retry.Answer
	// body is the bounded body of the last answer.
	body []byte
	// attempts is the number of attempts made.
	attempts int
	// unreadable reports that the last answer could not be read.
	unreadable bool
	// err is the last error.
	err error
}

// failure is the error of a request that ended with err: err itself after
// one attempt, and what, the attempt count and err after more.
func (o outcome) failure(what string) error {
	if o.attempts > 1 {
		return fmt.Errorf("%s%s: %w", what, retry.After(o.attempts), o.err)
	}
	return o.err
}

// sendRetrying sends req, carrying body, under policy, and reads the bounded
// answer of each attempt. An answer above MaxResponseBytes is final.
func sendRetrying(ctx context.Context, policy retry.Policy, client *http.Client, req *http.Request, body []byte) outcome {
	var result outcome
	result.answer, result.attempts, result.err = policy.Do(ctx, func() (retry.Answer, error) {
		watched, exchange := retry.Watch(ctx)
		attempt := req.Clone(watched)
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		resp, err := client.Do(attempt) //nolint:gosec // registryURL is validated publish configuration
		if err != nil {
			result.unreadable = false
			return retry.Answer{}, exchange.Failed(err, err)
		}
		defer func() { _ = resp.Body.Close() }()
		result.body, err = readBoundedResponse(resp.Body)
		result.unreadable = err != nil
		switch {
		case errors.Is(err, errResponseTooLarge):
			return retry.Answer{}, err
		case err != nil:
			return retry.Answer{}, exchange.Failed(err, err)
		}
		return retry.Answer{Status: resp.StatusCode, Header: resp.Header}, nil
	})
	return result
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

// download GETs a standard Go proxy projection and hands the body of a 200
// answer to consume. A transient failure (package retry), a failure to read
// that body included, sends the GET again, up to retry.Attempts times in
// all. Any other error consume returns is final.
func download(ctx context.Context, client *http.Client, target, token, name string, consume func(io.Reader) error) error {
	client, err := withoutRedirects(client)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	authorize(req, token)
	answer, attempts, err := retryPolicy.Do(ctx, func() (retry.Answer, error) {
		watched, exchange := retry.Watch(ctx)
		resp, err := client.Do(req.Clone(watched)) //nolint:gosec // the URL is built from validated publish configuration
		if err != nil {
			return retry.Answer{}, exchange.Failed(err, err)
		}
		defer func() { _ = resp.Body.Close() }()
		answer := retry.Answer{Status: resp.StatusCode, Header: resp.Header}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
			return answer, nil
		}
		body := &readRecorder{reader: resp.Body}
		if err := consume(body); err != nil {
			if body.err != nil {
				return answer, exchange.Failed(body.err, err)
			}
			return answer, err
		}
		return answer, nil
	})
	if err != nil {
		return outcome{attempts: attempts, err: err}.failure(name + " download failed")
	}
	if answer.Status != http.StatusOK {
		return fmt.Errorf("%s download returned %d%s", name, answer.Status, retry.Note(answer, attempts))
	}
	return nil
}

// readRecorder reads from reader and keeps the first error other than io.EOF
// that reader returned.
type readRecorder struct {
	reader io.Reader
	err    error
}

func (r *readRecorder) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// VerifyZip downloads the module zip the registry serves to consumers and
// refuses it unless its digest is wantDigest. A blob-upload answer alone is
// no proof: the version could point at other bytes. A zip of another digest
// is refused with an error marked artifacterr.ErrConflict.
func VerifyZip(ctx context.Context, client *http.Client, registryURL, token, modulePath, version, wantDigest string) error {
	target, err := proxyURL(registryURL, modulePath, version, ".zip")
	if err != nil {
		return err
	}
	return download(ctx, client, target, token, "module zip", func(body io.Reader) error {
		hash := sha256.New()
		if _, err := io.Copy(hash, io.LimitReader(body, MaxZipBytes+1)); err != nil {
			return fmt.Errorf("hash downloaded module zip: %w", err)
		}
		gotDigest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
		if gotDigest != wantDigest {
			return artifacterr.Conflict(fmt.Errorf("downloaded module zip digest %q does not match uploaded digest %q", gotDigest, wantDigest))
		}
		return nil
	})
}

// VerifyMod downloads the go.mod the registry serves to consumers and refuses
// it unless it equals want byte for byte. The zip digest does not prove this
// separately served file. Other bytes are refused with an error marked
// artifacterr.ErrConflict.
func VerifyMod(ctx context.Context, client *http.Client, registryURL, token, modulePath, version string, want []byte) error {
	target, err := proxyURL(registryURL, modulePath, version, ".mod")
	if err != nil {
		return err
	}
	return download(ctx, client, target, token, "go.mod", func(body io.Reader) error {
		got, err := io.ReadAll(io.LimitReader(body, int64(len(want))+1))
		if err != nil {
			return fmt.Errorf("read downloaded go.mod: %w", err)
		}
		if !bytes.Equal(got, want) {
			return artifacterr.Conflict(fmt.Errorf("downloaded go.mod does not match submitted bytes"))
		}
		return nil
	})
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
