package jobs

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	extensionproto "go.putnami.dev/protocol/extension"
	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// ProviderLaunch is how the CLI starts a provider subprocess it owns: the
// remote build-cache provider, the runner provider, or the session reporter.
// Each of those packages converts it to its own launch type.
type ProviderLaunch struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
}

// PrepareProviderLaunch resolves how to start the provider that command names
// in ext. It is the one launch path of the three providers.
//
// A provider command gets the preparation a task gets. The CLI prepares the
// extension runtime the command references, or loads it from the store, and
// verifies it with the runtime-info handshake. {extensionRuntime} then names
// compiled/<name>, or compiled/<name>.exe on Windows, and never a POSIX
// launcher. The command's templates are expanded, the toolchains it declares
// are applied, and a bare command resolves against the provider's own PATH.
// The provider inherits this process's environment plus the command's env, and
// runs in the workspace root unless the command declares a cwd. Like a task,
// it finds this CLI first on its PATH and in PUTNAMI_CLI_EXECUTABLE. A provider
// launched before CaptureProcessCapabilities, such as the bootstrap provider
// of a relaunch, gets none of the capability transport that capture would
// take (withoutUncapturedCapabilities).
//
// The runtime is prepared and verified once per process for each runtime
// identity (see providerRuntimes). A second launch of the same provider, such
// as the build after auto-selection's run-marker lookup, reuses it.
//
// An error means the provider cannot start: the command has no executable, the
// runtime cannot be prepared, a toolchain cannot be applied, or the executable
// is not on the prepared PATH. Preparation writes ext.RuntimeExecutable and
// ext.RuntimeDigest, so call it before any job that reads them runs.
func PrepareProviderLaunch(ctx context.Context, workspaceRoot string, ext *extension.ExtensionDescription, command string) (ProviderLaunch, error) {
	if ext == nil {
		return ProviderLaunch{}, fmt.Errorf("provider command %q has no extension", command)
	}
	job := ext.Jobs[command]
	if job == nil || strings.TrimSpace(job.Command) == "" {
		return ProviderLaunch{}, fmt.Errorf("provider command %q of %s has no executable", command, ext.Name)
	}
	if err := prepareProviderRuntime(ctx, workspaceRoot, ext, command); err != nil {
		return ProviderLaunch{}, err
	}

	vars := extension.BuildTemplateVars(workspaceRoot, workspaceRoot, ext.Path, filepath.Join(workspaceRoot, ".putnami", "out"))
	vars[extensionproto.TemplateVarExtensionRuntime] = ext.RuntimeExecutable
	launch := ProviderLaunch{
		Command: extension.ExpandTemplateVars(job.Command, vars),
		Args:    make([]string, 0, len(job.Args)),
		Dir:     workspaceRoot,
		Env:     withoutUncapturedCapabilities(os.Environ()),
	}
	for _, arg := range job.Args {
		launch.Args = append(launch.Args, extension.ExpandTemplateVars(arg, vars))
	}
	if job.Cwd != "" {
		launch.Dir = extension.ExpandTemplateVars(job.Cwd, vars)
	}
	for _, key := range slices.Sorted(maps.Keys(job.Env)) {
		launch.Env = append(launch.Env, key+"="+extension.ExpandTemplateVars(job.Env[key], vars))
	}

	// A provider that calls back into the CLI reaches this exact process, as a
	// task does (see the runner): the cache provider's token command runs a
	// bare `putnami`, and a source workspace refuses every other binary.
	cliExecutable := ""
	if executable, err := os.Executable(); err == nil && executable != "" {
		cliExecutable = executable
		launch.Env = prependEnvPath(launch.Env, filepath.Dir(cliExecutable))
	}

	var err error
	if launch.Env, err = ProviderRuntimeEnvironment(ctx, launch.Env, ext, job.Toolchains); err != nil {
		return ProviderLaunch{}, err
	}
	if cliExecutable != "" {
		launch.Env = setEnv(launch.Env, registryproto.CLIExecutableEnv, cliExecutable)
	}
	if launch.Command, err = ProviderExecutable(launch.Command, launch.Env); err != nil {
		return ProviderLaunch{}, err
	}
	return launch, nil
}

