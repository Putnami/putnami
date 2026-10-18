package finalize

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	job "go.putnami.dev/protocol/job"
)

// Host is what the command reaches through the putnami process that
// launched it: the collaboration providers, the owners of workspace paths,
// and restored verification reports. host.Channel is one.
type Host interface {
	Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error)
	Owners(ctx context.Context, paths []string) ([]wire.PathOwners, error)
	Recover(root string, report features.VerificationReportRef) ([]byte, error)
}

// Command is one run of `putnami change finalize`.
type Command struct {
	// Root is the workspace root, the directory putnami launches the
	// command in.
	Root string
	// ContextFile is the job context putnami passes with --putnamiContext.
	// Empty means none: the command then writes the text lines.
	ContextFile string
	Host        Host
	Stdout      io.Writer
	Stderr      io.Writer

	// Git, Wait and Now exist for tests: production leaves them nil.
	Git  Git
	Wait func(context.Context, time.Duration) error
	Now  func() time.Time
}

// Run runs the command with args, the flags after `change finalize`, and
// returns the exit code: 2 for a usage error, 1 for a failure, 0 for a
// published proposal.
func (c Command) Run(ctx context.Context, args []string) int {
	output := ""
	if c.ContextFile != "" {
		launch, err := job.ParseFile(c.ContextFile)
		if err != nil {
			fmt.Fprintf(c.Stderr, "change finalize: %v\n", err)
			return 1
		}
		if selection := refusedSelection(launch); selection != "" {
			fmt.Fprintf(c.Stderr, "change finalize takes no %s selection\n", selection)
			return 2
		}
		output = launch.Params.String("output")
	}
	options, err := ParseOptions(args)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change finalize: %v\n", err)
		return 2
	}
	f := Finalizer{Root: c.Root, Git: c.Git, Wait: c.Wait, Now: c.Now, Call: c.Host.Call,
		Verify: DossierVerify(c.Root, c.Host.Recover), Workflow: HostedWorkflowVerify(c.Root, c.Host.Recover),
		Owners: HostOwners(c.Root, c.Host.Owners)}
	result, err := f.Run(ctx, options)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change finalize: %v\n", err)
		return 1
	}
	if structuredOutput(output) {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(c.Stderr, "change finalize: %v\n", err)
			return 1
		}
		fmt.Fprintln(c.Stdout, string(data))
		return 0
	}
	ref, _ := json.Marshal(result.ProposalRef)
	fmt.Fprintf(c.Stdout, "PROPOSAL_REF=%s\n", ref)
	if result.ProposalURL != "" {
		fmt.Fprintf(c.Stdout, "PROPOSAL_URL=%s\n", result.ProposalURL)
	}
	return 0
}

// refusedSelection names the project selection flag the launch carries, in
// the order the core checks them, or returns "" when it carries none.
// putnami forwards --projects raw, and writes it for --all and --impacted
// too. A tag or exclusion filter reaches the command only as a scoped
// selection, which does not say which of the three filters narrowed it.
func refusedSelection(launch *job.Context) string {
	selection := launch.Selection
	switch {
	case launch.Params.String("projects") != "":
		return "--projects"
	case launch.Params.Bool("all", false):
		return "--all"
	case selection != nil && selection.Mode == job.SelectionModeImpacted:
		return "--impacted"
	case launch.Params.String("baseline") != "":
		return "--baseline"
	case selection != nil && selection.Scoped:
		return "--tag, --exclude-tag or --exclude"
	}
	return ""
}

// structuredOutput reports whether the output mode putnami forwards is one
// the core answers with the JSON result.
func structuredOutput(mode string) bool {
	switch mode {
	case "json", "jsonl", "cloud-logging":
		return true
	}
	return false
}
