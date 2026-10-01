// Package hooks discovers and invokes lifecycle hooks from project dependencies.
//
// Extensions like @putnami/application and @putnami/web declare hooks
// in their putnami.extension.json manifests (e.g. `preBuild`, `configExtract`).
// These hooks generate code (loaders, SSR routes, client bundles, config
// schemas) that the build/serve/extract pipelines depend on.
package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/project"
)

// execRunFunc is the function used to run subprocesses. Defaults to exec.Run.
var execRunFunc = exec.Run

// Hook kind identifiers, matching the JSON keys under `hooks` in an
// extension's putnami.extension.json. Callers select which kind to discover
// by passing one of these constants to the runner functions below.
const (
	HookPreBuild      = "preBuild"
	HookConfigExtract = "configExtract"
)

const maxHookEventBytes = 16 * 1024 * 1024

// Result holds the merged exports and assets from all invoked hooks.
type Result struct {
	Exports   map[string]string
	Assets    map[string]string
	HookCount int
	// Statuses collects the per-hook `status` field from each summary event,
	// in invocation order ("ok", "empty", "skipped", or "" when the hook did
	// not report one). Callers use it to distinguish "the hook regenerated
	// its output" from "the hook ran but produced nothing" — reading the
	// output file alone cannot tell a fresh write from a stale leftover.
	Statuses []string
}

// extensionManifest is a minimal parse of putnami.extension.json. The hooks
// map is keyed by hook kind (see Hook* constants); unknown keys are ignored
// rather than failing the parse so extensions can grow new hooks without
// breaking older runners.
type extensionManifest struct {
	Hooks map[string]*hookDefinition `json:"hooks"`
}

// hookDefinition describes a hook command from the manifest. Fields the runner
// does not use are intentionally absent: extension manifests carry keys for
// other consumers (cache, env, dependsOn, …) and unknown keys must keep
// parsing so a manifest written for a newer runner still works here.
type hookDefinition struct {
	Kind      string   `json:"kind"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	TimeoutMs int      `json:"timeoutMs"`
	// Order is the invocation rank of this hook among all extensions that
	// declare the same kind for one project — see the ordering contract on
	// discoverHooks. Absent (0) means "producer", which is what every manifest
	// written before the field existed means, so older manifests keep working.
	Order int `json:"order"`
}

// hookContext is the JSON structure written to the context file for hook subprocesses.
type hookContext struct {
	WorkspaceRoot string `json:"workspaceRoot"`
	ProjectRoot   string `json:"projectRoot"`
	ExtensionRoot string `json:"extensionRoot"`
	OutputRoot    string `json:"outputRoot"`
	CacheRoot     string `json:"cacheRoot"`
	Debug         bool   `json:"debug"`
	Hook          string `json:"hook"`
	Extension     string `json:"extension"`
	// ProjectName is the putnami project identity (putnami.json name / project
	// path), the same source of truth the Go config extractor uses as appName.
	// It is distinct from the npm package.json name a bun hook would otherwise
	// derive locally. It is omitted when the orchestrator has no project
	// identity to forward.
	ProjectName string         `json:"projectName,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	Mode        string         `json:"mode,omitempty"`
}

// hookEvent is a JSONL event emitted by hook subprocesses.
type hookEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// hookSummaryData is the data field of a summary event. Status is optional:
// hooks that follow the config-extract convention report "ok" / "empty" /
// "skipped" alongside their exports (runHookCommand spreads the hook's
// custom data into the summary), older hooks simply omit it.
type hookSummaryData struct {
	Exports map[string]string `json:"exports"`
	Assets  map[string]string `json:"assets"`
	Status  string            `json:"status"`
}

// hookErrorData is the data field of an error event.
type hookErrorData struct {
	Code  string `json:"code"`
	Stack string `json:"stack"`
}

// hookEventWithMessage parses the common envelope fields of any event.
type hookEventWithMessage struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// discoveredHook represents a dependency that registered a hook of a given kind.
// The kind is propagated to the bun-side context so the hook script can
// distinguish which lifecycle phase invoked it.
type discoveredHook struct {
	name          string
	kind          string
	extensionRoot string
	hook          *hookDefinition
}

