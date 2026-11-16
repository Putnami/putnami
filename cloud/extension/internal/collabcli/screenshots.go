package collabcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
)

// Reasons of proposals.screenshots.
const (
	// reasonScreenshotsNotServed: Delivery answered 404 on the manifest, so
	// it does not serve the screenshots of this change.
	reasonScreenshotsNotServed = "screenshots.not_served"
	// reasonScreenshotsSession: the process holds only a machine token, and
	// Delivery takes the upload from a person's session.
	reasonScreenshotsSession = "screenshots.session_required"
	reasonScreenshotsInvalid = "screenshots.invalid"
	reasonScreenshotsRefused = "screenshots.refused"
)

// Bounds Delivery applies; the provider checks them before it sends.
const (
	maxManifestBytes = 1 << 20
	maxCaptureBytes  = 16 << 20
)

// screenshotsRepeat is the reconciliation of an upload whose outcome is
// unknown: the repeat stores the manifest again and sends what is missing.
const screenshotsRepeat = "repeat proposals.screenshots with the same manifest: it sends only the files Putnami Delivery still lacks"

// screenshotManifest is what the provider reads of a manifest. The bytes go
// to Delivery as written; Delivery owns the strict decoding.
type screenshotManifest struct {
	Project  string `json:"project"`
	HeadSHA  string `json:"headSHA"`
	Captures []struct {
		File   string `json:"file"`
		SHA256 string `json:"sha256"`
	} `json:"captures"`
}

// manifestAnswer is Delivery's answer to a manifest: the digests whose bytes
// it does not hold yet.
type manifestAnswer struct {
	Missing []string `json:"missing"`
}

// screenshots serves proposals.screenshots against Putnami Delivery: the
// manifest as written, then each file Delivery lists as missing.
func (p *Provider) screenshots(ctx context.Context, call collab.Call) (any, *collab.Failure) {
	input, ok := call.Input.(*collab.ProposalScreenshotsInput)
	if !ok || input == nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the request is a %T", call.Input)
	}
	if failure := checkSettings(call.Settings); failure != nil {
		return nil, failure
	}
	if _, err := strconv.ParseUint(input.Ref.ID, 10, 64); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid,
			"the proposal id %q is not a pull request number; nothing was sent", input.Ref.ID)
	}
	file := input.Manifest
	if !filepath.IsAbs(file) {
		file = filepath.Join(call.Request.WorkspaceRoot, file)
	}
	raw, manifest, err := readScreenshotManifest(file, input.Commit)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonScreenshotsInvalid, "%v; nothing was sent", err)
	}
	ctx, cancel := p.budget(ctx)
	defer cancel()
	workspace, failure := p.person(ctx, call.Request.WorkspaceRoot)
	if failure != nil {
		return nil, failure
	}
	delivery := &deliveryScreenshots{workspace: workspace, number: input.Ref.ID}
	answer, err := delivery.postManifest(ctx, raw)
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonScreenshotsNotServed,
				"Putnami Delivery does not serve the screenshots of proposal %s (404 Not Found): %s", input.Ref.ID, refusal.Error())
		}
		return nil, screenshotsFailure(err)
	}
	files, err := missingCaptures(filepath.Dir(file), manifest, answer.Missing)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonScreenshotsInvalid, "%v; the manifest was sent and no file", err)
	}
	for _, capture := range files {
		if err := delivery.putCapture(ctx, capture.digest, capture.path); err != nil {
			return nil, screenshotsFailure(err)
		}
	}
	return &collab.ProposalScreenshotsResult{Ref: input.Ref, Commit: manifest.HeadSHA, Project: manifest.Project,
		Captures: len(manifest.Captures), Uploaded: len(files)}, nil
}

