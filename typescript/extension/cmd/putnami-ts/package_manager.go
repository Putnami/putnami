package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.putnami.dev/typescript/extension/internal/pnpmworkspace"
)

// The package managers a workspace install runs.
const (
	managerBun  = "bun"
	managerNPM  = "npm"
	managerPNPM = "pnpm"
	managerYarn = "yarn"
)

// rootLockfiles are the lockfiles that name a workspace's package manager when
// its root package.json declares none, in the order they are looked for.
var rootLockfiles = []struct {
	file    string
	manager string
}{
	{"bun.lock", managerBun},
	{"bun.lockb", managerBun},
	{"pnpm-lock.yaml", managerPNPM},
	{"yarn.lock", managerYarn},
	{"package-lock.json", managerNPM},
	{"npm-shrinkwrap.json", managerNPM},
}

// yarnBerryConfig is the configuration file of Yarn 2 and later, which Yarn 1
// does not read.
const yarnBerryConfig = ".yarnrc.yml"

// packageManager is the package manager a workspace installs with.
type packageManager struct {
	// Name is bun, npm, pnpm or yarn; for a declaration the extension does not
	// install with, the declared name.
	Name string
	// Version is the version packageManager declares, "" when the manager comes
	// from a lockfile or is the default.
	Version string
	// Berry reports a Yarn 2 or later.
	Berry bool
	// Source names what chose the manager: package.json#packageManager, a
	// lockfile, or "" for the default.
	Source string
}

// label names the manager and what chose it, for a message.
func (m packageManager) label() string {
	if m.Source == "" {
		return m.Name + " (the default)"
	}
	return m.Name + " (" + m.Source + ")"
}

// readsCatalogs reports whether the manager resolves the catalog: protocol.
func (m packageManager) readsCatalogs() bool {
	return m.Name == managerBun || m.Name == managerPNPM
}

// listsMembersInPNPMWorkspace reports whether the manager reads its members
// from pnpm-workspace.yaml instead of the root package.json workspaces.
func (m packageManager) listsMembersInPNPMWorkspace() bool {
	return m.Name == managerPNPM
}

// lockfiles are the lockfiles the manager writes and reads, the first one the
// one it writes in a workspace that has none.
func (m packageManager) lockfiles() []string {
	switch m.Name {
	case managerBun:
		return []string{"bun.lock", "bun.lockb"}
	case managerPNPM:
		return []string{"pnpm-lock.yaml"}
	case managerYarn:
		return []string{"yarn.lock"}
	case managerNPM:
		return []string{"package-lock.json", "npm-shrinkwrap.json"}
	}
	return nil
}

// errUnknownPackageManager is the failure of a packageManager the extension
// does not install with.
var errUnknownPackageManager = errors.New("@putnami/typescript installs with bun, npm, pnpm or yarn")

// detectPackageManager returns the package manager the workspace at root
// declares: the packageManager field of its root package.json, else the
// manager whose lockfile lies at the root (rootLockfiles), else Bun. A
// declaration of another manager returns that name and an error wrapping
// errUnknownPackageManager; a root package.json it cannot read is an error.
func detectPackageManager(root string) (packageManager, error) {
	declared, err := declaredPackageManager(root)
	if err != nil {
		return packageManager{}, err
	}
	if declared != "" {
		name, version, _ := strings.Cut(declared, "@")
		version, _, _ = strings.Cut(version, "+")
		m := packageManager{Name: strings.TrimSpace(name), Version: strings.TrimSpace(version), Source: "package.json#packageManager"}
		switch m.Name {
		case managerBun, managerNPM, managerPNPM:
		case managerYarn:
			m.Berry = yarnIsBerry(root, m.Version)
		default:
			return m, fmt.Errorf("package.json#packageManager declares %q: %w; declare one of them, or remove the field to install with the manager whose lockfile lies at the root", declared, errUnknownPackageManager)
		}
		return m, nil
	}
	for _, lock := range rootLockfiles {
		if !isRegularFile(filepath.Join(root, lock.file)) {
			continue
		}
		m := packageManager{Name: lock.manager, Source: lock.file}
		if m.Name == managerYarn {
			m.Berry = yarnIsBerry(root, "")
		}
		return m, nil
	}
	return packageManager{Name: managerBun}, nil
}

