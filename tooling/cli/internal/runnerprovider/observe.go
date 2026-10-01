package runnerprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// Observation policy. None of these bounds a verdict: they pace polling,
// bound the reconnection of a transport this package owns, and bound how long
// an interrupted user waits for the provider to acknowledge a cancellation —
// the whole cancellation must finish inside the terminal adapter's
// two-signal shutdown window, after which a second Ctrl-C forces the exit.
const (
	// followIdle is the client-side pause when a follow returned nothing new.
	followIdle = 100 * time.Millisecond
	// maxReconnects bounds consecutive transport losses before observation
	// is reported lost. A successful answer resets the count.
	maxReconnects = 3
	// reconnectDelay paces respawning a provider whose transport died.
	reconnectDelay = 200 * time.Millisecond
	// cancelAckBudget bounds the wait for the provider's cancel acknowledgement.
	cancelAckBudget = 3 * time.Second
	// cancelSettleBudget bounds the follow to a terminal state after the
	// acknowledgement reported the attempt still terminating.
	cancelSettleBudget = 5 * time.Second
	// settleBudget bounds fetch and import once the run's own context is gone:
	// an attempt that completed under an interrupted observation is still the
	// verdict, and bringing it home is bounded local work.
	settleBudget = 10 * time.Second
)

// client is one observation of one attempt: the provider session, the durable
// record it keeps current, and the local streams it forwards to. offer is the
// capability list it sends at initialize, and capabilities the provider's
// echo.
type client struct {
	wsRoot       string
	workspace    string
	provider     string
	launch       LaunchSpec
	retention    int
	stdout       io.Writer
	stderr       io.Writer
	store        *AttemptStore
	record       *AttemptRecord
	exchangeDir  string
	session      *Session
	offer        []string
	capabilities []string
}

func newClient(wsRoot, workspace, provider string, launch LaunchSpec, retention int, stdout, stderr io.Writer) (*client, error) {
	exchangeDir, err := newExchangeDir(wsRoot)
	if err != nil {
		return nil, err
	}
	return &client{
		wsRoot: wsRoot, workspace: workspace, provider: provider, launch: launch, retention: retention,
		stdout: stdout, stderr: stderr, store: NewAttemptStore(wsRoot), exchangeDir: exchangeDir,
		offer: requestCapabilities(runner.ExecutionRequest{}),
	}, nil
}

func (c *client) close() {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	_ = os.RemoveAll(c.exchangeDir)
}

// connect spawns the provider and negotiates. It replaces a dead session, so
// a reconnect is the same call as the first connection.
func (c *client) connect(ctx context.Context) error {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	session, err := Spawn(ctx, c.launch, c.stderr)
	if err != nil {
		return err
	}
	initialized, err := session.Initialize(ctx, &runner.InitializeParams{
		ProtocolVersion: runner.ProviderProtocolVersion, ExchangeDir: c.exchangeDir,
		Capabilities: c.offer,
		Workspace:    c.workspace,
	})
	if err != nil {
		_ = session.Close()
		return err
	}
	c.session, c.capabilities = session, initialized.Capabilities
	return nil
}

// observeAndSettle follows the acknowledged attempt to a terminal state and
// brings its verdict home. An interrupted observation cancels the attempt
// within bounded time and reports what it observed.
func (c *client) observeAndSettle(ctx context.Context, outcome Outcome) (Outcome, error) {
	final, err := c.observe(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return c.cancel(outcome)
		}
		return outcome, fmt.Errorf("remote attempt %s: %w", c.record.Attempt, err)
	}
	// The attempt is terminal: there is nothing left to cancel, and the
	// verdict that exists is brought home even if the interrupt lands now.
	// The terminal adapter's two-signal policy still bounds a user who wants
	// out during this tail.
	return c.settle(context.WithoutCancel(ctx), outcome, final)
}

