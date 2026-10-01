// Package hooks implements lifecycle hook execution for the two kinds of hook
// the CLI runs:
//
//   - Extension hooks (hooks.go) — subprocess tasks declared in an extension
//     manifest that run around job execution (preBuild, onInstall).
//   - Workspace hooks (cli_hooks.go) — shell commands declared in
//     putnami.workspace.json that bracket a whole invocation (hooks.cli.*,
//     hooks.commands.*).
//
// They run under ONE contract, which is the point of keeping them together:
// bounded by a timeout (DefaultHookTimeoutMs unless the hook overrides it), from
// an explicit working directory, with an environment the CLI states rather than
// inherits wholesale. Workspace hooks used to run under a weaker one — no
// timeout, a login shell, the invocation directory — until that gap closed.
package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// DefaultHookTimeoutMs is the default hook timeout (120 seconds).
const DefaultHookTimeoutMs = 120_000

// HookResult holds the outcome of a hook execution.
type HookResult struct {
	Exports map[string]string `json:"exports,omitempty"`
	Assets  map[string]string `json:"assets,omitempty"`
	// FreedBytes is the number of bytes a cache hook (cacheClean/cacheGC)
	// reported reclaiming via its JSONL "summary" event. Zero for hooks that
	// free nothing or do not report it.
	FreedBytes int64 `json:"freedBytes,omitempty"`
}

// hookContext is the JSON context passed to hook subprocesses.
type hookContext struct {
	WorkspaceRoot string `json:"workspaceRoot"`
	ProjectRoot   string `json:"projectRoot"`
	ExtensionRoot string `json:"extensionRoot"`
	OutputRoot    string `json:"outputRoot"`
	CacheRoot     string `json:"cacheRoot"`
	Debug         bool   `json:"debug"`
	Hook          string `json:"hook"`
	Extension     string `json:"extension"`
}

// RunPreBuildHook executes an extension's preBuild hook as a subprocess.
// Returns nil result if the extension has no preBuild hook defined. env holds
// KEY=value entries set after the manifest's Env, so each one wins over the
// inherited environment and the manifest.
func RunPreBuildHook(
	ctx context.Context,
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	proj *workspace.Project,
	debug bool,
	env []string,
) (*HookResult, error) {
	if ext.Hooks == nil || ext.Hooks.PreBuild == nil {
		return nil, nil
	}

	hook := ext.Hooks.PreBuild
	if hook.Kind != "command" && hook.Kind != "" {
		return nil, nil
	}

	projRoot := filepath.Join(ws.Root, proj.Path)
	outputRoot := filepath.Join(projRoot, ".gen")
	cacheRoot := hookCacheRoot(ws.Root, proj.Name, ext.Name)

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create hook cache dir: %w", err)
	}

	tmplVars := buildHookTemplateVars(ws.Root, projRoot, ext, outputRoot)
	tmplVars["cacheRoot"] = cacheRoot

	hctx := &hookContext{
		WorkspaceRoot: ws.Root,
		ProjectRoot:   projRoot,
		ExtensionRoot: ext.Path,
		OutputRoot:    outputRoot,
		CacheRoot:     cacheRoot,
		Debug:         debug,
		Hook:          "preBuild",
		Extension:     ext.Name,
	}

	contextFile, err := writeHookContext(hctx, cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("write hook context: %w", err)
	}
	defer os.Remove(contextFile)

	resolvedCommand := extension.ExpandTemplateVars(hook.Command, tmplVars)
	resolvedArgs := make([]string, len(hook.Args))
	for i, arg := range hook.Args {
		resolvedArgs[i] = extension.ExpandTemplateVars(arg, tmplVars)
	}
	resolvedArgs = append(resolvedArgs, "--putnami-context", contextFile)

	resolvedCwd := projRoot
	if hook.Cwd != "" {
		resolvedCwd = extension.ExpandTemplateVars(hook.Cwd, tmplVars)
	}

	timeoutMs := hook.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = DefaultHookTimeoutMs
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	hookEnv := os.Environ()
	hookEnv = append(hookEnv, "PUTNAMI_PROJECT_ROOT="+projRoot)
	hookEnv = append(hookEnv, "PUTNAMI_WORKSPACE_ROOT="+ws.Root)
	hookEnv = append(hookEnv, "PUTNAMI_EXTENSION_ROOT="+ext.Path)
	for k, v := range hook.Env {
		expanded := extension.ExpandTemplateVars(v, tmplVars)
		hookEnv = append(hookEnv, k+"="+expanded)
	}
	// exec.Cmd keeps the last value of a duplicated key.
	hookEnv = append(hookEnv, env...)

	cmd := exec.CommandContext(ctx, resolvedCommand, resolvedArgs...)
	cmd.Dir = resolvedCwd
	// A hosted run's hook downloads nothing and holds no framework credential.
	cmd.Env = runcredential.ChildEnv(hookEnv, false)
	// The hook's own context cache is under .putnami/projects, but its child
	// may also use workspace scratch inherited from a parent execution.
	childLease, err := store.AttachScratch(cmd, ws.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = childLease.Close() }()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("hook stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("hook stderr pipe: %w", err)
	}

	// A hosted run hands its credential to no process started after this one.
	runcredential.MarkRepositoryCodeStarted("hook " + ext.Name + "/preBuild")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start hook %s/%s: %w", ext.Name, "preBuild", err)
	}

	// Read JSONL events from stdout
	resultCh := make(chan *HookResult, 1)
	go func() {
		result := readHookEvents(stdout, debug)
		resultCh <- result
	}()

	stderrCh := make(chan string, 1)
	go func() {
		stderrCh <- iox.ReadCapped(stderr, iox.DefaultReadCap)
	}()

	hookResult := <-resultCh
	stderrText := <-stderrCh

	waitErr := cmd.Wait()
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("hook %s/%s timed out after %dms", ext.Name, "preBuild", timeoutMs)
		}

		errMsg := fmt.Sprintf("hook %s/%s failed", ext.Name, "preBuild")
		if stderrText != "" {
			lines := strings.Split(strings.TrimSpace(stderrText), "\n")
			if len(lines) > 50 {
				lines = lines[len(lines)-50:]
			}
			errMsg += ": " + strings.Join(lines, "\n")
		}
		return nil, fmt.Errorf("%s", errMsg)
	}

	if hookResult == nil {
		hookResult = &HookResult{}
	}
	return hookResult, nil
}

