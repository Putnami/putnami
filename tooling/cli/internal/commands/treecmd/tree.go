// Package treecmd answers questions about the worktree the CLI was invoked in.
//
// It is deliberately workspace-free: `putnami tree fingerprint` is a pure git
// operation, and the build skill runs it inside throwaway repositories that carry
// no putnami.json. It writes nothing itself — no session record, no workspace
// state, no file.
package treecmd

import (
	"encoding/json"
	"fmt"
	"os"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// Fingerprint prints the digest identifying the worktree containing dir.
//
// Human output is the bare digest and nothing else, because its first caller is
// a shell that captures it into a variable and compares it with another agent's:
// a label, a prefix or a second line would have to be stripped by every caller,
// and a caller that forgot would compare two different strings for the same
// tree.
//
// Structured output carries the three members a recorded session's `tree` block
// carries, under the same names, so a consumer reading either surface reads one
// shape.
func Fingerprint(dir string, outputFormat string) error {
	tree, err := git.FingerprintTree(dir)
	if err != nil {
		return err
	}
	if !protocolcli.OutputMode(outputFormat).IsMachineReadable() {
		iox.Fprintln(os.Stdout, tree.Fingerprint)
		return nil
	}
	binding := protocolcli.SessionTree{
		Fingerprint: tree.Fingerprint,
		Dirty:       tree.Dirty,
		HeadSHA:     tree.HeadSHA,
	}
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		return shared.WriteResult(outputFormat, "tree fingerprint", binding, nil)
	}
	document, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("encode tree fingerprint: %w", err)
	}
	iox.Fprintln(os.Stdout, string(document))
	return nil
}