// observe forwards output records unchanged to the local streams until the
// attempt reaches a terminal state, persisting the cursor as it goes. A
// transport loss reconnects and replays from the persisted cursor; a record
// at or below it is a duplicate and is dropped. It has no wall-clock budget of
// its own: the caller's context and the provider's deadline bound it.
func (c *client) observe(ctx context.Context) (*runner.FollowResult, error) {
	reconnects := 0
	for {
		result, err := c.session.Follow(ctx, &runner.FollowParams{Attempt: c.record.Attempt, Cursor: c.record.Cursor})
		if err != nil {
			var opErr *OpError
			if ctx.Err() != nil || errors.As(err, &opErr) {
				return nil, err
			}
			// The transport died under us; the attempt did not. Reconnect from
			// the persisted cursor a bounded number of times.
			reconnects++
			if reconnects > maxReconnects {
				return nil, fmt.Errorf("observation lost after %d reconnections (%w); the attempt keeps running and can be resumed with `putnami sessions inspect --run %s`", maxReconnects, err, c.record.Attempt)
			}
			iox.Fprintf(c.stderr, "putnami: observation of attempt %s interrupted (%v); reconnecting from cursor %d\n", c.record.Attempt, err, c.record.Cursor)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(reconnectDelay):
			}
			if err := c.connect(ctx); err != nil {
				return nil, err
			}
			continue
		}
		reconnects = 0
		advanced, err := c.forward(result)
		if err != nil {
			return nil, err
		}
		if !runner.KnownState(result.State) {
			return nil, fmt.Errorf("runner provider reported unknown attempt state %q", result.State)
		}
		if result.State != c.record.State {
			c.record.State = result.State
			advanced = true
		}
		// The cursor is persisted before anything else happens with it — before
		// a drain step as much as before an idle wait — so a CLI that dies at
		// any point resumes after the last line it printed, never before it.
		if advanced {
			if err := c.store.Write(c.record); err != nil {
				return nil, fmt.Errorf("record attempt %s: %w", c.record.Attempt, err)
			}
		}
		if runner.TerminalState(result.State) {
			// A full answer may still hide output before the end: drain it,
			// bounded step by bounded step, before the terminal state is final.
			if len(result.Records) == runner.MaxFollowRecords {
				continue
			}
			return result, nil
		}
		if len(result.Records) == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(followIdle):
			}
		}
	}
}

// forward prints the records after the persisted cursor and reports whether
// the cursor advanced. Records at or below the cursor are a replay and are
// dropped; a non-increasing sequence within one answer is a protocol error.
func (c *client) forward(result *runner.FollowResult) (bool, error) {
	advanced := false
	last := c.record.Cursor
	for _, record := range result.Records {
		if record.Cursor <= c.record.Cursor {
			continue
		}
		if record.Cursor <= last {
			return advanced, fmt.Errorf("runner provider answered follow with cursor %d after %d", record.Cursor, last)
		}
		last = record.Cursor
		target := c.stderr
		if record.Stream == "stdout" {
			target = c.stdout
		}
		iox.Fprintln(target, record.Line)
		advanced = true
	}
	if last > c.record.Cursor {
		c.record.Cursor = last
	}
	if result.Cursor > c.record.Cursor {
		c.record.Cursor = result.Cursor
		advanced = true
	}
	return advanced, nil
}

