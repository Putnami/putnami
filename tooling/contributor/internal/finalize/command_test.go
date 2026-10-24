package finalize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// commandHost is a Host that counts every call into it.
type commandHost struct {
	provider *finalizerProvider
	calls    int
}

func (h *commandHost) Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	h.calls++
	if h.provider == nil {
		return collab.Envelope{}, errors.New("no provider")
	}
	return h.provider.call(ctx, contract, operation, input)
}

func (h *commandHost) Owners(context.Context, []string) ([]wire.PathOwners, error) {
	h.calls++
	return nil, errors.New("no owners")
}

func (h *commandHost) Recover(string, features.VerificationReportRef) ([]byte, error) {
	h.calls++
	return nil, errors.New("no report")
}

// writeContext writes a job context file with params and selection, and
// returns its path.
func writeContext(t *testing.T, params map[string]any, selection map[string]any) string {
	t.Helper()
	document := map[string]any{"params": params}
	if selection != nil {
		document["selection"] = selection
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// draftCommand returns a Command over the finalizer fixture that publishes
// a draft, and the arguments that ask for it.
func draftCommand(t *testing.T, contextFile string) (Command, *commandHost, *bytes.Buffer, *bytes.Buffer, []string) {
	t.Helper()
	f, git, provider, opts := finalizerFixture(t)
	git.ahead = false
	h := &commandHost{provider: provider}
	var stdout, stderr bytes.Buffer
	command := Command{Root: f.Root, ContextFile: contextFile, Host: h, Git: git, Stdout: &stdout, Stderr: &stderr}
	args := []string{"--base", opts.Base, "--branch", opts.Branch, "--title", opts.Title, "--body-file", opts.BodyFile, "--draft"}
	return command, h, &stdout, &stderr, args
}

// TestTheCommandRefusesAProjectSelection pins the refusal of every project
// selection putnami forwards, in the core's order and with its wording, as
// a usage error that reaches no provider.
func TestTheCommandRefusesAProjectSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		params    map[string]any
		selection map[string]any
		want      string
	}{
		{"projects", map[string]any{"projects": "/lib"}, map[string]any{"mode": "projects", "scoped": true, "projects": []string{"/lib"}}, "--projects"},
		{"all", map[string]any{"projects": "*", "all": true}, map[string]any{"mode": "all", "scoped": false, "projects": []string{"/lib"}}, "--projects"},
		{"impacted", map[string]any{"projects": "[impacted]"}, map[string]any{"mode": "impacted", "scoped": true, "projects": []string{}}, "--projects"},
		{"all without projects", map[string]any{"all": true}, nil, "--all"},
		{"impacted without projects", map[string]any{}, map[string]any{"mode": "impacted", "scoped": true, "projects": []string{}}, "--impacted"},
		{"baseline", map[string]any{"baseline": "origin/main"}, map[string]any{"mode": "all", "scoped": false, "projects": []string{"/lib"}}, "--baseline"},
		{"filters", map[string]any{}, map[string]any{"mode": "projects", "scoped": true, "projects": []string{"/lib"}}, "--tag, --exclude-tag or --exclude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			command, h, stdout, stderr, args := draftCommand(t, writeContext(t, tc.params, tc.selection))
			code := command.Run(context.Background(), args)
			if want := "change finalize takes no " + tc.want + " selection\n"; code != 2 || stderr.String() != want || stdout.Len() != 0 || h.calls != 0 {
				t.Errorf("exit %d, stderr %q, stdout %q, %d host calls; want 2, %q, nothing and none", code, stderr, stdout, h.calls, want)
			}
		})
	}
}

