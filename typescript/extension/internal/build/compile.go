package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
)

// execRunFunc is the function used to run subprocesses. It defaults to exec.Run
// but can be replaced in tests for isolation.
var execRunFunc = exec.Run

// DefaultBunTargets are the cross-compilation targets for bun build --compile.
//
// They are the DISTRIBUTION matrix, not a default every compile owes: a compile
// bound to a package channel resolves the one target that channel consumes (see
// ResolveCompileTargets).
var DefaultBunTargets = []string{
	"bun-linux-x64",
	"bun-linux-arm64",
	"bun-darwin-x64",
	"bun-darwin-arm64",
	"bun-windows-x64",
}

// DefaultDockerPlatform is the image platform assumed when none is named. It
// mirrors the `platform` flag default of the `package` command.
const DefaultDockerPlatform = "linux/amd64"

// CompileTarget maps a source file to an output path.
type CompileTarget struct {
	Source string
	Target string
}

// CompileTargetRequest carries the plan-time inputs that decide which bun
// targets a compile produces.
//
// Every field is a resolved job parameter the task declares as a cache-key
// input (`from: "params"`), never something read from the project tree or the
// machine at execution time: a target set the key cannot see would let a
// four-target entry answer a one-target request.
type CompileTargetRequest struct {
	// CompileTarget is an explicit `--compile-target`. It names one target and
	// wins over everything else.
	CompileTarget string
	// Docker reports that this invocation packages an image — the `--docker`
	// flag, or a project's `options.package.docker`. The `package` pipeline
	// schedules its compile step ONLY for the docker channel, so under that
	// intent the used target set is exactly the image's.
	Docker bool
	// DockerPlatform is the image platform (`--platform`), empty for the default.
	DockerPlatform string
}

// ResolveCompileTargets resolves the bun target set from plan-time inputs.
//
// An image carries ONE executable, for one os/arch. Compiling four and copying
// one in was three whole bun compiles discarded per packaged service, so the
// docker intent binds the compile to the image's target — the same mapping
// findBinaryForPlatform uses to pick the binary back out, so the file the
// packager looks for is by construction the file the compile wrote.
//
// Everything else keeps the full matrix: `build --compile` is not bound to a
// channel and its output is a distribution artifact set.
func ResolveCompileTargets(req CompileTargetRequest) ([]string, error) {
	if req.CompileTarget != "" {
		return []string{req.CompileTarget}, nil
	}
	if req.Docker {
		target, err := ImageCompileTarget(req.DockerPlatform)
		if err != nil {
			return nil, err
		}
		return []string{target}, nil
	}
	return append([]string(nil), DefaultBunTargets...), nil
}

// ImageCompileTarget maps a Docker platform ("linux/amd64") to the bun compile
// target that produces the executable an image of that platform runs
// ("bun-linux-x64").
func ImageCompileTarget(dockerPlatform string) (string, error) {
	if dockerPlatform == "" {
		dockerPlatform = DefaultDockerPlatform
	}
	osName, arch, ok := strings.Cut(dockerPlatform, "/")
	if !ok || osName == "" || arch == "" {
		return "", fmt.Errorf(
			"invalid platform format %q: expected an \"os/arch\" pair (e.g. %s)", dockerPlatform, DefaultDockerPlatform)
	}
	return "bun-" + osName + "-" + bunArch(arch), nil
}

// bunArch translates a Docker architecture name to bun's spelling. Only the
// names differ; the architecture does not.
func bunArch(arch string) string {
	switch arch {
	case "amd64", "x86_64":
		return "x64"
	case "aarch64":
		return "arm64"
	default:
		return arch
	}
}

// TargetSuffix is the filename suffix a compiled target carries
// ("bun-linux-x64" → "-linux-x64", "bun-windows-x64" → "-windows-x64.exe":
// Windows starts only a program whose name ends in ".exe"). It is the single
// source of that spelling: the compile writes it and the docker packager
// searches for it.
func TargetSuffix(target string) string {
	suffix := strings.Replace(target, "bun-", "-", 1)
	if strings.HasPrefix(target, "bun-windows-") {
		return suffix + ".exe"
	}
	return suffix
}

// resolveCompileOutputFile computes the output file path for a compile target.
func resolveCompileOutputFile(outputPath, entrypoint, target string) string {
	outputFile := filepath.Join(outputPath, filepath.Base(entrypoint))
	outputFile = strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + TargetSuffix(target)
	return outputFile
}

// buildCompileArgs builds the bun build --compile args. A non-empty config
// names the bunfig.toml that bun reads instead of the one in its working
// directory.
func buildCompileArgs(config, entrypoint, outputFile, target string) []string {
	args := []string{"build"}
	if config != "" {
		args = append(args, "--config="+config)
	}
	return append(args,
		"--compile",
		entrypoint,
		"--outfile", outputFile,
		"--target", target,
	)
}

// RunCompile invokes bun build --compile once per resolved target.
//
// The target set is a PARAMETER, never a decision made here: the caller
// resolves it from plan-time inputs (ResolveCompileTargets) so the set that
// produced a cache entry is the set its key was computed from.
//
// Bun runs from a directory of its own under outputPath, never from the
// project. bun build --compile writes each executable to a temporary
// ".<hash>-<n>.bun-build" file in its working directory, and extracts each
// cross-compilation runtime to a ".<hash>-<n>.tmp" directory there. In the
// project, git lists them as untracked files while the compile runs, so a
// source binding computed at that moment covers files no commit holds. Under
// outputPath, the final rename stays on one filesystem.
//
// The working directory decides two inputs, and both stay bound to the
// project: the entrypoint resolves against projectPath, and the project's
// bunfig.toml, which bun reads only from its working directory, is passed
// with --config.
func RunCompile(bunBin, projectPath, outputPath string, entrypoint string, targets []string) ([]string, []string, error) {
	outputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve compile output %s: %w", outputPath, err)
	}
	if err := os.MkdirAll(outputPath, 0755); err != nil {
		return nil, nil, fmt.Errorf("create compile output %s: %w", outputPath, err)
	}
	workDir, err := os.MkdirTemp(outputPath, ".bun-compile-")
	if err != nil {
		return nil, nil, fmt.Errorf("create compile working directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	entry := entrypoint
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(projectPath, entry)
	}
	config := filepath.Join(projectPath, "bunfig.toml")
	if info, statErr := os.Stat(config); statErr != nil || info.IsDir() {
		config = ""
	}

	var files []string
	var errors []string

	for _, target := range targets {
		outputFile := resolveCompileOutputFile(outputPath, entrypoint, target)

		args := buildCompileArgs(config, entry, outputFile, target)

		result, err := execRunFunc(bunBin, args, exec.Dir(workDir), exec.Timeout(5*time.Minute))
		if err != nil {
			errors = append(errors, fmt.Sprintf("Compile failed for %s [%s]: %v", entrypoint, target, err))
			continue
		}

		if !result.Success {
			stderr := strings.TrimSpace(result.Stderr)
			errors = append(errors, fmt.Sprintf("Compile failed for %s [%s]%s", entrypoint, target,
				cond(stderr != "", ":\n"+stderr, "")))
			continue
		}

		files = append(files, outputFile)
	}

	return files, errors, nil
}

func cond(test bool, ifTrue, ifFalse string) string {
	if test {
		return ifTrue
	}
	return ifFalse
}