// readScreenshotManifest reads the manifest file and checks what the provider
// relies on: a JSON object that names its project and captures commit.
func readScreenshotManifest(file, commit string) ([]byte, screenshotManifest, error) {
	var manifest screenshotManifest
	info, err := os.Stat(file)
	if err != nil {
		return nil, manifest, fmt.Errorf("read the screenshots manifest: %w", err)
	}
	if info.Size() > maxManifestBytes {
		return nil, manifest, fmt.Errorf("the screenshots manifest holds %d bytes, above the %d Putnami Delivery takes", info.Size(), maxManifestBytes)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, manifest, fmt.Errorf("read the screenshots manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, manifest, fmt.Errorf("the screenshots manifest is not a JSON object: %w", err)
	}
	if strings.TrimSpace(manifest.Project) == "" {
		return nil, manifest, errors.New("the screenshots manifest names no project")
	}
	if manifest.HeadSHA != commit {
		return nil, manifest, fmt.Errorf("the screenshots manifest captures commit %q, not the requested %s", manifest.HeadSHA, commit)
	}
	return raw, manifest, nil
}

type missingCapture struct{ digest, path string }

// missingCaptures resolves each missing digest, once, to the file of the
// manifest that carries it, and checks that the file's bytes hash to it.
func missingCaptures(dir string, manifest screenshotManifest, missing []string) ([]missingCapture, error) {
	byDigest := map[string]string{}
	for _, capture := range manifest.Captures {
		if _, known := byDigest[capture.SHA256]; !known {
			byDigest[capture.SHA256] = capture.File
		}
	}
	var files []missingCapture
	seen := map[string]bool{}
	for _, digest := range missing {
		if seen[digest] {
			continue
		}
		seen[digest] = true
		name, listed := byDigest[digest]
		if !listed {
			return nil, fmt.Errorf("no capture of the manifest carries the digest %q that Putnami Delivery asks for", digest)
		}
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return nil, fmt.Errorf("the capture file %q is not a file name beside the manifest", name)
		}
		full := filepath.Join(dir, name)
		got, size, err := hashFile(full)
		if err != nil {
			return nil, fmt.Errorf("read the capture %s: %w", name, err)
		}
		if size > maxCaptureBytes {
			return nil, fmt.Errorf("the capture %s holds %d bytes, above the %d Putnami Delivery takes", name, size, maxCaptureBytes)
		}
		if got != digest {
			return nil, fmt.Errorf("the capture %s does not hash to the sha256 its manifest names", name)
		}
		files = append(files, missingCapture{digest: digest, path: full})
	}
	return files, nil
}

func hashFile(file string) (string, int64, error) {
	handle, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer handle.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, handle)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// person resolves the linked workspace under the stored session only. A
// process that holds PUTNAMI_CLOUD_TOKEN and no usable session answers
// unsupported and sends nothing: Delivery denies the upload to a machine
// token.
func (p *Provider) person(ctx context.Context, workspaceRoot string) (*clicore.WorkspaceContext, *collab.Failure) {
	env := p.environment()
	token := strings.TrimSpace(env[cloudTokenVar])
	if token != "" {
		p.remember(token)
	}
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnlinked, "read the Putnami Cloud link of the workspace: %v", err)
	}
	if clicore.ResolveWorkspaceID(nil, link) == "" {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnlinked,
			"the workspace is not linked to Putnami Cloud: run putnami cloud setup")
	}
	io := p.io(ctx)
	if token != "" && clicore.ResolveCredential(env, io.Now()).Machine {
		return nil, collab.Fail(collab.OutcomeUnsupported, reasonScreenshotsSession,
			"Putnami Delivery takes screenshots from a person's session, and this process holds only %s: nothing was sent", cloudTokenVar)
	}
	sessionEnv := make(map[string]string, len(env))
	for name, value := range env {
		if name != cloudTokenVar {
			sessionEnv[name] = value
		}
	}
	workspace, err := clicore.ResolveWorkspaceContextFromLink(nil, link, sessionEnv, io)
	if err != nil {
		if token != "" {
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonScreenshotsSession,
				"Putnami Delivery takes screenshots from a person's session, and the stored one cannot be used (%v): nothing was sent", err)
		}
		if clicore.ExitCode(err) == clicore.ExitAuth {
			return nil, collab.Fail(collab.OutcomeDenied, reasonSignIn, "no Putnami session can call Putnami Delivery: %v", err)
		}
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonTransport, "resolve the linked workspace: %v", err)
	}
	p.watchBearer(workspace)
	return workspace, nil
}

