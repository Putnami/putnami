package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/contributor"
	"go.putnami.dev/tooling/cli/internal/commands/proposals"
	"go.putnami.dev/tooling/cli/internal/commands/treecmd"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mcp"
	"go.putnami.dev/tooling/cli/internal/output"
)

// cmdContributor is the native helper namespace registered by the command
// catalog. It owns no contributor workflow or publication state.
func cmdContributor(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "machine-load":
		if len(env.Args) != 0 {
			return usageErrorf("contributor machine-load takes no arguments")
		}
		sample := contributor.MachineLoad(contributor.NativeLoadReader())
		if output.StructuredOutput(env.OutputFormat) {
			data, _ := json.Marshal(sample)
			iox.Fprintln(iox.Stdout(), string(data))
		} else {
			iox.Fprintln(iox.Stdout(), sample.String())
		}
		return nil
	case "language":
		if len(env.Args) == 0 {
			return usageErrorf("contributor language needs text, files, commits or tasks")
		}
		discovered, _ := extension.DiscoverExtensions(env.WsRoot, env.Cfg, projectPathsForRoot(env.WsRoot))
		providers, unavailable := collaborationProviders(env.Ctx, env.WsRoot, env.Cfg, discovered)
		runner := contributor.LanguageRunner{Root: env.WsRoot, Stdin: os.Stdin, Out: iox.Stdout(), Err: os.Stderr,
			Call: func(ctx context.Context, contract, operation string, input json.RawMessage) collab.Envelope {
				return mcp.CallProviderOperation(ctx, mcp.ProviderEnvironment{WorkspaceRoot: env.WsRoot,
					Extensions: providers, Unavailable: unavailable, AttachView: cliWorkspaceView(env.WsRoot)}, contract, operation, input)
			}}
		switch status := runner.RunLanguage(env.Ctx, env.Args[0], env.Args[1:]); status {
		case 0:
			return nil
		case 1:
			return reportedFailure{err: errors.New("non-English signals found")}
		default:
			return usageErrorf("contributor language failed")
		}
	default:
		return usageErrorf("unknown subcommand: contributor %s", env.Sub)
	}
}

func finalizeOptions(args []string) (proposals.Options, error) {
	var options proposals.Options
	seen := map[string]bool{}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		switch name {
		case "--draft", "--partial-delivery":
			if hasValue {
				return options, fmt.Errorf("%s takes no value", name)
			}
			if seen[name] {
				return options, fmt.Errorf("%s given twice", name)
			}
			seen[name] = true
			if name == "--draft" {
				options.Draft = true
			} else {
				options.PartialDelivery = true
			}
			continue
		case "--base", "--branch", "--title", "--body-file", "--task-source", "--task-id", "--verification-file", "--gate", "--proof":
		default:
			return options, fmt.Errorf("unexpected finalize argument %q", raw)
		}
		if seen[name] {
			return options, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if !hasValue {
			if len(args) == 0 {
				return options, fmt.Errorf("%s needs a value", name)
			}
			value = args[0]
			args = args[1:]
		}
		if value == "" || strings.HasPrefix(value, "--") {
			return options, fmt.Errorf("%s needs a nonempty value", name)
		}
		if name == "--gate" && value != "ci" {
			return options, errors.New("--gate accepts only ci; local proof uses --verification-file")
		}
		switch name {
		case "--base":
			options.Base = value
		case "--branch":
			options.Branch = value
		case "--title":
			options.Title = value
		case "--body-file":
			options.BodyFile = value
		case "--task-source":
			options.TaskSource = value
		case "--task-id":
			options.TaskID = value
		case "--verification-file":
			options.VerificationFile = value
		case "--gate":
			options.Gate = value
		case "--proof":
			options.Proof = value
		}
	}
	return options, nil
}