// prepareProviderRuntime is synchronizeExtensionRuntimesForCommands for one
// provider command, with the runtime it prepares and verifies remembered in
// providerRuntimes. A remembered runtime skips preparation and the handshake;
// the command's toolchains are still resolved.
func prepareProviderRuntime(ctx context.Context, workspaceRoot string, ext *extension.ExtensionDescription, command string) error {
	extensions := []*extension.ExtensionDescription{ext}
	activeCommands := commandDependencyClosure(extensions, []string{command})
	slot, memoized := providerRuntimeSlotFor(ext, activeCommands)
	if memoized {
		if record, ok := providerRuntimes.current(slot, ext); ok {
			if ext.RuntimeExecutable != record.executable || ext.RuntimeDigest != record.digest {
				ext.RuntimeExecutable, ext.RuntimeDigest = record.executable, record.digest
			}
			return resolveCommandRuntimeToolchains(ctx, workspaceRoot, extensions, activeCommands, resolveStrict)
		}
	}
	if err := synchronizeExtensionRuntimesForCommands(ctx, workspaceRoot, extensions, []string{command}, nil, resolveStrict); err != nil {
		return err
	}
	if memoized {
		providerRuntimes.remember(slot, ext)
	}
	return nil
}

// providerRuntimes remembers each provider runtime this process prepared and
// verified with the runtime-info handshake.
//
// A record is reused only while its runtime identity holds: the same extension
// name, version, root and declared executable; for a runtime prepared from
// local source, the same input digest; and the same file at the executable
// path, compared by os.SameFile, size, mode and modification time. A failed
// preparation records nothing, so the next launch tries again.
var providerRuntimes = providerRuntimeMemo{records: map[providerRuntimeSlot]providerRuntimeRecord{}}

type providerRuntimeMemo struct {
	mu      sync.Mutex
	records map[providerRuntimeSlot]providerRuntimeRecord
}

// providerRuntimeSlot is one extension's declared runtime.
type providerRuntimeSlot struct {
	extension, version, root, executable string
	// prepared is set for a local-source runtime that runtime.prepare builds:
	// its identity is the digest of its inputs, not only the file.
	prepared bool
}

type providerRuntimeRecord struct {
	executable string
	digest     string
	file       os.FileInfo
}

// providerRuntimeSlotFor names the runtime the active commands of ext use. It
// is false when they use none: synchronization then only resolves toolchains,
// which has no handshake to save.
func providerRuntimeSlotFor(ext *extension.ExtensionDescription, activeCommands map[string]bool) (providerRuntimeSlot, bool) {
	if ext.Runtime == nil || commandRuntimeUse(ext, activeCommands) == nil {
		return providerRuntimeSlot{}, false
	}
	return providerRuntimeSlot{
		extension:  ext.Name,
		version:    ext.Version,
		root:       filepath.Clean(ext.Path),
		executable: ext.Runtime.Executable,
		prepared:   ext.LocalSource && ext.Runtime.Prepare != nil,
	}, true
}

// current returns the record for slot while its runtime identity holds. For a
// prepared runtime it hashes the inputs of ext, the check synchronization
// itself makes; a digest it cannot compute is not a match.
func (m *providerRuntimeMemo) current(slot providerRuntimeSlot, ext *extension.ExtensionDescription) (providerRuntimeRecord, bool) {
	m.mu.Lock()
	record, ok := m.records[slot]
	m.mu.Unlock()
	if !ok {
		return providerRuntimeRecord{}, false
	}
	if slot.prepared {
		digest, err := extensionRuntimeDigest(ext)
		if err != nil || digest != record.digest {
			return providerRuntimeRecord{}, false
		}
	}
	file, err := verifiedRuntimeFile(record.executable)
	if err != nil || !sameRuntimeFile(record.file, file) {
		return providerRuntimeRecord{}, false
	}
	return record, true
}

// remember records the runtime synchronization just prepared and verified for
// ext. A runtime whose file cannot be read now is not recorded.
func (m *providerRuntimeMemo) remember(slot providerRuntimeSlot, ext *extension.ExtensionDescription) {
	if ext.RuntimeExecutable == "" {
		return
	}
	file, err := verifiedRuntimeFile(ext.RuntimeExecutable)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.records[slot] = providerRuntimeRecord{executable: ext.RuntimeExecutable, digest: ext.RuntimeDigest, file: file}
	m.mu.Unlock()
}

// verifiedRuntimeFile is the file at executable when it still passes the check
// preparation applies before the handshake.
func verifiedRuntimeFile(executable string) (os.FileInfo, error) {
	if err := validateRuntimeExecutable(executable); err != nil {
		return nil, err
	}
	return os.Lstat(executable)
}

func sameRuntimeFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}
