package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	providerext "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// SessionsInspectRun resolves a remote attempt by reference — the provider's
// attempt reference or the submission key — through the durable attempt
// records under .putnami/runner/attempts, and shows the session it produced.
//
// An attempt whose session is already imported is shown from the local store,
// exactly as `sessions inspect <id>` shows it. An attempt that is not — a CLI
// that died mid-run, an import that failed, a bundle the provider could not
// serve — is RESUMED: the provider is asked for the same submission key, the
// output is followed from the persisted cursor, and the bundle is imported
// through the one import path. Nothing is ever resubmitted from here: a
// submission the provider no longer knows is reported as such, beside the
// record that keeps its last observed verdict.
func SessionsInspectRun(ctx context.Context, wsRoot string, cfg *wsproto.Config, ref, outputFormat string) error {
	store := runnerprovider.NewAttemptStore(wsRoot)
	record, err := store.Find(ref)
	if err != nil {
		if errors.Is(err, runnerprovider.ErrAttemptNotFound) {
			return cmderr.Classify(fmt.Errorf("no attempt record names %q under %s; references are the attempt reference or submission key printed when the run was submitted", ref, store.Dir()), cmderr.ErrNotFound)
		}
		return err
	}
	structured := output.StructuredOutput(outputFormat)
	if !record.Imported {
		// Resume through the provider the record names. Forwarded engine output
		// keeps its streams in text mode; a structured stdout belongs to the
		// inspection document alone, so the forwarded records go to stderr then.
		stdout := os.Stdout
		if structured {
			stdout = os.Stderr
		}
		resume, err := resolveRunnerProvider(ctx, wsRoot, cfg)
		if err != nil {
			return err
		}
		resume.Record, resume.Stdout, resume.Stderr = record, stdout, os.Stderr
		outcome, err := runnerprovider.ResumeAttempt(ctx, resume)
		if err != nil {
			return err
		}
		record, err = store.Find(outcome.Attempt)
		if err != nil {
			return err
		}
	}
	if !structured {
		printAttemptRecord(record)
	}
	if record.SessionID == "" {
		if structured {
			data, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				return err
			}
			iox.Fprintln(os.Stdout, string(data))
		}
		return nil
	}
	return SessionsInspect(wsRoot, []string{record.SessionID}, outputFormat)
}

// printAttemptRecord prints the transport's own ledger of the attempt: what the
// provider observed, never a verdict of its own.
func printAttemptRecord(record *runnerprovider.AttemptRecord) {
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Attempt:    %s\n", record.Attempt)
	iox.Fprintf(os.Stdout, "  Submission: %s\n", record.Submission)
	iox.Fprintf(os.Stdout, "  Provider:   %s\n", record.Provider)
	iox.Fprintf(os.Stdout, "  State:      %s\n", record.State)
	if record.ExitCode != nil {
		iox.Fprintf(os.Stdout, "  Exit code:  %d\n", *record.ExitCode)
	}
	iox.Fprintf(os.Stdout, "  Inputs:     %s\n", record.InputDigest)
	iox.Fprintf(os.Stdout, "  Source:     %s\n", record.SourceDigest)
	if record.SessionID == "" && record.Imported {
		iox.Fprintln(os.Stdout, "  Session:    none (the remote engine refused before recording one)")
	}
	if record.Error != "" {
		iox.Fprintf(os.Stdout, "  Error:      %s\n", record.Error)
	}
}

// resolveRunnerProvider discovers the workspace's runner provider the way the
// engine does — by the reserved command, exactly one extension — and prepares
// the launch specification the resumed observation uses. It is the same
// resolution `upgrade` and `ci` perform for their reserved providers outside a
// run: no run is assembled here and nothing is planned.
func resolveRunnerProvider(ctx context.Context, wsRoot string, cfg *wsproto.Config) (runnerprovider.Resume, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return runnerprovider.Resume{}, fmt.Errorf("load workspace for runner provider discovery: %w", err)
	}
	projectPaths := make([]string, len(ws.Projects))
	for index, project := range ws.Projects {
		projectPaths[index] = project.Path
	}
	discovered, err := providerext.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return runnerprovider.Resume{}, fmt.Errorf("discover runner provider: %w", err)
	}
	provider, err := providerext.ResolveReservedProvider(discovered.Extensions, runner.ProviderCommandName)
	if err != nil {
		return runnerprovider.Resume{}, err
	}
	if provider == nil {
		detail := fmt.Sprintf("install exactly one extension declaring %q to resume a remote attempt", runner.ProviderCommandName)
		if cause := providerext.SkippedProviderCause(discovered.Skipped); cause != "" {
			detail += ". " + cause
		}
		return runnerprovider.Resume{}, cmderr.Classify(errors.New(detail), cmderr.ErrNotFound)
	}
	launch, err := runnerprovider.LaunchSpecFor(ctx, wsRoot, discovered.Extensions, provider)
	if err != nil {
		return runnerprovider.Resume{}, err
	}
	return runnerprovider.Resume{
		WorkspaceRoot: wsRoot, Workspace: jobs.RunMarkerWorkspaceID(ws), ProviderName: provider.ExtensionName,
		Launch: launch, Retention: workspace_state.RetentionFromWorkspace(cfg),
	}, nil
}

// sessionIdentity is what a listing needs beyond the v1 metadata view: the
// commit the session's tree sat on and where it executed, both absent for a
// session that recorded neither (older CLIs, a tree outside git).
type sessionIdentity struct {
	headSHA   string
	placement *protocolcli.SessionPlacement
}

func readSessionIdentity(store *workspace_state.SessionStore, id string) sessionIdentity {
	data, err := os.ReadFile(filepath.Join(store.Root(), id, "session.json"))
	if err != nil || recordedProtocolVersion(data) < 2 {
		return sessionIdentity{}
	}
	var doc protocolcli.SessionFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return sessionIdentity{}
	}
	identity := sessionIdentity{placement: doc.Placement}
	if doc.Tree != nil {
		identity.headSHA = doc.Tree.HeadSHA
	}
	return identity
}

// ValidateRevision checks a --revision selector: at least seven and at most
// sixty-four lowercase hex characters, matched as a prefix of the recorded
// tree's head commit. A branch name or a symbolic ref is refused: the listing
// answers "which sessions judged THIS commit", and a name that moves names
// nothing.
func ValidateRevision(revision string) error {
	if len(revision) < 7 || len(revision) > 64 {
		return cmderr.Usagef("--revision takes 7 to 64 hex characters of a commit id, got %q", revision)
	}
	for i := 0; i < len(revision); i++ {
		b := revision[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return cmderr.Usagef("--revision takes lowercase hex characters of a commit id, not %q; resolve a ref with git rev-parse first", revision)
		}
	}
	return nil
}

func matchesRevision(identity sessionIdentity, revision string) bool {
	return revision == "" || (identity.headSHA != "" && strings.HasPrefix(identity.headSHA, revision))
}