// order is the hook's declared invocation rank (0 when the manifest omits it).
func (h discoveredHook) order() int {
	if h.hook == nil {
		return 0
	}
	return h.hook.Order
}

// RunHooks discovers and invokes all hooks of the given kind for a project, in
// the deterministic order discoverHooks defines (see its ordering contract —
// hooks share one `.gen` tree, so the sequence is part of the build contract).
// Returns an empty Result (never nil) when no dependency registers the hook.
// hookConfig is forwarded to each hook subprocess as its `config`; pass nil
// when the hook kind carries no project configuration. projectName is the
// putnami project identity (see hookContext.ProjectName); pass "" when the
// caller has no project identity to forward.
func RunHooks(workspaceRoot, projectPath, projectName, hookKind, mode, bunBin string, debug bool, hookConfig map[string]any) (*Result, error) {
	hooks := discoverHooks(workspaceRoot, projectPath, hookKind)
	if len(hooks) == 0 {
		return &Result{Exports: map[string]string{}, Assets: map[string]string{}, HookCount: 0}, nil
	}

	result := &Result{
		Exports:   make(map[string]string),
		Assets:    make(map[string]string),
		HookCount: len(hooks),
	}

	for _, h := range hooks {
		exports, assets, status, err := invokeHook(workspaceRoot, projectPath, projectName, mode, bunBin, h, debug, hookConfig)
		if err != nil {
			return nil, fmt.Errorf("hook %s: %w", h.name, err)
		}
		for k, v := range exports {
			if existing, ok := result.Exports[k]; ok && filepath.Clean(existing) != filepath.Clean(v) {
				return nil, fmt.Errorf("hook %s: export %q conflicts with another hook (%s != %s)", h.name, k, existing, v)
			}
			result.Exports[k] = v
		}
		for k, v := range assets {
			result.Assets[k] = v
		}
		result.Statuses = append(result.Statuses, status)
	}

	return result, nil
}