func printFinalizeHelp(w io.Writer) {
	iox.Fprintln(w, "  finalize --base BRANCH --branch BRANCH --title TITLE --body-file FILE [--task-source SOURCE --task-id ID] [--partial-delivery] [--draft | --verification-file DOSSIER | --gate ci] [--proof TEXT]")
	iox.Fprintln(w, "    Publish a draft or a clean committed proposal with current native proof; --gate ci waits for policy-named hosted checks. The command runs no local gate.")
}

func nativeDossierVerify(root string) proposals.Verify {
	return func(_ context.Context, file, base string) (proposals.Verified, error) {
		var buffer bytes.Buffer
		verifier := treecmd.Verifier{Dir: root, Stdout: &buffer, RecoverReport: engine.VerifyCachedVerificationReport}
		err := verifier.Run([]string{"--record", file, "--base", base})
		if err != nil {
			return proposals.Verified{}, fmt.Errorf("native dossier verification: %s", strings.TrimSpace(buffer.String()))
		}
		var answer struct {
			Verdict   string `json:"verdict"`
			Stage     string `json:"stage"`
			Assurance string `json:"assurance"`
			Binding   struct {
				Fingerprint string `json:"fingerprint"`
				HeadSHA     string `json:"headSHA"`
				BaseSHA     string `json:"baseSHA"`
			} `json:"binding"`
			GateSession string `json:"gateSession"`
			Workflow    *struct {
				OutcomeComplete bool `json:"outcomeComplete"`
			} `json:"workflow"`
		}
		if err := json.Unmarshal(buffer.Bytes(), &answer); err != nil {
			return proposals.Verified{}, err
		}
		if answer.Verdict != "Putnami verified" || answer.Stage != "ready" || answer.Assurance != "local-declared-scope" {
			return proposals.Verified{}, errors.New("dossier did not produce local ready assurance")
		}
		return proposals.Verified{Fingerprint: answer.Binding.Fingerprint, HeadSHA: answer.Binding.HeadSHA,
			BaseSHA: answer.Binding.BaseSHA, GateSession: answer.GateSession,
			WorkflowPresent: answer.Workflow != nil, OutcomeComplete: answer.Workflow != nil && answer.Workflow.OutcomeComplete}, nil
	}
}

func (a *App) runFinalizeCommand(ctx context.Context, parsed *ParsedArgs, cfg *wsproto.Config,
	root string, discovered []*extension.ExtensionDescription, stdout, stderr io.Writer) int {
	if selection := collaborationSelectionFlags(parsed.Global); selection != "" {
		iox.Fprintf(stderr, "putnami: proposals finalize takes no %s selection\n", selection)
		return ExitUsage
	}
	options, err := finalizeOptions(parsed.RawJobArgs)
	if err != nil {
		iox.Fprintf(stderr, "putnami: proposals finalize: %v\n", err)
		return ExitUsage
	}
	providers, unavailable := collaborationProviders(ctx, root, cfg, discovered)
	f := proposals.Finalizer{Root: root, Verify: nativeDossierVerify(root), Call: func(ctx context.Context, contract, operation string, input json.RawMessage) collab.Envelope {
		return mcp.CallProviderOperation(ctx, mcp.ProviderEnvironment{WorkspaceRoot: root,
			Extensions: providers, Unavailable: unavailable, AttachView: cliWorkspaceView(root)}, contract, operation, input)
	}}
	result, err := f.Run(ctx, options)
	if err != nil {
		iox.Fprintf(stderr, "putnami: proposals finalize: %v\n", err)
		return ExitError
	}
	if output.StructuredOutput(parsed.Global.Output) {
		data, err := json.Marshal(result)
		if err != nil {
			iox.Fprintf(stderr, "putnami: proposals finalize: %v\n", err)
			return ExitError
		}
		iox.Fprintln(stdout, string(data))
	} else {
		ref, _ := json.Marshal(result.ProposalRef)
		iox.Fprintf(stdout, "PROPOSAL_REF=%s\n", ref)
		if result.ProposalURL != "" {
			iox.Fprintf(stdout, "PROPOSAL_URL=%s\n", result.ProposalURL)
		}
	}
	return ExitSuccess
}
