package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Workspace-derived image pins, the image-project directory
// resolver, and the streamed-output adapter — everything that outlived
// `putnami cloud image-build` when the runner became a `type: "image"` project.
//
// The retired hook read each source of truth and forwarded it as a Docker build
// arg. The build arg is gone with the Dockerfile; the READERS are not, because
// they were never about docker: they are the single derivation of every version
// the runner image bakes, and `cloud image-layers` (imagelayers.go) resolves its
// layers through this very table. Keeping their `imageBuild*` names is
// deliberate: renaming them would churn every producer and test that already
// cites them.
//
// A MISSING source is not an error: a workspace that declares no bun, no node
// or no lint tools simply derives nothing for that axis, and the consumer
// decides whether that is fatal (the layer producers say so loudly; the runner
// declares every axis). A source that EXISTS but cannot be parsed IS an error —
// that is drift, not absence, and degrading it to "no pin" sends the reader to
// entirely the wrong problem.

// imageBuildPinArgCLI is the pin name the CLI layer derives under. The lock is
// its source, not a second comparison axis.
const imageBuildPinArgCLI = "PUTNAMI_VERSION"

// imageBuildPin is one derived pin: the name a layer references it by
// (imageLayerSpec.Pin) and how to read it from the workspace. Slice order is
// table order, so diagnostics and the derived set stay stable and diffable.
type imageBuildPin struct {
	arg  string
	read func(workspaceRoot string) (string, error)
}

// imageBuildPins is the whole derivation table. Its axes mirror the CI runner
// image's pin-guard.sh (`pin-guard.sh sources`), and the runner image's tests
// keep the two from drifting apart.
func imageBuildPins() []imageBuildPin {
	return []imageBuildPin{
		{arg: imageBuildPinArgCLI, read: imageBuildLockCLIVersion},
		{arg: "GO_VERSION", read: imageBuildGoWorkVersion},
		{arg: "GOLANGCI_LINT_VERSION", read: imageBuildGoToolVersion("golangci-lint")},
		{arg: "STATICCHECK_VERSION", read: imageBuildGoToolVersion("staticcheck")},
		{arg: "BUN_VERSION", read: imageBuildPackageManagerVersion},
		{arg: "NODE_VERSION", read: imageBuildEnginesNodeVersion},
		{arg: "PUTNAMI_GO_EXTENSION_VERSION", read: imageBuildLockExtensionVersion("@putnami/go")},
		{arg: "PUTNAMI_TYPESCRIPT_EXTENSION_VERSION", read: imageBuildLockExtensionVersion("@putnami/typescript")},
		{arg: "PUTNAMI_SDD_EXTENSION_VERSION", read: imageBuildLockExtensionVersion("@putnami/sdd")},
	}
}

// imageBuildWorkspacePins derives every pin from its source and returns the
// non-empty ones as `NAME=value` pairs, in table order. Layer producers consume
// the same source readers, so the table remains the testable workspace
// toolchain contract even though CI delivery no longer mirrors it into a ledger.
func imageBuildWorkspacePins(workspaceRoot string) ([]string, error) {
	if workspaceRoot == "" {
		return nil, nil
	}
	var pins []string
	for _, pin := range imageBuildPins() {
		value, err := pin.read(workspaceRoot)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pin.arg, err)
		}
		if value == "" {
			continue
		}
		pins = append(pins, pin.arg+"="+value)
	}
	return pins, nil
}

// imageBuildLockCLIVersion reads the workspace CLI pin from putnami.lock.json.
func imageBuildLockCLIVersion(workspaceRoot string) (string, error) {
	lockPath := filepath.Join(workspaceRoot, "putnami.lock.json")
	var lock struct {
		CLI struct {
			Version string `json:"version"`
		} `json:"cli"`
	}
	if err := imageBuildReadJSON(lockPath, &lock); err != nil {
		return "", err
	}
	return strings.TrimSpace(lock.CLI.Version), nil
}