// cancel runs after the observing context was canceled: it asks the provider
// to terminate the attempt, waits a bounded time for the acknowledgement and
// then for a terminal state, records what it observed, and reports it. A
// cancellation that raced a completion converges on the provider's terminal
// outcome: a completed attempt is still fetched and imported, because the
// verdict exists and the interrupt changes nothing about it.
func (c *client) cancel(outcome Outcome) (Outcome, error) {
	attempt := c.record.Attempt
	iox.Fprintf(c.stderr, "putnami: interrupt: canceling remote attempt %s\n", attempt)
	ackCtx, cancelAck := context.WithTimeout(context.Background(), cancelAckBudget)
	defer cancelAck()
	if c.session == nil || c.session.ended() {
		if err := c.connect(ackCtx); err != nil {
			return outcome, fmt.Errorf("%w: cancellation of attempt %s could not be sent (%w); it may still be running, resume it with `putnami sessions inspect --run %s`", protocolcli.ErrSignal, attempt, err, attempt)
		}
	}
	ack, err := c.session.Cancel(ackCtx, &runner.CancelParams{Attempt: attempt})
	if err != nil {
		return outcome, fmt.Errorf("%w: cancellation of attempt %s was not acknowledged (%w); it may still be running, resume it with `putnami sessions inspect --run %s`", protocolcli.ErrSignal, attempt, err, attempt)
	}
	state, final := ack.State, (*runner.FollowResult)(nil)
	if !runner.TerminalState(state) {
		settleCtx, cancelSettle := context.WithTimeout(context.Background(), cancelSettleBudget)
		defer cancelSettle()
		if result, err := c.observe(settleCtx); err == nil {
			state, final = result.State, result
		} else if c.record.State != "" {
			state = c.record.State
		}
	}
	c.record.State = state
	if final != nil && final.ExitCode != nil {
		code := *final.ExitCode
		c.record.ExitCode = &code
	}
	if err := c.store.Write(c.record); err != nil {
		iox.Fprintf(c.stderr, "putnami: record attempt %s: %v\n", attempt, err)
	}
	switch {
	case state == runner.StateCompleted:
		iox.Fprintf(c.stderr, "putnami: remote attempt %s had already completed; importing its session\n", attempt)
		settleCtx, cancelSettle := context.WithTimeout(context.Background(), settleBudget)
		defer cancelSettle()
		if final == nil {
			// The same drain a normal observation performs: every record after
			// the persisted cursor is forwarded, bounded step by bounded step,
			// before the terminal answer is accepted.
			result, err := c.observe(settleCtx)
			if err != nil {
				return outcome, fmt.Errorf("%w: remote attempt %s completed but its final state could not be read: %w", protocolcli.ErrSignal, attempt, err)
			}
			final = result
		}
		return c.settle(settleCtx, outcome, final)
	case runner.TerminalState(state):
		iox.Fprintf(c.stderr, "putnami: remote attempt %s %s\n", attempt, state)
		return outcome, fmt.Errorf("%w: remote attempt %s %s", protocolcli.ErrSignal, attempt, state)
	default:
		iox.Fprintf(c.stderr, "putnami: remote attempt %s is still %s after the cancellation was acknowledged; observation ended, resume it with `putnami sessions inspect --run %s`\n", attempt, state, attempt)
		return outcome, fmt.Errorf("%w: remote attempt %s still %s after cancellation", protocolcli.ErrSignal, attempt, state)
	}
}

// settle records the terminal state, fetches the bundle of a completed
// attempt and imports it. A retrieval or import failure is recorded beside
// the remote verdict, which it never rewrites, and reported; nothing is rerun.
func (c *client) settle(ctx context.Context, outcome Outcome, final *runner.FollowResult) (Outcome, error) {
	attempt := c.record.Attempt
	c.record.State = final.State
	if final.ExitCode != nil {
		code := *final.ExitCode
		c.record.ExitCode = &code
	}
	if final.State != runner.StateCompleted {
		c.record.Error = fmt.Sprintf("attempt ended %s: %s", final.State, final.Reason)
		_ = c.store.Write(c.record)
		return outcome, fmt.Errorf("remote attempt %s ended %s: %s", attempt, final.State, final.Reason)
	}
	fetched, err := c.session.Fetch(ctx, &runner.FetchParams{Attempt: attempt})
	if err != nil {
		return c.settleFailure(outcome, fmt.Errorf("remote attempt %s completed%s; its session could not be retrieved: %w", attempt, exitSuffix(c.record.ExitCode), err))
	}
	imported, err := importOutcome(c.wsRoot, c.exchangeDir, c.retention, c.stderr, outcome, fetched.Bundle)
	if err != nil {
		return c.settleFailure(outcome, err)
	}
	c.record.Error, c.record.Imported, c.record.SessionID = "", true, imported.SessionID
	code := imported.ExitCode
	c.record.ExitCode = &code
	if err := c.store.Write(c.record); err != nil {
		return imported, fmt.Errorf("record attempt %s: %w", attempt, err)
	}
	_ = c.store.Prune(c.retention)
	return imported, nil
}

