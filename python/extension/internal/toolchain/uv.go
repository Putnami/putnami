package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

// uvToolchainName names uv in the toolchains subtree of a Putnami home.
const uvToolchainName = "uv"

// uvVersionTimeout bounds the uvVersionArgs probe that tells which release a
// uv program is.
const uvVersionTimeout = 30 * time.Second

// uvVersionArgs make a uv program print its release and nothing else. Without
// --color never, a FORCE_COLOR in the environment wraps the release in color
// codes, and the output matches no release. The extension manifest declares
// the same arguments as the probe of its uv runtime toolchain.
var uvVersionArgs = []string{"self", "version", "--short", "--color", "never"}

// uvHost is what selecting a uv reads from the machine.
type uvHost struct {
	// goos and goarch are the platform a uv is selected for.
	goos, goarch string
	// lookup reads one environment variable.
	lookup func(string) string
	// lookPath finds a program on PATH.
	lookPath func(file string) (string, error)
	// version returns the release the uv program at path reports.
	version func(path string) (string, error)
}

// currentUVHost is the machine this process runs on.
func currentUVHost() uvHost {
	return uvHost{
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
		lookup:   os.Getenv,
		lookPath: exec.LookPath,
		version:  UVVersion,
	}
}

// UVVersion returns the release the uv program at path reports with
// uvVersionArgs, the probe the extension manifest declares for its uv runtime
// toolchain.
func UVVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), uvVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, uvVersionArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("uv self version did not answer within %s", uvVersionTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("uv self version failed (%w): %s", err, TailText(stderr.String(), "", 3))
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveUV returns the uv program a task of workspaceRoot runs: the release
// the workspace lock pins, else the one the root pyproject.toml declares as
// [tool.uv] required-version, else DefaultUVVersion.
//
// The candidates are the ones the extension manifest declares for its uv
// runtime toolchain, in the same order: uv on PATH, where the CLI puts the uv
// it resolved for the task first, then the release the CLI provisioned under
// the Putnami home (see uvSearch.provisioned). Only a uv that reports the
// release with `uv self version --short` qualifies: a uv of another release
// is never run. ResolveUV installs nothing, and its error names the release
// and `putnami install`, which provisions it.
func ResolveUV(workspaceRoot string) (string, error) {
	return resolveUV(currentUVHost(), workspaceRoot)
}

func resolveUV(host uvHost, workspaceRoot string) (string, error) {
	want, err := wantedUV(workspaceRoot)
	if err != nil {
		return "", err
	}
	search := newUVSearch(host, workspaceRoot, want)
	if uv, ok := search.find(); ok {
		return uv.Path, nil
	}
	return "", fmt.Errorf("no uv %s on this machine%s: run `putnami install`, which provisions it under the Putnami home",
		want.version, search.seen())
}

// uvWant is the release a workspace asks for.
type uvWant struct {
	// version is the exact release.
	version string
}

// uvDeclarationOrigin names the declaration a workspace makes in its root
// pyproject.toml.
const uvDeclarationOrigin = RootPyproject + " [tool.uv] required-version"

// wantedUV reads the release workspaceRoot asks for: the one its lock pins,
// else the one its root pyproject.toml declares, else DefaultUVVersion.
//
// A declaration that uvtoolchain refuses (Declaration.Problem) fails whether
// or not the lock pins a release. The CLI pins nothing from a required-version
// that names no single release, such as a range, so the workspace would
// otherwise run a release it never declared. No probe can tell a release
// older than uvtoolchain.MinimumVersion. A pinned release older than that
// minimum fails too.
func wantedUV(workspaceRoot string) (uvWant, error) {
	declaration, err := uvtoolchain.ReadWorkspaceDeclaration(workspaceRoot)
	if err != nil {
		return uvWant{}, err
	}
	if err := declaration.Problem(); err != nil {
		return uvWant{}, fmt.Errorf("%w, then run `putnami install`", err)
	}
	locked, pinned, err := lockedUVRelease(workspaceRoot)
	if err != nil {
		return uvWant{}, err
	}
	if pinned {
		if err := uvtoolchain.Supported(locked.Version); err != nil {
			return uvWant{}, fmt.Errorf("toolchains.uv in %s pins a release Putnami cannot run: %w: "+
				"declare required-version = \"==%s\" or a later release in the [tool.uv] table of %s, then run `putnami install`",
				WorkspaceLockFile, err, uvtoolchain.MinimumVersion, RootPyproject)
		}
		return uvWant{version: locked.Version}, nil
	}
	if declaration.Version != "" {
		return uvWant{version: declaration.Version}, nil
	}
	return uvWant{version: DefaultUVVersion}, nil
}

// UV is the uv a job runs.
type UV struct {
	// Path is the uv program.
	Path string
	// Version is the release it reports.
	Version string
}

// uvSearch is one selection of a uv, with what it saw on the way.
type uvSearch struct {
	host uvHost
	want uvWant
	// dir is the directory of the wanted release under the Putnami home:
	// toolchains/uv/uv-<version>.
	dir string
	// other lists the candidates that report another release, or none.
	other []string
}

func newUVSearch(host uvHost, workspaceRoot string, want uvWant) *uvSearch {
	root := putnamihome.ToolchainRoot(host.lookup, workspaceRoot, uvToolchainName)
	return &uvSearch{host: host, want: want, dir: managedUVDir(root, want.version)}
}

// managedUVDir is the directory of a uv release under root, the
// toolchains/uv directory of a Putnami home.
func managedUVDir(root, version string) string {
	return filepath.Join(root, "uv-"+version)
}

// provisioned returns the uv program of the wanted release under the Putnami
// home, and "" when the CLI provisioned none. The CLI unpacks the release's
// archive in its directory, so the program is where the archive holds it, the
// executable the toolchain-resolve answer names (uvLockPin): uv-<target>/uv for
// a tar.gz archive, and uv at the top of the directory for a zip archive. A
// uv at the top of a tar.gz release directory qualifies as well.
func (s *uvSearch) provisioned() string {
	program := uvProgram(s.host.goos)
	var candidates []string
	if archive, ok := uvArchiveFor(UVRelease{Version: s.want.version}, s.host.goos, s.host.goarch); ok {
		candidates = append(candidates, filepath.Join(s.dir, archive.dir, program))
	}
	for _, candidate := range append(candidates, filepath.Join(s.dir, program)) {
		if isRegularFile(candidate) {
			return candidate
		}
	}
	return ""
}

// find returns the first candidate that reports the wanted release: uv on
// PATH, then the release the CLI provisioned.
func (s *uvSearch) find() (UV, bool) {
	var candidates []string
	if onPath, err := s.host.lookPath("uv"); err == nil {
		candidates = append(candidates, onPath)
	}
	if provisioned := s.provisioned(); provisioned != "" {
		candidates = append(candidates, provisioned)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		got, err := s.host.version(candidate)
		switch {
		case err != nil:
			s.other = append(s.other, candidate+" (reports no version: "+err.Error()+")")
		case got != s.want.version:
			s.other = append(s.other, candidate+" (uv "+got+")")
		default:
			return UV{Path: candidate, Version: got}, true
		}
	}
	return UV{}, false
}

// seen describes the candidates the search passed over, for the end of an
// error.
func (s *uvSearch) seen() string {
	if len(s.other) == 0 {
		return ""
	}
	return " (found " + strings.Join(s.other, ", ") + ")"
}

// isRegularFile reports whether path names a regular file, through symbolic
// links.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