// imageBuildLockExtensionVersion reads the exact extension version the lock
// resolved. The runner's warm layer (imagelayers.go) consumes
// these references directly — they become the throwaway manifest it
// materializes against — instead of resolving an unversioned "latest"
// extension and comparing it with another copied tool pin.
func imageBuildLockExtensionVersion(name string) func(string) (string, error) {
	return func(workspaceRoot string) (string, error) {
		lockPath := filepath.Join(workspaceRoot, "putnami.lock.json")
		var lock struct {
			Extensions map[string]struct {
				Version string `json:"version"`
			} `json:"extensions"`
		}
		if err := imageBuildReadJSON(lockPath, &lock); err != nil {
			return "", err
		}
		return strings.TrimSpace(lock.Extensions[name].Version), nil
	}
}

// imageBuildGoWorkVersion reads the Go version the workspace requires from
// go.work. A `toolchain` line wins over the `go` line when both are present:
// that is Go's own precedence, and reading the weaker one would let the image
// bake a toolchain the workspace already declared insufficient.
func imageBuildGoWorkVersion(workspaceRoot string) (string, error) {
	path := filepath.Join(workspaceRoot, "go.work")
	data, err := os.ReadFile(path) //nolint:gosec // G304: a workspace-root manifest path, not user input
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var goDirective, toolchain string
	for _, raw := range strings.Split(string(data), "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "go":
			if goDirective == "" {
				goDirective = fields[1]
			}
		case "toolchain":
			if toolchain == "" {
				toolchain = strings.TrimPrefix(fields[1], "go")
			}
		}
	}
	version := goDirective
	if toolchain != "" {
		version = toolchain
	}
	if version == "" {
		return "", nil
	}
	if !imageBuildGoVersionPattern.MatchString(version) {
		// The file exists and states something this cannot read, which is drift
		// rather than absence.
		return "", fmt.Errorf("%s declares an unreadable Go version %q", path, version)
	}
	return version, nil
}

// imageBuildGoVersionPattern is the shape a Go toolchain version may take. It is
// deliberately strict: the value selects the toolchain tarball the Go layer is
// built from, and in the image it becomes the runner's toolchain FLOOR, which
// the runner library (ci-run-lib.sh) derives from /usr/local/go/VERSION and compares numerically.
var imageBuildGoVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// imageBuildGoToolPath locates the @putnami/go extension's tool manifest. It is
// the workspace-local symlink into the content-addressed store `putnami
// install` populates — deliberately not a search of ~/.putnami, whose layout
// has no `putnami-go` path component.
func imageBuildGoToolPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "bin", "extensions", "putnami-go", "tools", "versions.json")
}

// imageBuildGoToolVersion reads one lint-tool pin from the go extension's
// tools/versions.json, with the leading `v` stripped so it compares directly
// against the module version the layer producer builds and verifies.
func imageBuildGoToolVersion(tool string) func(string) (string, error) {
	return func(workspaceRoot string) (string, error) {
		path := imageBuildGoToolPath(workspaceRoot)
		var manifest struct {
			Tools map[string]struct {
				Version string `json:"version"`
			} `json:"tools"`
		}
		if err := imageBuildReadJSON(path, &manifest); err != nil {
			return "", err
		}
		return strings.TrimPrefix(strings.TrimSpace(manifest.Tools[tool].Version), "v"), nil
	}
}