// TestTheCommandPrintsTheResultInTheOutputMode pins both output forms: the
// JSON result on one line in a structured mode, and the PROPOSAL_REF and
// PROPOSAL_URL lines otherwise, or with no job context.
func TestTheCommandPrintsTheResultInTheOutputMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		noFile bool
		url    string
		json   bool
	}{
		{name: "json", output: "json", json: true},
		{name: "jsonl", output: "jsonl", json: true},
		{name: "cloud-logging", output: "cloud-logging", json: true},
		{name: "text", output: "text"},
		{name: "auto"},
		{name: "no context", noFile: true},
		{name: "with a URL", url: "https://example.invalid/proposals/pr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			contextFile := ""
			if !tc.noFile {
				params := map[string]any{}
				if tc.output != "" {
					params["output"] = tc.output
				}
				contextFile = writeContext(t, params, map[string]any{"mode": "all", "scoped": false, "projects": []string{}})
			}
			command, h, stdout, stderr, args := draftCommand(t, contextFile)
			if tc.url != "" {
				h.provider.statusHook = func(proposal *collab.Proposal) { proposal.URL = tc.url }
			}
			if code := command.Run(context.Background(), args); code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if tc.json {
				var result Result
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || strings.Count(stdout.String(), "\n") != 1 ||
					result.ProposalRef != (collab.Ref{Source: "local:proposal", ID: "pr"}) || result.State != collab.ProposalStateDraft {
					t.Errorf("stdout %q, want the result on one line", stdout)
				}
				return
			}
			want := `PROPOSAL_REF={"source":"local:proposal","id":"pr"}` + "\n"
			if tc.url != "" {
				want += "PROPOSAL_URL=" + tc.url + "\n"
			}
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q", stdout, want)
			}
		})
	}
}

// TestTheCommandReportsAMergedProposalAndExitsZero pins the output for a
// branch whose proposal is merged: both forms name the proposal, and stderr
// says nothing was published.
func TestTheCommandReportsAMergedProposalAndExitsZero(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "a-settled-proposal-of-the-branch-opens-no-replacement")
	for _, output := range []string{"json", "text"} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			contextFile := writeContext(t, map[string]any{"output": output}, map[string]any{"mode": "all", "scoped": false, "projects": []string{}})
			command, h, stdout, stderr, args := draftCommand(t, contextFile)
			h.provider.settled = []collab.Proposal{settledProposal(collab.ProposalStateMerged, "7", "0123456789012345678901234567890123456789")}
			if code := command.Run(context.Background(), args); code != 0 || h.provider.upserts != 0 ||
				stderr.String() != "change finalize: proposal github:acme/app/7 is already merged; nothing was published\n" {
				t.Fatalf("exit %d, upserts %d, stderr %q", code, h.provider.upserts, stderr)
			}
			if output == "json" {
				var result Result
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.State != collab.ProposalStateMerged || result.ProposalRef.ID != "7" {
					t.Errorf("stdout %q", stdout)
				}
				return
			}
			want := `PROPOSAL_REF={"source":"github:acme/app","id":"7"}` + "\nPROPOSAL_URL=https://github.com/acme/app/pull/7\nPROPOSAL_STATE=merged\n"
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q", stdout, want)
			}
		})
	}
}

// TestTheCommandExitCodes pins exit 2 for a usage error, which reaches no
// provider, exit 1 for a failure and for a context it cannot read, and the
// message prefix of each.
func TestTheCommandExitCodes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "tooling/contributor-executable", "change-finalize-parity", "a-usage-error-never-dials-the-host")
	command, h, stdout, stderr, args := draftCommand(t, "")
	if code := command.Run(context.Background(), append(args, "--gate", "local")); code != 2 || h.calls != 0 || stdout.Len() != 0 ||
		stderr.String() != "change finalize: --gate accepts only ci; local proof uses --verification-file\n" {
		t.Errorf("usage error: exit %d, %d host calls, stdout %q, stderr %q", code, h.calls, stdout, stderr)
	}
	command, h, stdout, stderr, args = draftCommand(t, "")
	h.provider = nil
	if code := command.Run(context.Background(), args); code != 1 || stdout.Len() != 0 || stderr.String() != "change finalize: no provider\n" {
		t.Errorf("failure: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	command, h, stdout, stderr, args = draftCommand(t, filepath.Join(t.TempDir(), "absent.json"))
	if code := command.Run(context.Background(), args); code != 1 || h.calls != 0 || stdout.Len() != 0 ||
		!strings.HasPrefix(stderr.String(), "change finalize: reading context file: ") {
		t.Errorf("unreadable context: exit %d, %d host calls, stdout %q, stderr %q", code, h.calls, stdout, stderr)
	}
}
