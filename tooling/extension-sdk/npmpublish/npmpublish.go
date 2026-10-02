// Package npmpublish uploads one npm package version to a managed registry
// with the registry's native publish document, and reads the published
// tarball back to prove the registry serves the uploaded bytes.
//
// Every function takes the bearer as an argument and attaches it only to
// requests for the registry it was given. No function reads a credential from
// the environment or starts a process, and no error carries the bearer.
package npmpublish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ErrorExcerptLimit bounds, in characters, how much of a registry error body
// an error carries.
const ErrorExcerptLimit = 300

// errorReadLimit bounds how much of an error body is read. It is far above
// ErrorExcerptLimit, so a credential cut by the read bound always lies past
// the excerpt and never shows partially.
const errorReadLimit = 4096

// Attachment is the tarball attachment of a publish document.
type Attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int    `json:"length"`
}

// Payload is the publish document the PUT carries: one version, one
// attachment, and no dist-tag.
type Payload struct {
	Name        string                     `json:"name"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]Attachment      `json:"_attachments"`
	DistTags    map[string]string          `json:"dist-tags"`
}

// Artifact is one packed npm version.
type Artifact struct {
	// Name is the package name, scoped or not.
	Name string
	// Version is the package version.
	Version string
	// Manifest is the staged package.json. Its name and version equal Name and
	// Version.
	Manifest []byte
	// Tarball is the packed archive.
	Tarball []byte
}

// Digest returns "sha256:" and the lowercase hex SHA-256 of data.
func Digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// BuildPayload returns the publish document for artifact. The manifest's
// publishConfig is removed: it is npm-client authority, not package identity.
// The dist-tags map is empty.
func BuildPayload(artifact Artifact) (Payload, error) {
	var wireManifest map[string]json.RawMessage
	if err := json.Unmarshal(artifact.Manifest, &wireManifest); err != nil {
		return Payload{}, fmt.Errorf("parse managed npm manifest: %w", err)
	}
	delete(wireManifest, "publishConfig")
	manifest, err := json.Marshal(wireManifest)
	if err != nil {
		return Payload{}, fmt.Errorf("encode managed npm manifest: %w", err)
	}
	attachmentName := artifact.Name
	if slash := strings.LastIndexByte(attachmentName, '/'); slash >= 0 {
		attachmentName = attachmentName[slash+1:]
	}
	attachmentName += "-" + artifact.Version + ".tgz"
	return Payload{
		Name:     artifact.Name,
		Versions: map[string]json.RawMessage{artifact.Version: manifest},
		Attachments: map[string]Attachment{
			attachmentName: {
				ContentType: "application/octet-stream",
				Data:        base64.StdEncoding.EncodeToString(artifact.Tarball),
				Length:      len(artifact.Tarball),
			},
		},
		DistTags: map[string]string{},
	}, nil
}

// NewHTTPClient returns the client a managed publication sends through: no
// proxy, so the bearer has no second recipient, no redirect, and a five-minute
// timeout.
func NewHTTPClient() (*http.Client, error) {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("managed npm transport is unavailable")
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
		return nil, errors.New("managed npm publication requires an HTTP client")
	}
	bounded := *client
	bounded.CheckRedirect = refuseRedirect
	return &bounded, nil
}

// Put sends payload to registry with token as its bearer. Any status outside
// 2xx is an error carrying a bounded, redacted excerpt of the response body.
func Put(ctx context.Context, client *http.Client, registry, token string, payload Payload) error {
	client, err := withoutRedirects(client)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode managed npm payload: %w", err)
	}
	endpoint := strings.TrimRight(registry, "/") + "/" + url.PathEscape(payload.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build managed npm request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) //nolint:gosec // registry is an explicit, validated managed publish target
	if err != nil {
		return fmt.Errorf("send managed npm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("registry returned %s%s", resp.Status, ErrorExcerpt(resp.Body, token))
	}
	return nil
}

// Probe downloads the tarball registry serves for packageName@version with
// token as its bearer. It reports false when the registry answers 404, true
// when the bytes have exactly wantSize bytes and wantDigest, and an error
// otherwise: a registry that serves other bytes holds an immutable version
// this artifact cannot replace.
func Probe(ctx context.Context, client *http.Client, registry, token, packageName, version, wantDigest string, wantSize int64) (bool, error) {
	client, err := withoutRedirects(client)
	if err != nil {
		return false, err
	}
	tarballURL, err := TarballURL(registry, packageName, version)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tarballURL, nil)
	if err != nil {
		return false, fmt.Errorf("build managed npm verification request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req) //nolint:gosec // tarball URL is constructed from the validated registry origin and staged coordinate
	if err != nil {
		return false, fmt.Errorf("download managed npm artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Errorf("registry tarball returned %s%s", resp.Status, ErrorExcerpt(resp.Body, token))
	}
	if wantSize < 0 {
		return false, fmt.Errorf("managed npm artifact size is invalid")
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(resp.Body, wantSize+1))
	if err != nil {
		return false, fmt.Errorf("hash registry npm artifact: %w", err)
	}
	if read != wantSize {
		return false, fmt.Errorf("published npm artifact size mismatch: staged %d, registry %d", wantSize, read)
	}
	gotDigest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if gotDigest != wantDigest {
		return false, fmt.Errorf("published npm artifact digest mismatch: staged %s, registry %s", wantDigest, gotDigest)
	}
	return true, nil
}

// CheckIdentity refuses an artifact whose manifest does not name exactly its
// Name and Version, so the version document and the attachment describe one
// package version.
func CheckIdentity(artifact Artifact) error {
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(artifact.Manifest, &manifest); err != nil {
		return fmt.Errorf("parse managed npm manifest: %w", err)
	}
	var name, version string
	if json.Unmarshal(manifest["name"], &name) != nil || json.Unmarshal(manifest["version"], &version) != nil ||
		name != artifact.Name || version != artifact.Version {
		return fmt.Errorf("managed npm manifest does not name %s@%s", artifact.Name, artifact.Version)
	}
	return nil
}

// Publish uploads artifact to registry unless the registry already serves
// the same tarball, then reads the tarball back. It reports whether the
// version already existed at the artifact's digest. An artifact CheckIdentity
// refuses is an error before any request, and a version the registry holds
// with other bytes is an error, before and after the upload.
func Publish(ctx context.Context, client *http.Client, registry, token string, artifact Artifact) (reused bool, err error) {
	if err := CheckIdentity(artifact); err != nil {
		return false, err
	}
	payload, err := BuildPayload(artifact)
	if err != nil {
		return false, err
	}
	digest, size := Digest(artifact.Tarball), int64(len(artifact.Tarball))
	found, err := Probe(ctx, client, registry, token, artifact.Name, artifact.Version, digest, size)
	if err != nil {
		return false, fmt.Errorf("check published npm artifact: %w", err)
	}
	if found {
		return true, nil
	}
	if err := Put(ctx, client, registry, token, payload); err != nil {
		return false, fmt.Errorf("managed npm publish failed: %w", err)
	}
	found, err = Probe(ctx, client, registry, token, artifact.Name, artifact.Version, digest, size)
	if err != nil {
		return false, fmt.Errorf("verify published npm artifact: %w", err)
	}
	if !found {
		return false, fmt.Errorf("verify published npm artifact: authenticated tarball returned 404 Not Found")
	}
	return false, nil
}

// TarballURL returns the URL registry serves packageName@version's tarball
// at, for a scoped or unscoped package name.
func TarballURL(registry, packageName, version string) (string, error) {
	name := packageName
	var coordinatePath string
	if strings.HasPrefix(packageName, "@") {
		scope, packagePart, found := strings.Cut(packageName, "/")
		if !found || len(scope) < 2 || packagePart == "" || strings.Contains(packagePart, "/") {
			return "", fmt.Errorf("managed npm package coordinate is invalid")
		}
		name = packagePart
		coordinatePath = url.PathEscape(scope) + "/" + url.PathEscape(packagePart)
	} else {
		if packageName == "" || strings.Contains(packageName, "/") {
			return "", fmt.Errorf("managed npm package coordinate is invalid")
		}
		coordinatePath = url.PathEscape(packageName)
	}
	filename := name + "-" + version + ".tgz"
	return strings.TrimRight(registry, "/") + "/" + coordinatePath + "/-/" + url.PathEscape(filename), nil
}

var bearerCredential = regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`)

// ErrorExcerpt returns ": <excerpt>" for a registry error body, or "" when it
// is empty. The body is upstream-controlled and may echo the request's
// Authorization: token, its query escaping and every bearer value are redacted
// before control characters are replaced and the text is cut to
// ErrorExcerptLimit characters.
func ErrorExcerpt(body io.Reader, token string) string {
	raw, _ := io.ReadAll(io.LimitReader(body, errorReadLimit))
	text := string(raw)
	if token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
		if escaped := url.QueryEscape(token); escaped != token {
			text = strings.ReplaceAll(text, escaped, "[redacted]")
		}
	}
	text = bearerCredential.ReplaceAllString(text, "Bearer [redacted]")
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	if text == "" {
		return ""
	}
	if runes := []rune(text); len(runes) > ErrorExcerptLimit {
		text = string(runes[:ErrorExcerptLimit]) + "..."
	}
	return ": " + text
}
