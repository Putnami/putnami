package workspaceclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/protocol/clientcontract"
)

// generatedTypeScriptEntry is the file every TypeScript client the emitter
// generates holds: the package's main module.
const generatedTypeScriptEntry = "src/index.ts"

// requireGeneratedTypeScriptClient is the emitter's post-condition: a
// generation that exits 0 leaves the client's src/index.ts, unless the
// provider's contract declares no first-party operation, in which case the
// emitter removes src and writes nothing.
//
// An emitter that predates the --contract and --output arguments ignores
// them, exits 0 and writes nothing. The task's src output is optionalEmpty, so
// the engine would record the client empty, and every worktree sharing the
// store would restore an empty src. Failing here stores no entry.
// requireLockedTypeScriptEmitter has already shown that the install matches
// the lockfile, so the remedy is to upgrade the pinned @putnami/client.
func requireGeneratedTypeScriptClient(emitter string, generation TypeScriptGeneration) error {
	entry := filepath.Join(generation.Project, filepath.FromSlash(generatedTypeScriptEntry))
	if _, err := os.Stat(entry); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check the generated client of %s: %w", generation.Project, err)
	}
	data, err := os.ReadFile(generation.Contract) //nolint:gosec // G304: the keyed copy of the provider's contract
	if err != nil {
		return fmt.Errorf("read the provider's contract: %w", err)
	}
	operations, err := firstPartyOperations(data)
	if err != nil {
		return fmt.Errorf("read the provider's contract: %w", err)
	}
	if operations == 0 {
		return nil
	}
	return fmt.Errorf("the TypeScript emitter %s exited 0 but wrote no %s into %s, although the provider's contract "+
		"declares %d first-party operation(s): this @putnami/client predates the --contract and --output arguments; "+
		"upgrade @putnami/client to the release of this @putnami/clientgen, run `putnami deps install`, then generate again",
		emitter, generatedTypeScriptEntry, generation.Project, operations)
}

// requireLockedTypeScriptEmitter is the emitter's pre-condition: the
// @putnami/client the emitter shim belongs to is the version the workspace's
// bun.lock pins.
//
// The task's cache key folds bun.lock, and the engine never hashes
// node_modules. An install that lags the lock would write an older client,
// which the engine would store under the lock's key: a later install would not
// move that key, so every hit would restore the older client. A lock that pins
// no version, such as a workspace package, or no bun.lock, leaves nothing to
// compare.
func requireLockedTypeScriptEmitter(workspaceRoot, emitter string, generation TypeScriptGeneration) error {
	lock, err := os.ReadFile(filepath.Join(workspaceRoot, "bun.lock")) //nolint:gosec // G304: the workspace's own lockfile
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read the workspace's bun.lock: %w", err)
	}
	lockKeys := []string{clientPackage}
	if !strings.HasPrefix(emitter, filepath.Join(workspaceRoot, "node_modules")+string(filepath.Separator)) {
		// The client project's own install: bun keys it under the project's name.
		lockKeys = []string{generation.PackageName + "/" + clientPackage, clientPackage}
	}
	pinned := lockedVersion(lock, lockKeys)
	if pinned == "" {
		return nil
	}
	manifest := filepath.Join(filepath.Dir(filepath.Dir(emitter)), filepath.FromSlash(clientPackage), "package.json")
	installed, err := packageVersion(manifest)
	if err != nil {
		return fmt.Errorf("read the installed %s: %w; run `putnami deps install`", clientPackage, err)
	}
	if installed != pinned {
		return fmt.Errorf("the installed %s is %s, but bun.lock pins %s: the emitter would write a client the cache "+
			"key does not describe; run `putnami deps install` so the install matches the lockfile, then generate again",
			clientPackage, installed, pinned)
	}
	return nil
}

// clientPackage is the npm package that ships the TypeScript emitter.
const clientPackage = "@putnami/client"

// lockedVersion reads the version bun.lock pins for the first of keys it
// lists in its packages, or "" when it lists none or pins no registry
// version. A bun.lock package line reads
// `"<key>": ["<name>@<version>", ...`. A registry version starts with a digit
// and holds no colon or slash. Every other pin is a source, not a version:
// workspace:, link:, a URL, or the path bun writes for a file: tarball, which
// may be a bare file name such as 0.5.0-2.tgz.
func lockedVersion(lock []byte, keys []string) string {
	for _, key := range keys {
		marker := `"` + key + `": ["` + clientPackage + `@`
		for line := range strings.Lines(string(lock)) {
			rest, found := strings.CutPrefix(strings.TrimSpace(line), marker)
			if !found {
				continue
			}
			version, _, closed := strings.Cut(rest, `"`)
			if !closed || version == "" || version[0] < '0' || version[0] > '9' || strings.ContainsAny(version, ":/") || isTarballName(version) {
				return ""
			}
			return version
		}
	}
	return ""
}

func isTarballName(pin string) bool {
	for _, extension := range []string{".tgz", ".tar.gz", ".tar"} {
		if strings.HasSuffix(pin, extension) {
			return true
		}
	}
	return false
}

func packageVersion(manifest string) (string, error) {
	data, err := os.ReadFile(manifest) //nolint:gosec // G304: the installed package's manifest
	if err != nil {
		return "", err
	}
	var document struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("parse %s: %w", manifest, err)
	}
	return document.Version, nil
}

// firstPartyOperations counts the operations of an OpenAPI contract that no
// external authority owns: the operations a generated client carries. It walks
// the same methods the generator walks (httpMethods), and skips every other
// path-item member, such as parameters or summary. An operation whose external
// marker is invalid counts, since the emitter does not skip it.
func firstPartyOperations(contract []byte) (int, error) {
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(contract, &document); err != nil {
		return 0, err
	}
	count := 0
	for _, members := range document.Paths {
		for method, raw := range members {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			var operation openAPIOperation
			if err := json.Unmarshal(raw, &operation); err != nil {
				return 0, fmt.Errorf("operation %s: %w", method, err)
			}
			authority, diagnostics := clientcontract.ExternalContractAuthority(operation.Client, operation.External)
			if authority != "" && len(diagnostics) == 0 {
				continue
			}
			count++
		}
	}
	return count, nil
}
