// Package runtimeinfo implements the extension executable handshake.
package runtimeinfo

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const (
	// Verb is the reserved executable-control verb.
	Verb = "__putnami"
	// Command requests the runtime descriptor.
	Command = "runtime-info"
	// manifestFilename is the extension manifest an installed tree holds at
	// its root.
	manifestFilename = "putnami.extension.json"
)

// Write writes the canonical runtime descriptor for an extension executable.
func Write(w io.Writer, extension, version string) error {
	return json.NewEncoder(w).Encode(runtimeproto.Info{
		Extension:       extension,
		Version:         version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
}

// Handle writes the descriptor and reports whether args named the reserved
// handshake. Call it before normal subcommand dispatch.
func Handle(args []string, w io.Writer, extension, version string) (bool, error) {
	if !isHandshake(args) {
		return false, nil
	}
	return true, Write(w, extension, version)
}

// HandleFromManifest is Handle for a runtime that carries no version of its
// own: the descriptor's version is ManifestVersion of the running executable.
// Such a runtime is byte-identical across builds that differ only in the
// version their packager stamps into the manifest.
func HandleFromManifest(args []string, w io.Writer, extension string) (bool, error) {
	if !isHandshake(args) {
		return false, nil
	}
	version := ""
	if executable, err := os.Executable(); err == nil {
		version = ManifestVersion(executable, extension)
	}
	return true, Write(w, extension, version)
}

func isHandshake(args []string) bool {
	return len(args) == 2 && args[0] == Verb && args[1] == Command
}

// ManifestVersion returns the version of the extension manifest that declares
// executable as the runtime of extension: the nearest putnami.extension.json
// in a directory above executable, when its runtime.executable, with the
// platform's executable suffix, is the same file as executable, and its name,
// when it has one, is extension. Symbolic links in executable are resolved
// first.
//
// The same-file check is what binds the manifest to the executable. The name
// is optional in a manifest: the Go and TypeScript extensions publish theirs
// without one, and the CLI then names the extension by its package or by the
// reference that installs it. A manifest without a name is therefore accepted.
//
// It returns "" when that manifest does not exist, names another extension or
// another file, or cannot be read. A prepared workspace runtime, whose output
// lies outside its source tree and whose source manifest carries no version,
// answers "" in the same way.
func ManifestVersion(executable, extension string) string {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return ""
	}
	self, err := os.Stat(resolved)
	if err != nil {
		return ""
	}
	for dir := filepath.Dir(resolved); ; {
		data, err := os.ReadFile(filepath.Join(dir, manifestFilename))
		if err == nil {
			return declaredVersion(data, dir, extension, self)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// declaredVersion returns the version of the manifest data found in dir when
// it declares self as its runtime executable and names extension or no
// extension, and "" otherwise.
func declaredVersion(data []byte, dir, extension string, self fs.FileInfo) string {
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Runtime *struct {
			Executable string `json:"executable"`
		} `json:"runtime"`
	}
	if json.Unmarshal(data, &manifest) != nil || (manifest.Name != "" && manifest.Name != extension) ||
		manifest.Runtime == nil || manifest.Runtime.Executable == "" {
		return ""
	}
	declared := filepath.Join(dir, filepath.FromSlash(pkgmeta.ExecutableName(runtime.GOOS, manifest.Runtime.Executable)))
	info, err := os.Stat(declared)
	if err != nil || !os.SameFile(self, info) {
		return ""
	}
	return manifest.Version
}
