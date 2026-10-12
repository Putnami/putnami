package toolchain

import (
	"context"
	"fmt"
	"net/http"
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
	// client downloads a release. Nil selects the client of pinnedarchive.
	client *http.Client
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
// it resolved for the task first, then toolchains/uv/uv-<version>/uv under the
// Putnami home, where `putnami install` installs it. Only a uv that reports
// the release with `uv self version --short` qualifies: a uv of another release
// is never run. ResolveUV installs nothing, and its error names
// `putnami install`.
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
	return "", fmt.Errorf("no uv %s on this machine%s: run `putnami install`, which installs it under the Putnami home",
		want.version, search.seen())
}

// uvWant is the release a workspace asks for.
type uvWant struct {
	// version is the exact release.
	version string
	// origin names what names version.
	origin string
	// release is what an install of the wanted release is verified with. Its
	// Version is "" when no digest is known for that release.
	release UVRelease
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
		return uvWant{version: locked.Version, origin: WorkspaceLockFile, release: locked}, nil
	}
	if declaration.Version != "" {
		want := uvWant{version: declaration.Version, origin: uvDeclarationOrigin}
		if declaration.Version == DefaultUVVersion {
			want.release = DefaultUVRelease()
		}
		return want, nil
	}
	return uvWant{version: DefaultUVVersion, origin: "the @putnami/python extension", release: DefaultUVRelease()}, nil
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
	// dir is the install directory of the wanted release under the Putnami
	// home: toolchains/uv/uv-<version>.
	dir string
	// other lists the candidates that report another release, or none.
	other []string
}

func newUVSearch(host uvHost, workspaceRoot string, want uvWant) *uvSearch {
	root := putnamihome.ToolchainRoot(host.lookup, workspaceRoot, uvToolchainName)
	return &uvSearch{host: host, want: want, dir: managedUVDir(root, want.version)}
}

// managedUVDir is the directory of the uv version Putnami installs under
// root, the toolchains/uv directory of a Putnami home.
func managedUVDir(root, version string) string {
	return filepath.Join(root, "uv-"+version)
}

// program is the uv program of the managed install.
func (s *uvSearch) program() string {
	return filepath.Join(s.dir, uvProgram(s.host.goos))
}

// find returns the first candidate that reports the wanted release: uv on
// PATH, then the managed install.
func (s *uvSearch) find() (UV, bool) {
	var candidates []string
	if onPath, err := s.host.lookPath("uv"); err == nil {
		candidates = append(candidates, onPath)
	}
	if managed := s.program(); isRegularFile(managed) {
		candidates = append(candidates, managed)
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