// discoverHooks finds project dependencies whose putnami.extension.json
// declares a hook of the given kind. Unknown manifest keys are ignored.
//
// # Ordering contract
//
// RunHooks invokes the returned hooks in slice order, and hooks are NOT
// independent: they share one `.gen` tree, so a hook can only see what the
// hooks before it wrote. The returned order is therefore part of the build
// contract and must be a pure function of the manifests.
//
// Hooks are sorted by (declared `order`, extension name in byte order):
//
//   - `order` 0 (the default, and what every manifest written before the field
//     existed means) is the producer rank: the hook only writes generated
//     sources.
//   - a higher `order` is the consumer/finalizer rank: the hook must observe
//     what the producers generated. `@putnami/application` declares one because
//     its hook imports the workload entry point — an entry point is free to
//     import a module another extension generates into `.gen`, and the whole
//     capability manifest it emits describes the finished generated tree.
//
// Two alternatives are wrong:
//
//   - Iterating the dependency map. Go randomizes map order per process, so
//     that order is not deterministic across runs.
//   - Sorting by name alone. That is deterministic but does not account for
//     producer/consumer rank.
//   - Topologically sorting `extensionDependencies`. That edge is a LIBRARY
//     relation and points the opposite way here: `@putnami/web` depends on
//     `@putnami/application`'s API, yet its hook must run first because it
//     produces the loaders `@putnami/application`'s hook consumes.
//
// The rank is declared by the consumer, not by naming the producers, so a
// third-party extension that generates sources gets the correct order without
// anyone editing this file or its own manifest.
func discoverHooks(_, projectPath, hookKind string) []discoveredHook {
	pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json"))
	if pkg == nil {
		return nil
	}

	var hooks []discoveredHook

	// Check all dependency types
	allDeps := make(map[string]string)
	for k, v := range pkg.Dependencies {
		allDeps[k] = v
	}
	for k, v := range pkg.DevDependencies {
		allDeps[k] = v
	}

	depNames := make([]string, 0, len(allDeps))
	for name := range allDeps {
		depNames = append(depNames, name)
	}
	sort.Strings(depNames)

	for _, depName := range depNames {
		depVersion := allDeps[depName]
		var extRoot string

		if strings.HasPrefix(depVersion, "workspace:") {
			// Workspace dependency — resolve via node_modules symlink
			nmPath := filepath.Join(projectPath, "node_modules", depName)
			resolved, err := filepath.EvalSymlinks(nmPath)
			if err != nil {
				continue
			}
			extRoot = resolved
		} else {
			// npm dependency — check in node_modules
			extRoot = filepath.Join(projectPath, "node_modules", depName)
		}

		manifestPath := filepath.Join(extRoot, "putnami.extension.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}

		var manifest extensionManifest
		if json.Unmarshal(data, &manifest) != nil {
			continue
		}

		if hook := manifest.Hooks[hookKind]; hook != nil {
			hooks = append(hooks, discoveredHook{
				name:          depName,
				kind:          hookKind,
				extensionRoot: extRoot,
				hook:          hook,
			})
		}
	}

	// depNames is already byte-ordered, so a stable sort on the rank alone
	// yields the full (order, name) sequence.
	sort.SliceStable(hooks, func(i, j int) bool {
		return hooks[i].order() < hooks[j].order()
	})

	return hooks
}

// invokeHook runs a single hook subprocess and parses its JSONL output. The
// hook kind in `h.kind` is forwarded through the context so the bun-side
// runner can dispatch on it. A hook declaring `preBuild` receives that exact
// kind. The third
// return value is the hook-reported summary status ("" when absent).
func invokeHook(workspaceRoot, projectPath, projectName, mode, bunBin string, h discoveredHook, debug bool, hookConfig map[string]any) (map[string]string, map[string]string, string, error) {
	// Write context file
	ctx := hookContext{
		WorkspaceRoot: workspaceRoot,
		ProjectRoot:   projectPath,
		ExtensionRoot: h.extensionRoot,
		OutputRoot:    filepath.Join(projectPath, ".gen"),
		CacheRoot:     filepath.Join(workspaceRoot, ".putnami", "hooks-cache", h.name),
		Debug:         debug,
		Hook:          h.kind,
		Extension:     h.name,
		ProjectName:   projectName,
		Config:        hookConfig,
		Mode:          mode,
	}

	contextData, err := json.Marshal(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshaling context: %w", err)
	}

	contextFile, err := os.CreateTemp("", "putnami-hook-*.json")
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating context file: %w", err)
	}
	contextPath := contextFile.Name()
	defer os.Remove(contextPath)

	if _, err := contextFile.Write(contextData); err != nil {
		contextFile.Close()
		return nil, nil, "", fmt.Errorf("writing context file: %w", err)
	}
	contextFile.Close()

	// Build command args with template expansion
	args := make([]string, 0, len(h.hook.Args)+2)
	for _, arg := range h.hook.Args {
		args = append(args, expandTemplates(arg, workspaceRoot, projectPath, h.extensionRoot))
	}
	args = append(args, "--putnami-context", contextPath)

	// Resolve command
	command := h.hook.Command
	if command == "bun" {
		command = bunBin
	}

	// Determine timeout
	timeout := 120 * time.Second
	if h.hook.TimeoutMs > 0 {
		timeout = time.Duration(h.hook.TimeoutMs) * time.Millisecond
	}

	// Run subprocess
	result, err := execRunFunc(command, args,
		exec.Dir(projectPath),
		exec.Env(map[string]string{
			"PUTNAMI_PROJECT_ROOT":     projectPath,
			"PUTNAMI_WORKSPACE_ROOT":   workspaceRoot,
			"PUTNAMI_PREBUILD_CONTEXT": "true",
		}),
		exec.Timeout(timeout),
	)
	if err != nil {
		return nil, nil, "", fmt.Errorf("executing hook: %w", err)
	}

	if !result.Success {
		return nil, nil, "", fmt.Errorf("hook exited with code %d: %s", result.ExitCode, formatFailure(result.Stdout, result.Stderr))
	}

	// Parse JSONL output for summary event
	exports, assets, status, err := parseSummary(result.Stdout)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parsing hook output: %w", err)
	}
	return exports, assets, status, nil
}