// imageBuildPackageManagerVersion reads the Bun pin from package.json's
// `packageManager` field — the workspace's declared JavaScript runtime, in the
// standard `name@version` form. A workspace on a different package manager
// declares no bun pin.
func imageBuildPackageManagerVersion(workspaceRoot string) (string, error) {
	var manifest struct {
		PackageManager string `json:"packageManager"`
	}
	if err := imageBuildReadJSON(filepath.Join(workspaceRoot, "package.json"), &manifest); err != nil {
		return "", err
	}
	spec := strings.TrimSpace(manifest.PackageManager)
	if !strings.HasPrefix(spec, "bun@") {
		return "", nil
	}
	// Corepack allows a `+<hash>` integrity suffix; the version is what precedes it.
	version := strings.TrimPrefix(spec, "bun@")
	if plus := strings.IndexByte(version, '+'); plus >= 0 {
		version = version[:plus]
	}
	return strings.TrimSpace(version), nil
}

// imageBuildEnginesNodeVersion reads the Node pin from package.json `engines.node`.
// Only an EXACT version is a pin: a range ("^24", ">=20") states what the
// workspace tolerates, not what CI must execute, and resolving it would make the
// image's assertion compare a version against a constraint.
func imageBuildEnginesNodeVersion(workspaceRoot string) (string, error) {
	var manifest struct {
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := imageBuildReadJSON(filepath.Join(workspaceRoot, "package.json"), &manifest); err != nil {
		return "", err
	}
	version := strings.TrimSpace(manifest.Engines.Node)
	if version == "" || strings.ContainsAny(version, "^~<>=* |x") {
		return "", nil
	}
	return version, nil
}

// imageBuildReadJSON decodes a workspace manifest. An absent file is absence,
// not failure (see the file note); an unreadable or malformed one is an error.
func imageBuildReadJSON(path string, into any) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a workspace-root manifest path, not user input
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// imageBuildLookPath is the tool-discovery seam the layer producers compile
// through; a package var so tests exercise the missing-toolchain path without
// touching PATH.
var imageBuildLookPath = exec.LookPath

// imageProjectRoot resolves the image project directory an image-* verb operates
// on (`cloud image-layers`). An explicit directory param wins; otherwise the
// project the native CLI resolved for this task (context.project → params.app
// via adoptContextProject) is located BY NAME under the workspace root — and a
// lookup failure is surfaced, not swallowed: falling back to cwd here would
// operate on whatever directory the dispatch happened to land in, because the
// runtime runs in the task's declared cwd, not the caller's. The process cwd is
// only trusted when the CLI resolved no project at all (a bare invocation from
// inside an image project).
func imageProjectRoot(params map[string]any, workspaceRoot string, explicit ...string) (string, error) {
	if dir := clicore.StringParam(params, explicit...); dir != "" {
		// A RELATIVE directory is resolved against the workspace root first. The
		// runtime does not run in the caller's cwd — it runs in the task's
		// declared cwd — so `--project images/ci-runner`, the form this
		// package's own diagnostics tell an operator to type, would otherwise resolve against that directory and report a
		// missing manifest for a project that is right there. The cwd
		// form still works for anything the workspace root does not answer.
		if !filepath.IsAbs(dir) && workspaceRoot != "" {
			candidate := filepath.Join(workspaceRoot, filepath.FromSlash(dir))
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate, nil
			}
		}
		return filepath.Abs(dir)
	}
	if app := clicore.StringParam(params, "app", "appName"); app != "" && workspaceRoot != "" {
		dir, err := clicore.FindAppDir(workspaceRoot, app)
		if err != nil {
			return "", fmt.Errorf("locate project %q under %s: %w", app, workspaceRoot, err)
		}
		return dir, nil
	}
	return filepath.Abs(".")
}

// imageBuildLineWriter adapts a line-callback IO sink to io.Writer so a long
// subprocess's streamed output surfaces through the command IO as it happens.
type imageBuildLineWriter struct {
	emit func(string)
	buf  []byte
}

func (w *imageBuildLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emitLine(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *imageBuildLineWriter) flush() {
	if len(w.buf) > 0 {
		w.emitLine(string(w.buf))
		w.buf = nil
	}
}

func (w *imageBuildLineWriter) emitLine(line string) {
	if w.emit != nil {
		w.emit(line)
	}
}
