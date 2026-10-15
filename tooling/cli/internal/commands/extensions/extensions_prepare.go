package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ErrHostedPrepare refuses `putnami extensions prepare` on a hosted run
// (runcredential.Flag). The command prepares the runtimes a hosted run starts,
// and a hosted run prepares none: it only loads them from the artifact store.
var ErrHostedPrepare = cmderr.Classify(
	fmt.Errorf("putnami extensions prepare does not accept %s: it prepares the runtimes a hosted run starts, so run it without the run credential, as ./putnamiw --print-engine does",
		runcredential.Flag),
	cmderr.ErrUsage)

// preparedRuntimeEntry is one line of `extensions prepare --output=jsonl`: a
// runtime the command prepared, or found already prepared, in the artifact
// store.
type preparedRuntimeEntry struct {
	Kind       string `json:"kind"` // "extension-runtime"
	Name       string `json:"name"`
	Digest     string `json:"digest"`
	Executable string `json:"executable"`
}

// ExtensionsPrepare prepares, into the artifact store, the runtime of every
// extension of wsRoot that the source build prepares
// (extension.PreparedBySourceBuild), with its prepare toolchains resolved
// strictly (jobs.PrepareSourceBuiltRuntimes). `./putnamiw --print-engine` runs
// it with the engine it built, so a hosted run of the same tree loads these
// runtimes from the store and prepares none itself. A workspace whose lock pins
// a published CLI has no such extension, and the command prepares nothing. A
// hosted run is refused (ErrHostedPrepare). outputFormat "jsonl" writes one
// preparedRuntimeEntry per runtime.
func ExtensionsPrepare(ctx context.Context, wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	return extensionsPrepare(ctx, wsRoot, cfg, outputFormat, os.Stdout)
}

func extensionsPrepare(ctx context.Context, wsRoot string, cfg *wsproto.Config, outputFormat string, out io.Writer) error {
	if runcredential.Hosted() {
		return ErrHostedPrepare
	}
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	projectPaths := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		projectPaths = append(projectPaths, project.Path)
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return fmt.Errorf("discover extensions: %w", err)
	}
	var built []*extension.ExtensionDescription
	for _, ext := range discovered.Extensions {
		if extension.PreparedBySourceBuild(wsRoot, ext) {
			built = append(built, ext)
		}
	}
	if err := jobs.PrepareSourceBuiltRuntimes(ctx, ws, built); err != nil {
		return fmt.Errorf("prepare extension runtimes: %w", err)
	}

	if outputFormat == "jsonl" {
		for _, ext := range built {
			data, err := json.Marshal(preparedRuntimeEntry{
				Kind: "extension-runtime", Name: ext.Name, Digest: ext.RuntimeDigest, Executable: ext.RuntimeExecutable,
			})
			if err != nil {
				return fmt.Errorf("encode prepared runtime of %s: %w", ext.Name, err)
			}
			iox.Fprintln(out, string(data))
		}
		return nil
	}
	if len(built) == 0 {
		iox.Fprintln(out, "  No extension runtime to prepare: no path extension of this workspace serves a provider from the source build.")
		return nil
	}
	for _, ext := range built {
		iox.Fprintf(out, "  ✓ %s runtime %s\n", ext.Name, ext.RuntimeExecutable)
	}
	return nil
}