// formatFailure builds the diagnostic message attached to a hook failure.
// Hooks emit errors as JSONL "error" events on stdout (see emitError in
// @putnami/utils), so a code-1 failure usually carries no stderr. Surface any
// stdout error events first, then fall back to stderr.
func formatFailure(stdout, stderr string) string {
	errs := extractErrorEvents(stdout)
	stderr = strings.TrimSpace(stderr)

	switch {
	case len(errs) > 0 && stderr != "":
		return strings.Join(errs, "; ") + " | stderr: " + truncateTail(stderr, 2000)
	case len(errs) > 0:
		return strings.Join(errs, "; ")
	case stderr != "":
		return truncateTail(stderr, 2000)
	default:
		return "no diagnostic output (hook crashed without emitting an error event — try re-running with debug logging)"
	}
}

// extractErrorEvents scans JSONL stdout for "error" events and returns their
// messages, optionally augmented with the first stack frame from data.stack.
func extractErrorEvents(output string) []string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), maxHookEventBytes)
	var msgs []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev hookEventWithMessage
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type != "error" {
			continue
		}
		msg := ev.Message
		if len(ev.Data) > 0 {
			var data hookErrorData
			if json.Unmarshal(ev.Data, &data) == nil && data.Stack != "" {
				if frame := firstStackFrame(data.Stack); frame != "" {
					msg += " (at " + frame + ")"
				}
			}
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// firstStackFrame returns the first "at ..." frame line of a JS stack trace.
// The first line of a JS stack is typically "Error: <message>" which duplicates
// the error message, so we skip it. Returns "" if no frame is found.
func firstStackFrame(stack string) string {
	for line := range strings.SplitSeq(stack, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "at ") {
			return line
		}
	}
	return ""
}

// truncateTail returns the last max bytes of s.
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// parseSummary extracts exports, assets, and the optional hook-reported
// status from the JSONL summary event.
//
// A successful hook always emits a summary (runHookCommand guarantees it), so
// a missing or unparsable summary is a protocol violation, not an empty
// result: returning empty maps here once silently dropped a hook's exports
// when its summary line was truncated at the 64KB pipe buffer, shipping a
// server binary with no static routes. Fail loudly instead.
func parseSummary(output string) (map[string]string, map[string]string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), maxHookEventBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event hookEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			// Hooks may interleave non-JSONL noise on stdout; skip it. But a
			// line that carries the summary marker and still fails to parse
			// means the summary itself is corrupt (e.g. truncated output).
			if strings.Contains(line, `"type":"summary"`) {
				return nil, nil, "", fmt.Errorf("summary event is not valid JSON (hook stdout truncated?): %s", truncateTail(line, 200))
			}
			continue
		}

		if event.Type != "summary" {
			continue
		}

		var data hookSummaryData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return nil, nil, "", fmt.Errorf("summary event has malformed data: %w", err)
		}

		exports := data.Exports
		if exports == nil {
			exports = map[string]string{}
		}
		assets := data.Assets
		if assets == nil {
			assets = map[string]string{}
		}
		return exports, assets, data.Status, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, "", fmt.Errorf("scanning hook output: %w", err)
	}

	return nil, nil, "", fmt.Errorf("hook exited successfully but emitted no summary event")
}

// expandTemplates replaces {extensionRoot}, {projectRoot}, {workspaceRoot} in a string.
func expandTemplates(s, workspaceRoot, projectRoot, extensionRoot string) string {
	s = strings.ReplaceAll(s, "{extensionRoot}", extensionRoot)
	s = strings.ReplaceAll(s, "{projectRoot}", projectRoot)
	s = strings.ReplaceAll(s, "{workspaceRoot}", workspaceRoot)
	return s
}
