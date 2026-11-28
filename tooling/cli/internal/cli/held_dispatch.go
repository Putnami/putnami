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
	// manifests witnesses each directory discovery probed for an extension,
	// and the manifest and putnami.json there, present or absent, each taken
	// before discovery read it (extension.WitnessProbes). The worktree's
	// journal sees a manifest in the worktree, and the decision's witnesses
	// the artifact store's directories, but not an extension a configuration
	// names outside the worktree, nor a link in the worktree's state to a
	// stored one, nor one that arrives where discovery found none.
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
		machineproc.DispatchMoved(wsRoot, "a path discovery probed for an extension moved: "+moved)
		return nil
	}
	return held
}

// Witnesses are the stats of the paths discovery probed for an extension:
// the worker's decision checks them before it offers anything of the held
// run, whether or not this App.Run takes the dispatch.
func (h *heldDispatch) Witnesses() map[string]string { return h.manifests }

// movedManifest names the first path discovery probed whose witness moved
// since it was taken, or "".
func (h *heldDispatch) movedManifest() string {
	for path, witness := range h.manifests {
		if machineproc.Witness(path) != witness {
			return path
		}
	}
	return ""
}

// loadedDispatch is what App.Run loaded for a run in wsRoot of args, with
// probes, the witnesses extension.WitnessProbes took while discovery ran; nil
// when an extension it loaded has no witness of its manifest there, which a
// stat taken now, after the read, cannot stand for.
func loadedDispatch(wsRoot string, args []string, index *fileindex.Index, cfg *wsproto.Config, extensions []*extension.ExtensionDescription, groups map[string]bool, probes map[string]string) *heldDispatch {
	for _, ext := range extensions {
		if ext == nil || ext.Path == "" {
			continue
		}
		if _, ok := probes[filepath.Join(ext.Path, extensionproto.ManifestFilename)]; !ok {
			return nil
		}
	}
	return &heldDispatch{root: wsRoot, args: slices.Clone(args), index: index, cfg: cfg, extensions: extensions, groups: groups, manifests: probes}
}
