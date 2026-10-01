package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// errGoUnavailable marks a Go project setup that found no go command, before
// and after installing the Go the workspace pins.
var errGoUnavailable = errors.New("no go command is available")

// goCommand is the go executable a new Go project is set up with, and the
// environment it runs in.
type goCommand struct {
	path string
	env  []string
	// installErr is the failure of the workspace installers that installed
	// this go (provisionGoCommand). It is nil when they succeeded or when a go
	// was already available.
	installErr error
}

// run runs `go <args>` in dir under the root context so a canceled create
// aborts the whole go toolchain process tree (go mod tidy can fan out to long
// network fetches). It uses the process-group helper to reach grandchildren.
// GOTOOLCHAIN=local keeps the resolved go from switching to another release.
func (g goCommand) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	env := append(append([]string(nil), g.env...), "GOTOOLCHAIN=local")
	return shared.RunGroupCombined(ctx, dir, env, g.path, args...)
}

// resolveGoCommand finds the go command the way a task of the template's
// extension finds it: the release the workspace lock pins, through the
// candidates and bindings the extension's run toolchains declare (a go on
// PATH, the Putnami home, the install inside the workspace). The CLI names no
// location and no release; the extension and the lock own both.
//
// When that resolution fails, a go on the ambient PATH still qualifies, as it
// did before tasks resolved their toolchains from the lock, so a host that set
// Go projects up before keeps doing so.
func resolveGoCommand(ctx context.Context, wsRoot, extensionName string) (goCommand, error) {
	base := os.Environ()
	env, err := templateToolchainEnvironment(ctx, wsRoot, extensionName, base)
	if err == nil {
		if path, lookErr := jobs.ProviderExecutable("go", env); lookErr == nil {
			return goCommand{path: path, env: env}, nil
		}
	}
	if path, lookErr := osexec.LookPath("go"); lookErr == nil {
		return goCommand{path: path, env: base}, nil
	}
	if err != nil {
		return goCommand{}, fmt.Errorf("%w: %w", errGoUnavailable, err)
	}
	return goCommand{}, fmt.Errorf("%w on PATH", errGoUnavailable)
}

// templateToolchainEnvironment applies the run toolchains of the extension
// that provided the template (jobs.RunToolchainEnvironment) to base. An
// extension the workspace does not discover leaves base as it is. Discovery
// builds a fresh description, so a second call after an install resolves
// again.
func templateToolchainEnvironment(ctx context.Context, wsRoot, extensionName string, base []string) ([]string, error) {
	discovered, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), discoverProjectPaths(wsRoot))
	if err != nil {
		return nil, fmt.Errorf("discover extension %s: %w", extensionName, err)
	}
	for _, ext := range discovered {
		if ext != nil && ext.Name == extensionName {
			return jobs.RunToolchainEnvironment(ctx, wsRoot, ext, base)
		}
	}
	return append([]string(nil), base...), nil
}

// ensureGoCommand returns the go command resolveGoCommand finds for
// extensionName and, on a host that offers none, installs the Go the
// workspace pins first (provisionGoCommand). goVersion is the go directive a
// missing go.work is written with; "" writes none.
func ensureGoCommand(ctx context.Context, wsRoot, projectPath, goVersion, extensionName string, env LifecycleEnv) (goCommand, error) {
	if command, err := resolveGoCommand(ctx, wsRoot, extensionName); err == nil {
		return command, nil
	}
	return provisionGoCommand(ctx, wsRoot, projectPath, goVersion, extensionName, env)
}

// goToolchainExtension names the extension whose runtime resolves the Go
// toolchain lock: of those the workspace discovers, the first by name. It is
// "" when none does, or when discovery fails.
func goToolchainExtension(wsRoot string) string {
	discovered, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), discoverProjectPaths(wsRoot))
	if err != nil {
		return ""
	}
	name := ""
	for _, ext := range discovered {
		if ext == nil || !slices.Contains(extension.RuntimeToolchainLocks(ext), "go") {
			continue
		}
		if name == "" || ext.Name < name {
			name = ext.Name
		}
	}
	return name
}

// provisionGoCommand installs the Go the workspace pins on a host that offers
// none, through the steps `putnami install` takes, and resolves it again.
//
// The lock pins Go from the go directive of go.work, and the extension's
// workspace-install installs the pinned release where its candidates look. A
// new workspace has neither, and go.work is the file the go command was about
// to create. So a missing go.work is written first, with the bytes that
// `go work init`, `go work use` and `go work edit -go` leave; the missing pin
// is filled next (the implicit install's lock step), and then every
// workspace-install runs. `putnami install` is not run: it also installs the
// extensions, the templates and the agent content, which are in place.
func provisionGoCommand(ctx context.Context, wsRoot, projectPath, goVersion, extensionName string, env LifecycleEnv) (goCommand, error) {
	iox.Fprintln(os.Stdout, "  No go command found; installing the Go the workspace lock pins…")
	goWork := filepath.Join(wsRoot, "go.work")
	if _, err := os.Stat(goWork); os.IsNotExist(err) && goVersion != "" {
		if err := os.WriteFile(goWork, []byte("go "+goVersion+"\n\nuse ./"+projectPath+"\n"), 0o644); err != nil {
			return goCommand{}, fmt.Errorf("write go.work: %w", err)
		}
		iox.Fprintf(os.Stdout, "  Initialized go.work\n")
	}
	var failures []error
	if _, err := fillImplicitToolchainPins(ctx, wsRoot); err != nil {
		failures = append(failures, fmt.Errorf("pin missing toolchains: %w", err))
	}
	installErr := DepsInstall(ctx, wsRoot, wsproto.Load(wsRoot), "", "", env)
	if installErr != nil {
		failures = append(failures, fmt.Errorf("install workspace dependencies: %w", installErr))
	}
	command, err := resolveGoCommand(ctx, wsRoot, extensionName)
	if err == nil {
		// The installers can fail on a dependency after they installed Go. The
		// setup goes on with that go, and the caller reports the failure.
		command.installErr = installErr
		return command, nil
	}
	return goCommand{}, errors.Join(append([]error{err}, failures...)...)
}
