package runnerprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runnersource"
	"go.putnami.dev/tooling/cli/internal/store"
)

// Offload is everything the engine hands over for one remote execution. The
// request arrives with every block frozen except the source identity, which
// this package binds from the snapshot it captures.
type Offload struct {
	WorkspaceRoot string
	// Workspace is the opaque workspace identity the provider scopes by.
	Workspace string
	// ProviderName is the resolved extension, for notices and for the attempt
	// record that binds a submission to the provider it went through.
	ProviderName string
	Launch       LaunchSpec
	Request      runner.ExecutionRequest
	// Retention is the session store's keep count for the post-import prune.
	Retention int
	Stdout    io.Writer
	Stderr    io.Writer
}

// Outcome is what a completed remote execution produced locally.
type Outcome struct {
	// ExitCode is the remote gate's exit code, which the local process adopts.
	ExitCode int
	// Attempt is the provider's opaque attempt reference.
	Attempt string
	// SessionID is the imported gate session, empty when the remote engine
	// refused before recording one.
	SessionID string
	// InputDigest is the bound request's execution-input digest.
	InputDigest string
	// SourceDigest is the captured snapshot's source-manifest digest.
	SourceDigest string
	// Submission is the request's idempotency key, after any pending record
	// was adopted. The three identities are what an imported session's
	// provenance must state before its verdict is adopted.
	Submission string
}

// ErrUnsupportedRequest reports a request the runner provider did not
// negotiate a capability for. Nothing was submitted or recorded.
var ErrUnsupportedRequest = errors.New("the runner provider cannot carry this request")

// requestCapabilities is the capability list a client offers at initialize
// for request: execution-request-v1 and session-bundle-v1, and
// invocation-providers-v1 when the request carries providers.
func requestCapabilities(request runner.ExecutionRequest) []string {
	offer := []string{runner.CapabilityExecutionRequestV1, runner.CapabilitySessionBundleV1}
	if len(request.Invocation.Providers) > 0 {
		offer = append(offer, runner.CapabilityInvocationProvidersV1)
	}
	return offer
}

// Execute captures the snapshot, binds and validates the request, negotiates
// with the provider, resolves any submission still in flight for the same
// inputs, transfers missing blobs, looks the idempotency key up before it
// submits, follows the attempt to a terminal state, fetches the bundle and
// imports it. Any failure is returned as is: nothing here ever runs the work
// locally instead, and an interrupted follow returns protocol ErrSignal after
// a bounded cancellation.
func Execute(ctx context.Context, offload Offload) (Outcome, error) {
	var outcome Outcome
	sourceStore, err := runnersource.OpenStore(filepath.Join(store.ResolveStoreRoot(offload.WorkspaceRoot), "runner-source"))
	if err != nil {
		return outcome, fmt.Errorf("open source store: %w", err)
	}
	defer func() { _ = sourceStore.Close() }()
	// The admitted bound set travels on the request; capture binds exactly it.
	snapshot, err := sourceStore.Capture(ctx, offload.WorkspaceRoot, offload.Request.Source.Bound)
	if err != nil {
		return outcome, fmt.Errorf("capture source: %w", err)
	}
	request, err := bindSource(offload.Request, offload.WorkspaceRoot, snapshot)
	if err != nil {
		return outcome, err
	}
	outcome.InputDigest, err = runner.ExecutionInputDigest(request)
	if err != nil {
		return outcome, fmt.Errorf("bind execution request: %w", err)
	}
	c, err := newClient(offload.WorkspaceRoot, offload.Workspace, offload.ProviderName, offload.Launch, offload.Retention, offload.Stdout, offload.Stderr)
	if err != nil {
		return outcome, err
	}
	defer c.close()

	c.offer = requestCapabilities(request)
	if err := c.connect(ctx); err != nil {
		return outcome, err
	}
	if len(request.Invocation.Providers) > 0 && !contains(c.capabilities, runner.CapabilityInvocationProvidersV1) {
		return outcome, fmt.Errorf("%w: runner provider %s does not echo %s, which --where remote needs for the providers --providers or %s enabled; run without them, or update the runner provider",
			ErrUnsupportedRequest, offload.ProviderName, runner.CapabilityInvocationProvidersV1, jobs.ProvidersEnv)
	}

	// A submission still in flight for these inputs is THIS run: a CLI that
	// died after submitting, or before its acknowledgement arrived, left the
	// key behind, and the provider resolves the key before anything is
	// submitted again. A settled record never blocks: running the same gate
	// again after it finished is an intentional retry with a new key. The
	// record is read and written once the provider accepted the request's
	// capabilities and before anything is transferred or submitted, so a
	// failed negotiation leaves none.
	pending, err := c.store.Pending(offload.ProviderName, outcome.InputDigest)
	if err != nil {
		return outcome, fmt.Errorf("read attempt records: %w", err)
	}
	if pending != nil {
		request.Control.IdempotencyKey = pending.Submission
		c.record = pending
	} else {
		c.record = &AttemptRecord{
			Submission: request.Control.IdempotencyKey, Provider: offload.ProviderName,
			InputDigest: outcome.InputDigest, SourceDigest: snapshot.Digest,
			Commands: append([]string{}, request.Invocation.Commands...), State: StateSubmitting,
		}
		if err := c.store.Write(c.record); err != nil {
			return outcome, fmt.Errorf("record submission: %w", err)
		}
	}
	request.Protocol.Capabilities = runner.SortStrings(c.capabilities)
	if err := transferMissingBlobs(ctx, c.session, sourceStore, c.exchangeDir, snapshot); err != nil {
		return outcome, err
	}
	found, err := c.session.Lookup(ctx, &runner.LookupParams{IdempotencyKey: request.Control.IdempotencyKey})
	if err != nil {
		return outcome, err
	}
	switch {
	case found.Attempt != "":
		if found.ExecutionInputDigest != "" && found.ExecutionInputDigest != outcome.InputDigest {
			return outcome, fmt.Errorf("submission %s already names attempt %s, which executed different inputs (%s); refusing to adopt it", request.Control.IdempotencyKey, found.Attempt, found.ExecutionInputDigest)
		}
		if c.record.Attempt != "" && c.record.Attempt != found.Attempt {
			return outcome, fmt.Errorf("submission %s names attempt %s locally but %s at the provider; refusing to guess", request.Control.IdempotencyKey, c.record.Attempt, found.Attempt)
		}
		c.record.Attempt, c.record.State = found.Attempt, found.State
		iox.Fprintf(c.stderr, "putnami: resuming remote attempt %s through %s (submission %s, cursor %d)\n", found.Attempt, offload.ProviderName, request.Control.IdempotencyKey, c.record.Cursor)
	default:
		if c.record.Attempt != "" {
			iox.Fprintf(c.stderr, "putnami: %s no longer knows attempt %s; submitting %s again under the same key\n", offload.ProviderName, c.record.Attempt, request.Control.IdempotencyKey)
			c.record.Attempt, c.record.Cursor = "", 0
		}
		submitted, err := c.session.Submit(ctx, &runner.SubmitParams{Request: request, Manifest: snapshot.Manifest})
		if err != nil {
			return outcome, err
		}
		c.record.Attempt, c.record.State = submitted.Attempt, submitted.State
		iox.Fprintf(c.stderr, "putnami: executing remotely through %s (attempt %s, source %s)\n", offload.ProviderName, submitted.Attempt, snapshot.Digest)
	}
	if err := c.store.Write(c.record); err != nil {
		return outcome, fmt.Errorf("record attempt %s: %w", c.record.Attempt, err)
	}
	outcome.Attempt, outcome.SourceDigest, outcome.Submission = c.record.Attempt, snapshot.Digest, request.Control.IdempotencyKey
	return c.observeAndSettle(ctx, outcome)
}