// RunOnInstallHook executes an extension's workspace-scoped install hook.
// The hook runs once when the extension is installed through the CLI.
func RunOnInstallHook(
	ctx context.Context,
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	debug bool,
) error {
	return RunOnInstallHookWithWriters(ctx, ws, ext, debug, os.Stdout, os.Stderr)
}

// RunOnInstallHookWithWriters executes an install hook with explicit output
// streams. Commands that reserve stdout for structured output can route hook
// chatter elsewhere while preserving the hook behavior.
func RunOnInstallHookWithWriters(
	ctx context.Context,
	ws *workspace.Workspace,
	ext *extension.ExtensionDescription,
	debug bool,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if ws == nil || ext == nil || ext.Hooks == nil || ext.Hooks.OnInstall == nil {
		return nil
	}

	hook := ext.Hooks.OnInstall
	if hook.Kind != "command" && hook.Kind != "" {
		return nil
	}

	outputRoot := filepath.Join(ws.Root, ".putnami", "out", "install-hooks", sanitizeSegment(ext.Name))
	cacheRoot := installHookCacheRoot(ws.Root, ext.Name)
	scratchLease, err := store.AcquireScratch(ws.Root)
	if err != nil {
		return err
	}
	defer func() { _ = scratchLease.Close() }()

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("create hook cache dir: %w", err)
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return fmt.Errorf("create hook output dir: %w", err)
	}

	tmplVars := buildHookTemplateVars(ws.Root, ws.Root, ext, outputRoot)
	tmplVars["cacheRoot"] = cacheRoot

	hctx := &hookContext{
		WorkspaceRoot: ws.Root,
		ProjectRoot:   ws.Root,
		ExtensionRoot: ext.Path,
		OutputRoot:    outputRoot,
		CacheRoot:     cacheRoot,
		Debug:         debug,
		Hook:          "onInstall",
		Extension:     ext.Name,
	}

	contextFile, err := writeHookContext(hctx, cacheRoot)
	if err != nil {
		return fmt.Errorf("write hook context: %w", err)
	}
	defer os.Remove(contextFile)

	resolvedCommand := extension.ExpandTemplateVars(hook.Command, tmplVars)
	resolvedArgs := make([]string, len(hook.Args))
	for i, arg := range hook.Args {
		resolvedArgs[i] = extension.ExpandTemplateVars(arg, tmplVars)
	}

	resolvedCwd := ws.Root
	if hook.Cwd != "" {
		resolvedCwd = extension.ExpandTemplateVars(hook.Cwd, tmplVars)
	}

	timeoutMs := hook.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = DefaultHookTimeoutMs
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	env := os.Environ()
	env = append(env, "PUTNAMI_WORKSPACE_ROOT="+ws.Root)
	env = append(env, "PUTNAMI_PROJECT_ROOT="+ws.Root)
	env = append(env, "PUTNAMI_EXTENSION_ROOT="+ext.Path)
	env = append(env, "PUTNAMI_HOOK=onInstall")
	env = append(env, "PUTNAMI_HOOK_CONTEXT="+contextFile)
	for k, v := range hook.Env {
		expanded := extension.ExpandTemplateVars(v, tmplVars)
		env = append(env, k+"="+expanded)
	}

	cmd := exec.CommandContext(ctx, resolvedCommand, resolvedArgs...)
	cmd.Dir = resolvedCwd
	// A hosted run's hook downloads nothing and holds no framework credential.
	cmd.Env = runcredential.ChildEnv(env, false)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	childLease, err := store.AttachScratch(cmd, ws.Root)
	if err != nil {
		return err
	}
	defer func() { _ = childLease.Close() }()

	if debug {
		iox.Fprintf(stderr, "[hook] %s %s\n", resolvedCommand, strings.Join(resolvedArgs, " "))
	}

	// A hosted run hands its credential to no process started after this one.
	runcredential.MarkRepositoryCodeStarted("hook " + ext.Name + "/onInstall")
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("hook %s/%s timed out after %dms", ext.Name, "onInstall", timeoutMs)
		}
		return fmt.Errorf("hook %s/%s failed: %w", ext.Name, "onInstall", err)
	}

	return nil
}

