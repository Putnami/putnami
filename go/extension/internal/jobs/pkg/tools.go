package pkg

import (
	"bytes"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/go/extension/tools"
)

// stagedToolsDir is the archive-relative directory carrying the prebuilt pinned
// development tools. It sits beside the runtime binary in compiled/ because it
// is the same kind of content: per-platform bytes this project compiled, which
// travel with the platform archive and are never rebuilt on a consumer machine.
//
// toolchain.ExtensionArtifactToolPath and bash's _go_tool_artifact_binary read
// this exact layout out of an installed extension root, so a change here is a
// change to all three.
const stagedToolsDir = "tools"

// stagesPinnedTools reports whether the project being packaged is the Go
// extension itself — the only project whose archive owns the pinned tools.
//
// The marker is the staged tools/versions.json, the machine-readable pin
// manifest this extension ships. Keying on the manifest rather than on the
// package name means an ordinary Go project that happens to be packaged never
// pays four golangci-lint cross-compiles for tools it does not own.
func stagesPinnedTools(stageDir string) bool {
	return fileExists(filepath.Join(stageDir, stagedToolsDir, "versions.json"))
}

// stagePinnedTools builds every pinned development tool for one platform,
// stages it at compiled/tools/<tool> (compiled/tools/<tool>.exe for windows,
// the name toolchain.ExtensionArtifactToolPath reads there), and returns the
// slash-separated archive paths it staged, so the archive marks each one
// executable.
//
// The build is deliberately the SAME recipe InstallTool uses on a consumer
// machine: the pinned `install` coordinate, the LOCAL Go toolchain
// (GOTOOLCHAIN=local, never the one the tool's own go.mod requests), CGO off so
// the binary is portable, and -trimpath so it carries no build-machine paths.
// A consumer whose Go minor is this build's or an older one copies these bytes
// instead of spending 40 s of wall time and 113 s of CPU compiling them
// (toolchain.ToolServesLocalGo); on a 4-vCPU Windows host the compile takes
// about 4.8 minutes.
//
// Each built binary is verified through its own embedded build information
// before staging: packaging a tool that reports a version other than the pin
// would ship a silent downgrade to every machine that restores it.
//
// goMaxProcs is this platform's share of the task's CPU grant: several
// platforms build at the same time (archives.go), so each build gets its share
// rather than the whole grant.
func stagePinnedTools(
	emit *jsonl.Emitter,
	goBinary string,
	target platform.Target,
	stageDir string,
	goMaxProcs int,
) ([]string, error) {
	manifest := tools.ManifestContract()
	names := make([]string, 0, len(manifest.Tools))
	for name := range manifest.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	destDir := filepath.Join(stageDir, "compiled", stagedToolsDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", destDir, err)
	}

	gopath, err := os.MkdirTemp("", "putnami-go-pkg-tools-")
	if err != nil {
		return nil, fmt.Errorf("create temporary GOPATH: %w", err)
	}
	defer os.RemoveAll(gopath)

	build, err := newPinnedToolBuild(goBinary, gopath, goMaxProcs)
	if err != nil {
		return nil, err
	}
	if build.offline {
		emit.Log("info", "Building the pinned tools from the module cache: this run has no network")
	}

	staged := make([]string, 0, len(names))
	for _, name := range names {
		spec := manifest.Tools[name]
		built, err := buildPinnedTool(build, goBinary, target, name, spec.Install)
		if err != nil {
			return nil, err
		}
		if err := verifyPinnedToolBuild(built, name, spec.Version); err != nil {
			return nil, err
		}
		file := pkgmeta.ExecutableName(target.GOOS, name)
		dest := filepath.Join(destDir, file)
		if err := copyExecutable(built, dest); err != nil {
			return nil, fmt.Errorf("stage %s for %s: %w", name, target.Suffix, err)
		}
		staged = append(staged, "compiled/"+stagedToolsDir+"/"+file)
		if info, err := os.Stat(dest); err == nil {
			emit.Metric("tool-binary-bytes-"+name+"-"+target.Suffix, info.Size(), "bytes")
			emit.Log("info", fmt.Sprintf(
				"Staged %s %s for %s (%d bytes)", name, spec.Version, target.Suffix, info.Size()))
		}
	}
	return staged, nil
}

// pinnedToolBuild is the environment every pinned tool build of one packaging
// run shares: the throwaway GOPATH the binaries land in, and whether this run
// has a network at all.
type pinnedToolBuild struct {
	env     []string
	gopath  string
	offline bool
}

