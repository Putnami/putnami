// Package treecmd answers questions about the worktree the CLI was invoked in.
//
// It is deliberately workspace-free: `putnami tree fingerprint` is a pure git
// operation, and the fix skill runs it inside throwaway repositories that carry
// no putnami.json. `putnami tree verify` checks the evidence the execute skill
// records against that same identity; for a gate record it also asks the
// workspace CLI for a dry-run plan. Neither command writes anything itself — no
// session record, no workspace state, no file.
package treecmd

import (
	"encoding/json"
	"fmt"
	"os"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/output"
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
	if !output.StructuredOutput(outputFormat) {
		iox.Fprintln(os.Stdout, tree.Fingerprint)
		return nil
	}
	document, err := json.Marshal(protocolcli.SessionTree{
		Fingerprint: tree.Fingerprint,
		Dirty:       tree.Dirty,
		HeadSHA:     tree.HeadSHA,
	})
	if err != nil {
		return fmt.Errorf("encode tree fingerprint: %w", err)
	}
	iox.Fprintln(os.Stdout, string(document))
	return nil
}