// bindSource writes the captured identity into the request and refuses a
// tree that moved between planning and capture, or a snapshot whose bound
// set is not the one the admission decided on.
func bindSource(request runner.ExecutionRequest, wsRoot string, snapshot runnersource.Snapshot) (runner.ExecutionRequest, error) {
	if bound := boundEntries(snapshot.Manifest); strings.Join(bound, "\x00") != strings.Join(request.Source.Bound, "\x00") {
		return request, fmt.Errorf("the snapshot binds %v, the admission bound %v; refusing to submit a snapshot that differs from what was admitted", bound, request.Source.Bound)
	}
	request.Source.Digest = snapshot.Digest
	request.Source.IndexDigest = snapshot.IndexDigest
	request.Source.Git = snapshot.Git
	request.Source.Tree = nil
	if fingerprint, err := git.FingerprintTree(wsRoot); err == nil {
		if fingerprint.HeadSHA != snapshot.Git.Head || fingerprint.Dirty != snapshot.Git.Dirty {
			return request, fmt.Errorf("source changed during capture (head %s dirty %t, then head %s dirty %t); retry after edits finish",
				snapshot.Git.Head, snapshot.Git.Dirty, fingerprint.HeadSHA, fingerprint.Dirty)
		}
		request.Source.Tree = &runner.TreeIdentity{Fingerprint: fingerprint.Fingerprint, Dirty: fingerprint.Dirty, HeadSHA: fingerprint.HeadSHA}
	}
	if err := runner.ValidateExecutionRequest(request); err != nil {
		return request, fmt.Errorf("bind execution request: %w", err)
	}
	return request, nil
}

// transferMissingBlobs copies exactly the blobs the provider reported missing
// into the exchange directory, each verified against its manifest digest.
func transferMissingBlobs(ctx context.Context, session *Session, sourceStore *runnersource.Store, exchangeDir string, snapshot runnersource.Snapshot) error {
	prepared, err := session.Prepare(ctx, &runner.PrepareParams{SourceDigest: snapshot.Digest, Manifest: snapshot.Manifest})
	if err != nil {
		return err
	}
	entries := make(map[string]runner.SourceEntry, len(snapshot.Manifest.Entries))
	for _, entry := range snapshot.Manifest.Entries {
		if entry.Kind == "file" {
			entries[entry.Digest] = entry
		}
	}
	for _, digest := range prepared.MissingBlobs {
		entry, ok := entries[digest]
		if !ok {
			return fmt.Errorf("runner provider requested blob %s, which the manifest does not name", digest)
		}
		if err := exportBlob(sourceStore, exchangeDir, entry); err != nil {
			return err
		}
	}
	return nil
}

func exportBlob(sourceStore *runnersource.Store, exchangeDir string, entry runner.SourceEntry) error {
	path, ok := runner.ExchangeBlobPath(exchangeDir, entry.Digest)
	if !ok {
		return fmt.Errorf("invalid blob digest %s", entry.Digest)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	writeErr := sourceStore.WriteBlob(file, entry)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("transfer blob %s: %w", entry.Digest, err)
	}
	return nil
}

// newExchangeDir creates the private content-addressed directory one provider
// session moves bytes through.
func newExchangeDir(wsRoot string) (string, error) {
	root := filepath.Join(store.ResolveStoreRoot(wsRoot), "runner-exchange")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create exchange directory: %w", err)
	}
	dir, err := os.MkdirTemp(root, "attempt-*")
	if err != nil {
		return "", fmt.Errorf("create exchange directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