// deliveryScreenshots is the screenshots surface of Putnami Delivery for one
// pull request. The published Cloud schema does not declare its two routes
// yet, so the requests are written here and listed in
// clientgen.framework.json.
type deliveryScreenshots struct {
	workspace *clicore.WorkspaceContext
	number    string
}

func (d *deliveryScreenshots) base() string {
	return d.workspace.WorkspaceURL("/changes/pr-" + url.PathEscape(d.number) + "/screenshots")
}

func (d *deliveryScreenshots) postManifest(ctx context.Context, manifest []byte) (*manifestAnswer, error) {
	body, err := d.send(ctx, http.MethodPost, d.base(), "application/json", func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(manifest)), int64(len(manifest)), nil
	})
	if err != nil {
		return nil, err
	}
	var answer manifestAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("read the answer of Putnami Delivery: %w", err)
	}
	return &answer, nil
}

func (d *deliveryScreenshots) putCapture(ctx context.Context, digest, file string) error {
	_, err := d.send(ctx, http.MethodPut, d.base()+"/captures/"+url.PathEscape(digest), "image/png", func() (io.ReadCloser, int64, error) {
		handle, err := os.Open(file)
		if err != nil {
			return nil, 0, err
		}
		info, err := handle.Stat()
		if err != nil {
			handle.Close()
			return nil, 0, err
		}
		return handle, info.Size(), nil
	})
	return err
}

// send issues one request under the session's bearer, and once more with a
// renewed bearer when Delivery answers 401. A refusal is a *Refusal; a
// request that could not be built is NotSent; any other failure leaves the
// outcome unknown.
func (d *deliveryScreenshots) send(ctx context.Context, method, target, contentType string, open func() (io.ReadCloser, int64, error)) ([]byte, error) {
	client := clicore.RedirectRefusingHTTPClient(d.workspace.IO.Client)
	for attempt := 0; ; attempt++ {
		body, size, err := open()
		if err != nil {
			return nil, NotSent(err)
		}
		request, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			body.Close()
			return nil, NotSent(err)
		}
		request.ContentLength = size
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", d.workspace.AuthToken.Authorization())
		clicore.SetUserAgent(request)
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		text, err := io.ReadAll(io.LimitReader(response.Body, maxManifestBytes))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return text, nil
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 && d.workspace.RefreshAuth != nil {
			if fresh, err := d.workspace.RefreshAuth(); err == nil {
				d.workspace.AuthToken = fresh
				continue
			}
		}
		return nil, &Refusal{Status: response.StatusCode, Message: clicore.ServerErrorMessageFromBody(text, response.Status, "message", "error")}
	}
}

// screenshotsFailure maps a failed Delivery call to its outcome. A refusal
// below 500 keeps Delivery's message. A request that never left is
// unavailable. Anything else is unknown: unresolved, with the repeat as its
// reconciliation.
func screenshotsFailure(err error) *collab.Failure {
	var refusal *Refusal
	if errors.As(err, &refusal) && refusal.Status > 0 && refusal.Status < 500 {
		outcome, reason, retryable := collab.OutcomeUnavailable, reasonScreenshotsRefused, false
		switch refusal.Status {
		case http.StatusNotFound:
			outcome, reason = collab.OutcomeNotFound, reasonNotFound
		case http.StatusConflict:
			outcome, reason = collab.OutcomeConflict, collab.ReasonRevisionConflict
		case http.StatusUnauthorized, http.StatusForbidden:
			outcome = collab.OutcomeDenied
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
			outcome, reason = collab.OutcomeInvalid, reasonScreenshotsInvalid
		case http.StatusTooManyRequests:
			reason, retryable = reasonRateLimit, true
		}
		failure := collab.Fail(outcome, reason, "Putnami Delivery refused the screenshots (%s): %s",
			clicore.StatusLine(refusal.Status), refusal.Error())
		failure.Error.Retryable = retryable
		return failure
	}
	if wasNotSent(err) {
		return collab.Fail(collab.OutcomeUnavailable, reasonTransport, "Putnami Delivery was not called: %v", err)
	}
	failure := collab.Fail(collab.OutcomeUnresolved, reasonUnanswered,
		"Putnami Delivery did not confirm the upload, and it was not sent again: %v", err)
	failure.Error.Reconcile = screenshotsRepeat
	return failure
}
