package cli

import (
	"path/filepath"
	"slices"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// An unchanged run that a machine worker serves is answered from memory (CLI
// ADR 0116 §6a). The worker decides whether a request is answerable
// (internal/machineproc), the engine answers its runs and the scheduler its
// plans from what the held run computed; this file is App.Run's half. Before
// it dispatches a job command, App.Run loads the workspace configuration
// through a read-only file index and discovers the workspace's extensions,
// which loads the workspace. A run that may be held leaves what it loaded to
// the worker, and the App.Run of a later run of the same request, decided
// answerable, takes it instead of loading it: it reads no file index, loads
// no configuration, materializes no lock-pinned artifact and discovers no
// extension. A process that is no worker holds nothing.
//
// The read-only file index is held as it was opened, and stays installed for
// the run as the one it would open: it records nothing, and answers a file's
// digest only while the file's stat is the one it recorded, so an index read
// earlier answers no less soundly than one read now; it may answer fewer.

// heldDispatch is what App.Run loaded before it dispatched a job command.
type heldDispatch struct {
	root       string
	args       []string
	index      *fileindex.Index
	cfg        *wsproto.Config
	extensions []*extension.ExtensionDescription
	groups     map[string]bool
	// manifests witnesses each discovered extension's manifest, taken once
	// discovery read it. The worktree's journal sees a manifest in the
	// worktree, and the decision's witnesses the artifact store's
	// directories, but not an extension a configuration names outside the
	// worktree, nor a link in the worktree's state to a stored one.
	manifests map[string]string
}

// takeHeldDispatch returns what the held run's App.Run loaded, for a run in
// wsRoot of args that the worker decided answerable, while nothing the
// decision witnessed moved and every extension manifest discovery read holds;
// nil otherwise. A manifest that moved withdraws everything the held run
// offers the run, which then loads and plans what is on disk.
func takeHeldDispatch(wsRoot string, args []string) *heldDispatch {
	if wsRoot == "" {
		return nil
	}
	held, ok := machineproc.TakeDispatch(wsRoot).(*heldDispatch)
	if !ok || held.root != wsRoot || !slices.Equal(held.args, args) {
		return nil
	}
	if moved := held.movedManifest(); moved != "" {
		machineproc.DispatchMoved(wsRoot, "an extension's manifest moved: "+moved)
		return nil
	}
	return held
}

// movedManifest names the first extension manifest whose witness moved since
// discovery read it, or "".
func (h *heldDispatch) movedManifest() string {
	for path, witness := range h.manifests {
		if machineproc.Witness(path) != witness {
			return path
		}
	}
	return ""
}

// loadedDispatch is what App.Run loaded for a run in wsRoot of args, with the
// witnesses of the extension manifests discovery read, taken now.
func loadedDispatch(wsRoot string, args []string, index *fileindex.Index, cfg *wsproto.Config, extensions []*extension.ExtensionDescription, groups map[string]bool) *heldDispatch {
	manifests := make(map[string]string, len(extensions))
	for _, ext := range extensions {
		if ext != nil && ext.Path != "" {
			path := filepath.Join(ext.Path, extensionproto.ManifestFilename)
			manifests[path] = machineproc.Witness(path)
		}
	}
	return &heldDispatch{root: wsRoot, args: slices.Clone(args), index: index, cfg: cfg, extensions: extensions, groups: groups, manifests: manifests}
}
