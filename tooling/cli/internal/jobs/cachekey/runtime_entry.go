package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey/framing"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/store"
)

// runtimeEntryDigestVersion names the layout of a task's implementation digest
// under runtime.compiledInputs (runtimeEntryDigest).
const runtimeEntryDigestVersion = "putnami-extension-entry-v1"

// RuntimeEntryExecutable is the executable a task of ext that names entry runs
// where its command says {extensionRuntime}: the runtime's main executable for
// an empty entry, the entry's executable otherwise, under the root that holds
// the main executable. It needs a synchronized runtime (ext.RuntimeExecutable).
func RuntimeEntryExecutable(ext *extension.ExtensionDescription, entry string) (string, error) {
	if ext == nil || ext.Runtime == nil {
		return "", fmt.Errorf("extension declares no runtime")
	}
	if ext.RuntimeExecutable == "" {
		return "", fmt.Errorf("extension %q runtime was not resolved during workspace synchronization", ext.Name)
	}
	if entry == "" {
		return ext.RuntimeExecutable, nil
	}
	declared, ok := ext.Runtime.Entries[entry]
	if !ok {
		return "", fmt.Errorf("extension %q declares no runtime entry %q", ext.Name, entry)
	}
	// The main executable is the declared path under the runtime root, with
	// the suffix the platform names executables by (".exe" on Windows); an
	// entry takes the same root and the same suffix.
	main := filepath.FromSlash(ext.Runtime.Executable)
	for _, suffix := range []string{"", ".exe"} {
		if root, ok := strings.CutSuffix(ext.RuntimeExecutable, main+suffix); ok {
			return filepath.Join(root, filepath.FromSlash(declared.Executable)+suffix), nil
		}
	}
	return "", fmt.Errorf("extension %q runtime %q is not at its declared path %q", ext.Name, ext.RuntimeExecutable, ext.Runtime.Executable)
}

// runtimeEntryDigest is the implementation digest of a task of a local
// extension whose runtime declares compiledInputs (protocols/extension
// RuntimeDefinition.CompiledInputs, CLI ADR 0133): the runtime declaration,
// every prepare input that is not compiled, and the bytes of the executable
// the task runs. A compiled input reaches the task only through that
// executable, so an edit that rebuilds another entry, or rebuilds the same
// bytes, leaves the task's key where it was. The runtime's main executable is
// folded too when a hook of the extension runs it, because such a hook can
// affect a task whose own command does not.
//
// It needs the synchronized runtime: a caller that keys before
// synchronization (ext.RuntimeExecutable empty) gets ok false and keys on the
// whole runtime, which only costs it a miss.
func runtimeEntryDigest(job *jobmodel.ScheduledJob, cache *store.CacheManager) (string, bool, error) {
	ext := job.Extension
	if ext.RuntimeExecutable == "" {
		return "", false, nil
	}
	entry := ""
	if job.JobDef != nil {
		entry = job.JobDef.RuntimeEntry
	}
	executables := []string{entry}
	if entry != "" && extensionHookRunsRuntime(ext) {
		executables = append(executables, "")
	}
	declaration, err := cache.Digest("runtime-declaration:"+ext.Path+"\x00"+ext.RuntimeDigest, func() (string, error) {
		return runtimeUncompiledDigest(ext)
	})
	if err != nil {
		return "", false, fmt.Errorf("digest extension %q uncompiled runtime inputs: %w", ext.Name, err)
	}
	h := sha256.New()
	framing.HashField(h, "schema", []byte(runtimeEntryDigestVersion))
	framing.HashField(h, "declaration", []byte(declaration))
	for _, name := range executables {
		path, err := RuntimeEntryExecutable(ext, name)
		if err != nil {
			return "", false, err
		}
		digest, err := cache.Digest("runtime-executable:"+path, func() (string, error) {
			return fileSHA256(path)
		})
		if err != nil {
			return "", false, fmt.Errorf("digest extension %q runtime entry %q: %w", ext.Name, name, err)
		}
		framing.HashField(h, "entry", []byte(name))
		framing.HashField(h, "executable", []byte(digest))
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// runtimeUncompiledDigest is ExtensionRuntimeDigest without the compiled
// inputs: the same declaration, platform, ABI and toolchains, and every
// prepare input file that some non-compiled pattern selects. The module
// replacement closure is left out with the compiled inputs: it is the Go
// modules prepare compiles into the executables.
func runtimeUncompiledDigest(ext *extension.ExtensionDescription) (string, error) {
	root, err := dirlink.Resolve(ext.Path)
	if err != nil {
		return "", fmt.Errorf("resolve extension root: %w", err)
	}
	compiled := make(map[string]bool, len(ext.Runtime.CompiledInputs))
	for _, pattern := range ext.Runtime.CompiledInputs {
		compiled[pattern] = true
	}
	var uncompiled []string
	for _, pattern := range ext.Runtime.Prepare.Inputs {
		if !compiled[pattern] {
			uncompiled = append(uncompiled, pattern)
		}
	}
	h := sha256.New()
	framing.HashField(h, "schema", []byte(runtimeEntryDigestVersion+"/declaration"))
	framing.HashField(h, "extension", []byte(ext.Name))
	framing.HashField(h, "version", []byte(ext.Version))
	framing.HashField(h, "platform", []byte(runtime.GOOS+"/"+runtime.GOARCH))
	framing.HashField(h, "runtime-abi", []byte(fmt.Sprintf("%d", runtimeproto.RuntimeABIVersion)))
	framing.HashField(h, "toolchains", []byte(toolchain.RuntimeToolchainIdentityForRefs(ext, ext.Runtime.Prepare.Toolchains)))
	declaration, err := json.Marshal(ext.Runtime)
	if err != nil {
		return "", fmt.Errorf("marshal runtime declaration: %w", err)
	}
	framing.HashField(h, "declaration", declaration)
	if len(uncompiled) == 0 {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	inputs, err := CollectRuntimeInputs(root, uncompiled)
	if err != nil {
		return "", err
	}
	for _, rel := range inputs {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return "", fmt.Errorf("stat runtime input %s: %w", rel, err)
		}
		framing.HashField(h, "input/"+rel+"/mode", []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeType|0o111))))
		digest, err := runtimeFileDigest(full)
		if err != nil {
			return "", fmt.Errorf("hash runtime input %s: %w", rel, err)
		}
		framing.HashField(h, "input/"+rel, digest)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extensionHookRunsRuntime reports whether a lifecycle hook of ext runs its
// runtime's main executable.
func extensionHookRunsRuntime(ext *extension.ExtensionDescription) bool {
	if ext.Hooks == nil {
		return false
	}
	for _, hook := range []*extension.HookDefinition{ext.Hooks.PreBuild, ext.Hooks.OnInstall} {
		if hook == nil {
			continue
		}
		token := "{extensionRuntime}"
		if strings.Contains(hook.Command, token) || strings.Contains(hook.Cwd, token) {
			return true
		}
		for _, arg := range hook.Args {
			if strings.Contains(arg, token) {
				return true
			}
		}
		for _, value := range hook.Env {
			if strings.Contains(value, token) {
				return true
			}
		}
	}
	return false
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
