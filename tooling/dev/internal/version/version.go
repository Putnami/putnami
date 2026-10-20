// Package version serves `putnami version get` and `putnami version tag`:
// it prints the version every version line is at, and releases one line
// with its changelog, its release commit and its annotated tag.
//
// It computes no version. The putnami process that launched the executable
// answers version.lines through the host RPC channel with the versions, the
// proposed releases and the commits the core computes from git, and this
// package prints and releases from that answer. `version tag` runs git itself
// to commit, tag and push.
package version

import (
	"context"
	"errors"
	"fmt"
	"io"

	protocolcli "go.putnami.dev/protocol/cli"
	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// Lines answers version.lines: every version line, or the one line scope
// names. *host.Channel is the production Lines.
type Lines interface {
	VersionLines(ctx context.Context, scope string) (wire.VersionLinesResult, error)
}

// Command runs one version command in a workspace.
type Command struct {
	// Root is the workspace root, the directory putnami launches the command
	// in. Line scope paths and changelog paths are relative to it.
	Root string
	// Output is the output mode the user selected, as putnami forwards it in
	// the job context. json and jsonl select the result envelope.
	Output protocolcli.OutputMode
	// DryRun is the --dry-run flag, which putnami consumes and forwards in
	// the job context.
	DryRun bool
	// Host answers version.lines.
	Host           Lines
	Stdout, Stderr io.Writer
}

// sourceRevisionEnv binds the putnami process that launched the command to a
// commit. The host answers that commit as the source revision.
const sourceRevisionEnv = "PUTNAMI_SOURCE_REVISION"

// supportCatalog is the file whose dependency graph the host refreshes before
// it computes a version, and names when the refresh fails.
const supportCatalog = "putnami.support.json"

// versionLines asks the host for the lines scope names, and turns a refusal
// of the host into the error class the core gives the same refusal: a scope
// that names no line is not found, an invalid configuration stays one, a
// launch outside any workspace has no workspace, and a failure of the host
// is a plain failure with the host's message.
func (c Command) versionLines(ctx context.Context, scope string) (wire.VersionLinesResult, error) {
	result, err := c.Host.VersionLines(ctx, scope)
	if err == nil {
		return result, nil
	}
	var refusal *wire.Error
	if !errors.As(err, &refusal) {
		return wire.VersionLinesResult{}, err
	}
	switch refusal.Code {
	case wire.CodeInvalidParams:
		return wire.VersionLinesResult{}, protocolcli.NotFoundf("%s", refusal.Message)
	case wire.CodeInvalidConfig:
		return wire.VersionLinesResult{}, protocolcli.InvalidConfigf("%s", refusal.Message)
	case wire.CodeUnavailable:
		return wire.VersionLinesResult{}, protocolcli.Classify(errors.New(refusal.Message), protocolcli.ErrNoWorkspace)
	case wire.CodeInternalError:
		return wire.VersionLinesResult{}, errors.New(refusal.Message)
	}
	return wire.VersionLinesResult{}, err
}

// report prints err on stderr and returns its exit code.
func (c Command) report(err error) int {
	fmt.Fprintln(c.Stderr, err)
	return protocolcli.ExitCodeForError(err)
}

// printableLine names a line for a human: the root line has no path, so it is
// spelled with the workspace's own marker rather than an empty column.
func printableLine(scopePath string) string {
	if scopePath == "" {
		return "(workspace root)"
	}
	return scopePath
}

// lineNames lists the printable names of lines, in their order.
func lineNames(lines []wire.VersionLine) []string {
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		names = append(names, printableLine(line.Line))
	}
	return names
}

// changelogRelPath is where a line's changelog lives: inside the line's scope
// directory, or at the workspace root for the root line.
func changelogRelPath(scopePath string) string {
	if scopePath == "" {
		return "CHANGELOG.md"
	}
	return scopePath + "/CHANGELOG.md"
}