func (c *client) settleFailure(outcome Outcome, err error) (Outcome, error) {
	c.record.Error = err.Error()
	if writeErr := c.store.Write(c.record); writeErr != nil {
		return outcome, errors.Join(err, fmt.Errorf("record attempt %s: %w", c.record.Attempt, writeErr))
	}
	return outcome, fmt.Errorf("%w; retry the import with `putnami sessions inspect --run %s`", err, c.record.Attempt)
}

func exitSuffix(code *int) string {
	if code == nil {
		return ""
	}
	return fmt.Sprintf(" with exit %d", *code)
}

// importOutcome imports the bundle and derives the local exit code from the
// canonical session it carries. A completed attempt without a recorded gate
// session is a refusal the remote engine already explained on stderr; it
// keeps its non-zero exit code and can never read as success. A session that
// states its provenance must state THIS submission's.
func importOutcome(wsRoot, exchangeDir string, retention int, stderr io.Writer, outcome Outcome, bundle runner.SessionBundle) (Outcome, error) {
	if bundle.Attempt != outcome.Attempt {
		return outcome, fmt.Errorf("runner provider returned the bundle of attempt %q for %q", bundle.Attempt, outcome.Attempt)
	}
	if bundle.SessionID == "" {
		if bundle.ExitCode == 0 {
			return outcome, fmt.Errorf("remote attempt %s reported success without a recorded session; refusing to treat missing evidence as a verdict", outcome.Attempt)
		}
		iox.Fprintf(stderr, "putnami: remote attempt %s exited %d before recording a session\n", outcome.Attempt, bundle.ExitCode)
		outcome.ExitCode = bundle.ExitCode
		return outcome, nil
	}
	// The gate session is read from the exchange before anything is
	// published, so a document that answers another submission, or a green
	// process over a recorded failure, never reaches the store or `latest`.
	document, err := bundledGateSession(exchangeDir, bundle)
	if err != nil {
		return outcome, fmt.Errorf("remote attempt %s completed with exit %d; its session %s could not be imported: %w", outcome.Attempt, bundle.ExitCode, bundle.SessionID, err)
	}
	// The process exit code is the provider's observation of the pinned
	// entrypoint (after-hooks may turn a recorded success into a failing
	// process). The one combination refused is a green process over a session
	// that recorded a failure: that is a fabricated verdict, not an observation.
	if bundle.ExitCode == 0 && document.Run.ExitCode != 0 {
		return outcome, fmt.Errorf("remote attempt %s: provider reports exit 0 but session %s records exit %d", outcome.Attempt, bundle.SessionID, document.Run.ExitCode)
	}
	// A session that states its provenance states which submission it
	// answers: every member the submitting side knows must match, or the
	// bundle is another submission's verdict however valid its bytes are.
	if document.Placement != nil && document.Placement.Provenance != nil {
		provenance := document.Placement.Provenance
		for _, member := range []struct{ name, stated, bound string }{
			{"execution inputs", provenance.InputDigest, outcome.InputDigest},
			{"source", provenance.SourceDigest, outcome.SourceDigest},
			{"submission", provenance.Submission, outcome.Submission},
		} {
			if member.bound != "" && member.stated != member.bound {
				return outcome, fmt.Errorf("remote attempt %s: session %s states %s %s, this submission bound %s; refusing to adopt another submission's verdict", outcome.Attempt, bundle.SessionID, member.name, member.stated, member.bound)
			}
		}
	}
	imported, err := ImportBundle(wsRoot, exchangeDir, bundle, retention)
	if err != nil {
		return outcome, fmt.Errorf("remote attempt %s completed with exit %d; its session %s could not be imported: %w", outcome.Attempt, bundle.ExitCode, bundle.SessionID, err)
	}
	outcome.SessionID = imported.GateSessionID
	outcome.ExitCode = bundle.ExitCode
	iox.Fprintf(stderr, "putnami: remote session %s imported (%d recorded, %d already present)\n", bundle.SessionID, len(imported.Imported), len(imported.Reused))
	return outcome, nil
}