// readHookEvents reads JSONL events from the hook subprocess stdout.
// It looks for a "summary" event containing exports and assets.
func readHookEvents(r io.Reader, debug bool) *HookResult {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var result *HookResult

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event struct {
			V    int            `json:"v"`
			Type string         `json:"type"`
			Data map[string]any `json:"data,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.V != 1 || event.Type == "" {
			continue
		}

		if debug {
			iox.Fprintf(os.Stderr, "[hook] %s\n", line)
		}

		// Extract result from summary event
		if event.Type == "summary" && event.Data != nil {
			result = &HookResult{
				Exports:    extractStringMap(event.Data, "exports"),
				Assets:     extractStringMap(event.Data, "assets"),
				FreedBytes: extractInt64(event.Data, "freedBytes"),
			}
		}
	}

	return result
}

// extractInt64 reads a numeric field from a decoded JSONL event. JSON numbers
// decode to float64 through map[string]any, so both float64 and the occasional
// integer form are accepted; anything else yields 0.
func extractInt64(data map[string]any, key string) int64 {
	switch v := data[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

func extractStringMap(data map[string]any, key string) map[string]string {
	raw, ok := data[key].(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
	return result
}

func writeHookContext(hctx *hookContext, dir string) (string, error) {
	data, err := json.Marshal(hctx)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "hook-context-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func buildHookTemplateVars(
	workspaceRoot string,
	projectRoot string,
	ext *extension.ExtensionDescription,
	outputRoot string,
) map[string]string {
	vars := extension.BuildTemplateVars(workspaceRoot, projectRoot, ext.Path, outputRoot)
	vars["extensionRuntime"] = ext.RuntimeExecutable
	return vars
}

// hookCacheRoot returns the cache directory for a hook execution.
func hookCacheRoot(wsRoot, projectName, extensionName string) string {
	projSegment := sanitizeSegment(projectName)
	extSegment := sanitizeSegment(extensionName)
	return filepath.Join(wsRoot, ".putnami", "projects", projSegment, "cache", extSegment)
}

func installHookCacheRoot(wsRoot, extensionName string) string {
	return filepath.Join(wsRoot, ".putnami", "cache", "install-hooks", sanitizeSegment(extensionName))
}

func sanitizeSegment(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