// newPinnedToolBuild resolves that environment.
//
// When the caller turned the network off (GOPROXY=off), GOPROXY points at the
// module cache's download directory and GOSUMDB is off: nothing is reachable
// but the materialized cache, and a gap in it is an immediate, offline failure.
// With the network on, the proxy and checksum settings are left as inherited.
//
// A positive goMaxProcs replaces the GOMAXPROCS inherited from the job. Zero or
// less keeps the inherited value.
func newPinnedToolBuild(goBinary, gopath string, goMaxProcs int) (pinnedToolBuild, error) {
	base := toolchain.GoCommandEnv(os.Environ(), goBinary)
	build := pinnedToolBuild{
		gopath:  gopath,
		offline: strings.TrimSpace(toolchain.EnvLookup(base)("GOPROXY")) == "off",
		env: appendEnvValues(base,
			"GOPATH="+gopath,
			"CGO_ENABLED=0",
			// go install pkg@version ignores the ambient module context by
			// design; GOFLAGS inherited from a caller (-mod=vendor,
			// -mod=readonly) would make it refuse to run at all.
			"GOFLAGS=-mod=mod",
		),
	}
	if goMaxProcs > 0 {
		build.env = appendEnvValues(build.env, "GOMAXPROCS="+strconv.Itoa(goMaxProcs))
	}
	if !build.offline {
		return build, nil
	}

	modCache := goEnvValue(goBinary, base, "GOMODCACHE")
	if modCache == "" {
		return pinnedToolBuild{}, errors.New(
			"cannot build the pinned tools offline: this run has GOPROXY=off and no module cache to read them from")
	}
	// The trailing `off` is what the caller asked for and what a gap in the
	// cache must still report: the file proxy answers, or nothing does.
	build.env = appendEnvValues(build.env,
		"GOPROXY="+moduleCacheProxy(modCache)+",off",
		"GOSUMDB=off",
	)
	return build, nil
}

// moduleCacheProxy is the file:// proxy URL of a module cache's download
// directory. Go writes that tree in the module proxy layout it reads, so the
// cache doubles as an offline proxy with no copying and no second store.
func moduleCacheProxy(modCache string) string {
	path := filepath.ToSlash(filepath.Join(modCache, "cache", "download"))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// goEnvValue asks the toolchain for one resolved variable. `go env` is the only
// answer that accounts for Go's own environment file, which this process cannot
// see: a machine with `go env -w GOMODCACHE=...` keeps its module cache where
// it put it.
func goEnvValue(goBinary string, env []string, name string) string {
	cmd := exec.Command(goBinary, "env", name)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildPinnedTool compiles one tool for one platform and returns its path.
//
// `go install pkg@version` refuses to honor GOBIN while cross-compiling, so
// the output lands in $GOPATH/bin/<goos>_<goarch> for a foreign platform and in
// $GOPATH/bin for the host. Both are probed, under the file name `go install`
// gives a windows program (<tool>.exe).
func buildPinnedTool(build pinnedToolBuild, goBinary string, target platform.Target, tool, install string) (string, error) {
	cmd := exec.Command(goBinary, "install", "-trimpath", install)
	cmd.Env = appendEnvValues(append([]string(nil), build.env...),
		"GOOS="+target.GOOS, "GOARCH="+target.GOARCH)
	// `go install` exits 1 and says everything that matters on stderr. Handing
	// that stream to os.Stderr and returning the bare exit status put the real
	// cause — "module lookup disabled by GOPROXY=off" — in the raw run log
	// while the task diagnostic read only "exit status 1".
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build %s for %s/%s: %w%s%s",
			tool, target.GOOS, target.GOARCH, err, goFailureDetail(stderr.Bytes()), build.offlineHint())
	}

	file := pkgmeta.ExecutableName(target.GOOS, tool)
	for _, candidate := range []string{
		filepath.Join(build.gopath, "bin", target.GOOS+"_"+target.GOARCH, file),
		filepath.Join(build.gopath, "bin", file),
	} {
		if fileExists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("build %s for %s/%s produced no binary under %s",
		tool, target.GOOS, target.GOARCH, build.gopath)
}

// offlineHint names the step that owns the bytes an offline build is missing.
// The failure surfaces here, hours of task graph away from the warm-up that
// should have prevented it, so the message carries the address of the fix.
func (b pinnedToolBuild) offlineHint() string {
	if !b.offline {
		return ""
	}
	return "\n(this run has no network: `putnami install` warms the pinned tool sources — " +
		"@putnami/go workspace-install, phase `tools`)"
}

// goFailureDetail is the tail of a failed go command's stderr, bounded so one
// unlucky command cannot turn a diagnostic into a log dump. The tail is the
// half that matters: `go` prints progress first and the reason last.
func goFailureDetail(stderr []byte) string {
	const limit = 4000
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return ""
	}
	if len(text) > limit {
		text = "…" + text[len(text)-limit:]
	}
	return "\n" + text
}

// verifyPinnedToolBuild refuses a binary whose embedded main-module version is
// not the pin. debug/buildinfo reads the binary's own record, so the check works
// for a foreign platform that cannot be executed here — the same record the
// consumer side reads before restoring it.
func verifyPinnedToolBuild(path, tool, version string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read build info of %s: %w", path, err)
	}
	if info.Main.Version != version {
		return fmt.Errorf(
			"built %s embeds %q, expected the pinned %s — refusing to package it",
			tool, info.Main.Version, version)
	}
	return nil
}

// copyExecutable writes src to dest with the executable bit set.
func copyExecutable(src, dest string) (resultErr error) {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}

// appendEnvValues sets each KEY=VALUE, replacing an existing entry for the key.
// An entry without `=` is not an assignment and is skipped rather than appended.
func appendEnvValues(env []string, values ...string) []string {
	out := env
	for _, value := range values {
		key, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		replaced := false
		for i, entry := range out {
			if entryKey, _, ok := strings.Cut(entry, "="); ok && entryKey == key {
				out[i] = value
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, value)
		}
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