// bundledGateSession reads the gate session document the bundle names from
// the exchange directory, by the digest and size the bundle claims. ImportBundle
// verifies the same bytes again before it publishes anything.
func bundledGateSession(exchangeDir string, bundle runner.SessionBundle) (protocolcli.SessionFile, error) {
	var document protocolcli.SessionFile
	if err := runner.ValidateSessionBundle(bundle); err != nil {
		return document, err
	}
	for _, file := range bundle.Sessions[0].Files {
		if file.Name != runner.BundleSessionFile {
			continue
		}
		path, ok := runner.ExchangeBlobPath(exchangeDir, file.Digest)
		if !ok {
			return document, fmt.Errorf("bundle file %s/%s has an invalid digest", bundle.SessionID, file.Name)
		}
		data, err := readBounded(path, file.Size)
		if err != nil {
			return document, fmt.Errorf("bundle file %s/%s: %w", bundle.SessionID, file.Name, err)
		}
		if blobDigest(data) != file.Digest {
			return document, fmt.Errorf("bundle file %s/%s failed digest verification", bundle.SessionID, file.Name)
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return document, fmt.Errorf("bundle file %s/%s: %w", bundle.SessionID, file.Name, err)
		}
		return document, nil
	}
	return document, fmt.Errorf("bundle session %s has no session.json", bundle.SessionID)
}

// Resume re-attaches to a recorded attempt: it resolves the submission key at
// the provider, follows from the persisted cursor, and imports the bundle
// through the same path a first observation uses. It is how a CLI that died
// mid-run, or a retrieval that failed, is completed without a second attempt.
type Resume struct {
	WorkspaceRoot string
	Workspace     string
	ProviderName  string
	Launch        LaunchSpec
	Record        *AttemptRecord
	Retention     int
	Stdout        io.Writer
	Stderr        io.Writer
}

// ResumeAttempt performs Resume. A submission the provider no longer knows is
// reported as such; nothing is submitted.
func ResumeAttempt(ctx context.Context, resume Resume) (Outcome, error) {
	outcome := Outcome{Attempt: resume.Record.Attempt, InputDigest: resume.Record.InputDigest,
		SourceDigest: resume.Record.SourceDigest, Submission: resume.Record.Submission}
	if resume.Record.Provider != resume.ProviderName {
		return outcome, fmt.Errorf("submission %s went through %s; the workspace now resolves %s as its runner provider", resume.Record.Submission, resume.Record.Provider, resume.ProviderName)
	}
	c, err := newClient(resume.WorkspaceRoot, resume.Workspace, resume.ProviderName, resume.Launch, resume.Retention, resume.Stdout, resume.Stderr)
	if err != nil {
		return outcome, err
	}
	defer c.close()
	c.record = resume.Record
	if err := c.connect(ctx); err != nil {
		return outcome, err
	}
	found, err := c.session.Lookup(ctx, &runner.LookupParams{IdempotencyKey: c.record.Submission})
	if err != nil {
		return outcome, err
	}
	if found.Attempt == "" {
		return outcome, fmt.Errorf("%s no longer knows submission %s (attempt %q); its record is kept and nothing was resubmitted", resume.ProviderName, c.record.Submission, c.record.Attempt)
	}
	if c.record.Attempt != "" && c.record.Attempt != found.Attempt {
		return outcome, fmt.Errorf("submission %s names attempt %s locally but %s at the provider; refusing to guess", c.record.Submission, c.record.Attempt, found.Attempt)
	}
	if found.ExecutionInputDigest != "" && found.ExecutionInputDigest != c.record.InputDigest {
		return outcome, fmt.Errorf("attempt %s executed inputs %s, the record names %s; refusing to adopt it", found.Attempt, found.ExecutionInputDigest, c.record.InputDigest)
	}
	c.record.Attempt, c.record.State = found.Attempt, found.State
	outcome.Attempt = found.Attempt
	iox.Fprintf(c.stderr, "putnami: resuming remote attempt %s through %s (submission %s, cursor %d)\n", found.Attempt, resume.ProviderName, c.record.Submission, c.record.Cursor)
	if err := c.store.Write(c.record); err != nil {
		return outcome, fmt.Errorf("record attempt %s: %w", found.Attempt, err)
	}
	return c.observeAndSettle(ctx, outcome)
}