// declaredPackageManager returns the packageManager field of the root
// package.json, "" when the file or the field is missing.
func declaredPackageManager(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read package.json: %w", err)
	}
	var pkg struct {
		PackageManager json.RawMessage `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("parse package.json: %w", err)
	}
	if len(pkg.PackageManager) == 0 || string(pkg.PackageManager) == "null" {
		return "", nil
	}
	var declared string
	if err := json.Unmarshal(pkg.PackageManager, &declared); err != nil {
		return "", fmt.Errorf("package.json#packageManager is not a string: %w", err)
	}
	return strings.TrimSpace(declared), nil
}

// yarnIsBerry reports whether the workspace runs Yarn 2 or later: the major of
// the declared version when there is one, else a .yarnrc.yml at the root or a
// yarn.lock in the Yarn 2 format, which opens with a __metadata entry.
func yarnIsBerry(root, version string) bool {
	if major, _, _ := strings.Cut(version, "."); major != "" {
		if n, err := strconv.Atoi(major); err == nil {
			return n >= 2
		}
	}
	if isRegularFile(filepath.Join(root, yarnBerryConfig)) {
		return true
	}
	file, err := os.Open(filepath.Join(root, "yarn.lock"))
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for lines := 0; scanner.Scan() && lines < 20; lines++ {
		if strings.HasPrefix(scanner.Text(), "__metadata:") {
			return true
		}
	}
	return false
}

// lookPath finds a package manager on PATH. Tests replace it.
var lookPath = stdexec.LookPath

// resolveManagerBin returns the executable the workspace installs with: the
// bun the CLI resolved for Bun, and the npm, pnpm or yarn on PATH otherwise.
// The CLI provisions only Bun; the repository brings its other manager, through
// Corepack or a global install.
func resolveManagerBin(m packageManager) (string, error) {
	if m.Name == managerBun {
		return resolveBunBin()
	}
	bin, err := lookPath(m.Name)
	if err != nil {
		return "", fmt.Errorf("the workspace installs with %s, and no %s is on PATH: install %s, or run `corepack enable`, then run `putnami install` again",
			m.label(), m.Name, m.Name)
	}
	return bin, nil
}

// installInvocation is one run of a package manager: its arguments and the
// environment it adds.
type installInvocation struct {
	args []string
	env  map[string]string
}

// workspaceInstallInvocation returns the install the workspace runs with m. A
// hosted run installs frozen from the cache workspace-fetch filled, offline
// where the manager can: bun install --frozen-lockfile, npm ci --offline,
// pnpm install --frozen-lockfile --offline, yarn install --frozen-lockfile
// --offline on Yarn 1, and yarn install --immutable with the network off on
// Yarn 2 and later. force reinstalls with the manager's own flag; Yarn 2 and
// later and a hosted npm, which installs clean, have none.
func workspaceInstallInvocation(m packageManager, force, hosted bool) installInvocation {
	switch m.Name {
	case managerNPM:
		if hosted {
			return installInvocation{args: []string{"ci", "--offline"}}
		}
		args := []string{"install"}
		if force {
			args = append(args, "--force")
		}
		return installInvocation{args: args}
	case managerPNPM:
		args := []string{"install"}
		if hosted {
			args = append(args, "--frozen-lockfile", "--offline")
		}
		if force {
			args = append(args, "--force")
		}
		return installInvocation{args: args}
	case managerYarn:
		if m.Berry {
			if hosted {
				return installInvocation{args: []string{"install", "--immutable"}, env: map[string]string{"YARN_ENABLE_NETWORK": "0"}}
			}
			return installInvocation{args: []string{"install"}}
		}
		args := []string{"install", "--non-interactive"}
		if hosted {
			args = append(args, "--frozen-lockfile", "--offline")
		}
		if force {
			args = append(args, "--force")
		}
		return installInvocation{args: args}
	}
	return installInvocation{args: workspaceInstallArgs(force, hosted)}
}

// pnpmWorkspaceFile is the path of pnpm-workspace.yaml under root.
func pnpmWorkspaceFile(root string) string {
	return filepath.Join(root, pnpmworkspace.File)
}
