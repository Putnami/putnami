// Package hooks discovers and invokes lifecycle hooks from project dependencies.
//
// Extensions like @putnami/application and @putnami/web declare hooks
// in their putnami.extension.json manifests (e.g. `preBuild`, `configExtract`).
// These hooks generate code (loaders, SSR routes, client bundles, config
// schemas) that the build/serve/extract pipelines depend on.
package hooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// The states of the version 1 route evaluation a preBuild hook reports as
// data.httpRoutes in its summary: {"v":1,"state":"complete"} or
// {"v":1,"state":"unavailable","reason":"<text>"}, with no other member.
const (
	routeEvaluationComplete    = "complete"
	routeEvaluationUnavailable = "unavailable"
)

// Result holds the merged exports and assets from all invoked hooks.
type Result struct {
	Exports   map[string]string
	Assets    map[string]string
	HookCount int
	// Statuses collects the per-hook `status` field from each summary event,
	// in invocation order ("ok", "empty", "skipped", or "" when the hook did
	// not report one). Callers use it to distinguish "the hook regenerated its
	// output" from "the hook ran but produced nothing" — reading the output
	// file alone cannot tell a fresh write from a stale leftover. A status
	// never decides a route evaluation.
	Statuses []string
	// Unavailable states, one entry per cause and in a deterministic order,
	// why this run cannot tell which outputs the hooks of this kind produce.
	// Discovery entries come first, in dependency-name order: the project's
	// package.json, or an installed dependency's putnami.extension.json, could
	// not be read or parsed, so discovery may have missed a hook. For
	// HookPreBuild, an entry in invocation order follows for each hook whose
	// summary did not report a complete route evaluation (see
	// routeEvaluationCause). The hooks that were discovered still ran and their
	// outputs are merged. A caller that reads a missing output as "nothing
	// produces it" must first check that this is empty. A discovery entry names
	// no absolute path; a hook entry names the hook and quotes the reason it
	// reported.
	Unavailable []string
}

// manifestDocument is the part of putnami.extension.json hook discovery reads.
// Hooks is kept raw so that discovery can tell a JSON null from an absent
// member; it decodes into a map keyed by hook kind (see Hook* constants).
// Unknown keys are ignored rather than failing the parse so extensions can
// grow new hooks without breaking older runners.
type manifestDocument struct {
	Hooks json.RawMessage `json:"hooks"`
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

// hookSummaryData is the data field of a summary event. runHookCommand
// spreads the hook's custom data into the summary. Status is optional: hooks
// that follow the config-extract convention report "ok" / "empty" /
// "skipped" alongside their exports, and older hooks simply omit it.
// HTTPRoutes is the route evaluation a preBuild hook reports; it is kept raw,
// so a JSON null stays distinct from an absent member, and RunHooks checks it
// with routeEvaluationCause.
type hookSummaryData struct {
	Exports    map[string]string `json:"exports"`
	Assets     map[string]string `json:"assets"`
	Status     string            `json:"status"`
	HTTPRoutes json.RawMessage   `json:"httpRoutes,omitempty"`
}

// hookReport is what a hook's summary states about its own run beyond its
// outputs.
type hookReport struct {
	status string
	// httpRoutes is the raw data.httpRoutes member, nil when it is absent.
	httpRoutes json.RawMessage
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
// Returns an empty Result (never nil) when no dependency registers the hook,
// and an error, before any hook runs, when a dependency the project declares
// is not installed (see discoverHooks), and at the first hook that fails or
// whose summary cannot be parsed. Result.Unavailable lists the manifests
// discovery could not read and, for HookPreBuild, every hook that returned
// without a complete route evaluation; the later hooks still run, so the
// caller sees every such cause at once and fails before it publishes any
// output. hookConfig is forwarded to each hook subprocess as its `config`; pass nil
// when the hook kind carries no project configuration. projectName is the
// putnami project identity (see hookContext.ProjectName); pass "" when the
// caller has no project identity to forward.
func RunHooks(workspaceRoot, projectPath, projectName, hookKind, mode, bunBin string, debug bool, hookConfig map[string]any) (*Result, error) {
	hooks, unavailable, err := discoverHooks(workspaceRoot, projectPath, hookKind)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Exports:     make(map[string]string),
		Assets:      make(map[string]string),
		HookCount:   len(hooks),
		Unavailable: unavailable,
	}

	for _, h := range hooks {
		exports, assets, report, err := invokeHook(workspaceRoot, projectPath, projectName, mode, bunBin, h, debug, hookConfig)
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
		result.Statuses = append(result.Statuses, report.status)
		// The route evaluation is a preBuild contract; other hook kinds keep
		// their own result semantics.
		if hookKind == HookPreBuild {
			if cause := routeEvaluationCause(h.name, report.httpRoutes); cause != "" {
				result.Unavailable = append(result.Unavailable, cause)
			}
		}
	}

	return result, nil
}

// routeEvaluationCause checks the data.httpRoutes member a preBuild hook
// returned. It is "" for a complete evaluation, which certifies that the hook
// evaluated its own framework-declared route contribution, with or without a
// fragment. Otherwise it is the cause, naming the hook, that keeps the run from
// telling which routes the project serves: the hook reported its routes
// unavailable, or its member is absent or not the exact version 1 object. A
// missing member cannot certify an evaluation.
func routeEvaluationCause(hook string, raw json.RawMessage) string {
	if raw == nil {
		return "hook " + hook + " reported no route evaluation: its summary has no data.httpRoutes; " +
			routeEvaluationGuidance(hook)
	}
	state, reason, err := parseRouteEvaluation(raw)
	if err != nil {
		return "hook " + hook + " reported a malformed route evaluation: data.httpRoutes " + err.Error() + "; " +
			routeEvaluationGuidance(hook)
	}
	if state == routeEvaluationUnavailable {
		return "hook " + hook + " reported its routes unavailable: " + reason
	}
	return ""
}

// routeEvaluationGuidance tells the owner of a hook how to make it report a
// route evaluation.
func routeEvaluationGuidance(hook string) string {
	return "upgrade " + hook + " together with @putnami/typescript to releases that report data.httpRoutes, " +
		`or adapt a custom hook to return {"v":1,"state":"complete"} as data.httpRoutes once it has evaluated its routes`
}

// parseRouteEvaluation reads a version 1 route evaluation: an object with v 1
// and state complete, or state unavailable and a reason string that is not
// blank. It refuses any other member, a reason on a complete evaluation, and
// a JSON null anywhere a value is required. Members are checked in byte order,
// so the error is the same for the same bytes.
func parseRouteEvaluation(raw json.RawMessage) (state, reason string, err error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return "", "", errors.New("is not an object")
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != "v" && name != "state" && name != "reason" {
			return "", "", fmt.Errorf("has the member %q, which version 1 does not define", name)
		}
	}

	var version int
	rawVersion, ok := members["v"]
	if !ok {
		return "", "", errors.New("has no v")
	}
	if json.Unmarshal(rawVersion, &version) != nil || version != 1 {
		return "", "", fmt.Errorf("has v %s, and only version 1 is supported", rawVersion)
	}

	rawState, ok := members["state"]
	if !ok {
		return "", "", errors.New("has no state")
	}
	if json.Unmarshal(rawState, &state) != nil || (state != routeEvaluationComplete && state != routeEvaluationUnavailable) {
		return "", "", fmt.Errorf(`has state %s, not "complete" or "unavailable"`, rawState)
	}

	rawReason, hasReason := members["reason"]
	if state == routeEvaluationComplete {
		if hasReason {
			return "", "", errors.New("is complete and has a reason, which only an unavailable evaluation carries")
		}
		return state, "", nil
	}
	if !hasReason || json.Unmarshal(rawReason, &reason) != nil || strings.TrimSpace(reason) == "" {
		return "", "", errors.New("is unavailable without a reason string that is not blank")
	}
	return state, reason, nil
}

// Discovery is what hook discovery finds for one hook kind, without running a
// hook.
type Discovery struct {
	// Hooks names the dependencies that declare a hook of the kind, in the
	// order RunHooks invokes them.
	Hooks []string
	// Unavailable lists the manifests discovery could not use, as
	// Result.Unavailable does.
	Unavailable []string
}

// Discover runs hook discovery alone, so a caller that cannot run hooks can
// tell a project that declares none from one whose hooks it would skip. It
// fails exactly where RunHooks fails before it runs a hook.
func Discover(workspaceRoot, projectPath, hookKind string) (Discovery, error) {
	found, unavailable, err := discoverHooks(workspaceRoot, projectPath, hookKind)
	if err != nil {
		return Discovery{}, err
	}
	names := make([]string, 0, len(found))
	for _, h := range found {
		names = append(names, h.name)
	}
	return Discovery{Hooks: names, Unavailable: unavailable}, nil
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
//
// # An uninstalled dependency fails discovery
//
// A dependency the project declares that no node_modules holds is an error,
// never "a dependency without hooks". No cache key holds the hook set: a key
// reads package.json and bun.lock, which name the dependencies, not whether
// they are installed. A generate that ran without the hooks of an uninstalled
// dependency therefore succeeded, in milliseconds, with a .gen that lacked
// what those hooks write (clientgen/config.json, schema/, the loaders), under
// the key of a complete run, and the shared cache served that tree to every
// checkout. A dependency is installed where Node resolution finds it:
// node_modules of the project, then of each parent directory up to the
// workspace root (installedDependencyRoot).
//
// # An unreadable manifest makes discovery incomplete
//
// The second return value lists, in dependency-name order, each manifest discovery
// needed and could not use: the project's package.json when it is missing,
// unreadable or malformed (no hook is then discovered), and an installed
// dependency's putnami.extension.json when it exists but cannot be read or
// parsed (that dependency contributes no hook). A JSON null where discovery
// needs an object is malformed (see requirePackageObjects and
// parseManifestHook): encoding/json decodes it into an empty value without an
// error, which would read as a project without dependencies or a dependency
// without hooks. A dependency without a putnami.extension.json declares no
// hook, which is not a gap. The hooks discovery did find are still returned
// and run.
func discoverHooks(workspaceRoot, projectPath, hookKind string) ([]discoveredHook, []string, error) {
	pkg, err := project.ReadPackageJSON(filepath.Join(projectPath, "package.json"))
	if err == nil {
		err = requirePackageObjects(pkg)
	}
	if err != nil {
		return nil, []string{describeManifestGap("package.json", err)}, nil
	}

	var hooks []discoveredHook
	var unavailable []string

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

	var missing []string
	for _, depName := range depNames {
		extRoot, installed := installedDependencyRoot(workspaceRoot, projectPath, depName,
			strings.HasPrefix(allDeps[depName], "workspace:"))
		if !installed {
			missing = append(missing, depName)
			continue
		}

		manifestPath := filepath.Join(extRoot, "putnami.extension.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				unavailable = append(unavailable, describeManifestGap(depName+": putnami.extension.json", err))
			}
			continue
		}

		hook, err := parseManifestHook(data, hookKind)
		if err != nil {
			unavailable = append(unavailable, describeManifestGap(depName+": putnami.extension.json", err))
			continue
		}

		if hook != nil {
			hooks = append(hooks, discoveredHook{
				name:          depName,
				kind:          hookKind,
				extensionRoot: extRoot,
				hook:          hook,
			})
		}
	}

	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("%s declares %s, which no node_modules from the project up to the workspace root holds: "+
			"the dependencies are not installed, so the hooks they contribute cannot run; run `putnami install`",
			projectPath, strings.Join(missing, ", "))
	}

	// depNames is already byte-ordered, so a stable sort on the rank alone
	// yields the full (order, name) sequence.
	sort.SliceStable(hooks, func(i, j int) bool {
		return hooks[i].order() < hooks[j].order()
	})

	return hooks, unavailable, nil
}

// requirePackageObjects refuses a package.json that is JSON null, or whose
// dependencies or devDependencies member is JSON null. Each would decode into
// an empty value without an error and read as a project without
// dependencies, which is a known absence of hooks.
func requirePackageObjects(pkg *project.PackageJSON) error {
	if pkg.Raw == nil {
		return errors.New("the document is null, not an object")
	}
	for _, member := range []string{"dependencies", "devDependencies"} {
		if raw, ok := pkg.Raw[member]; ok && isJSONNull(raw) {
			return fmt.Errorf("%s is null, not an object", member)
		}
	}
	return nil
}

// parseManifestHook returns the hook of the given kind that a
// putnami.extension.json declares, or nil when it declares none. Every hook
// kind must decode, as before; a JSON null for the document, for hooks or for
// the requested kind is malformed, because it would otherwise read as a
// dependency without that hook.
func parseManifestHook(data []byte, hookKind string) (*hookDefinition, error) {
	var document *manifestDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("the document is null, not an object")
	}
	if document.Hooks == nil {
		return nil, nil
	}
	var hooks map[string]*hookDefinition
	if err := json.Unmarshal(document.Hooks, &hooks); err != nil {
		return nil, err
	}
	if hooks == nil {
		return nil, errors.New("hooks is null, not an object")
	}
	hook, declared := hooks[hookKind]
	if declared && hook == nil {
		return nil, fmt.Errorf("hooks.%s is null, not an object", hookKind)
	}
	return hook, nil
}

// isJSONNull reports whether raw is the JSON null literal.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// describeManifestGap renders why discovery could not use the manifest named
// by label. It names no absolute path: a read error contributes only its
// cause, so the sentence is the same in every checkout.
func describeManifestGap(label string, err error) string {
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return label + " is missing"
	case errors.As(err, &pathErr):
		return label + " cannot be read: " + pathErr.Err.Error()
	default:
		if inner := errors.Unwrap(err); inner != nil {
			err = inner
		}
		return label + " is malformed: " + err.Error()
	}
}

// installedDependencyRoot returns the directory a declared dependency is
// installed at, searched the way Node resolves a bare specifier: node_modules
// of the project, then of each parent directory up to and including the
// workspace root. A workspace: dependency is the target of its link, which
// must exist; any other dependency is the node_modules path itself, which must
// resolve to an existing entry (a dangling link does not). installed is false
// when no candidate qualifies.
func installedDependencyRoot(workspaceRoot, projectPath, depName string, workspaceLink bool) (string, bool) {
	for _, dir := range resolutionDirs(workspaceRoot, projectPath) {
		candidate := filepath.Join(dir, "node_modules", filepath.FromSlash(depName))
		if workspaceLink {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
				return resolved, true
			}
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// resolutionDirs lists the directories whose node_modules may hold a
// dependency of the project, nearest first: the project, then each parent up to
// and including the workspace root. Without a workspace root, or for a project
// outside it, it is the project alone, so resolution never leaves the
// workspace.
func resolutionDirs(workspaceRoot, projectPath string) []string {
	projectDir := filepath.Clean(projectPath)
	dirs := []string{projectDir}
	if workspaceRoot == "" {
		return dirs
	}
	root := filepath.Clean(workspaceRoot)
	if rel, err := filepath.Rel(root, projectDir); err != nil || rel == "." || !filepath.IsLocal(rel) {
		return dirs
	}
	for dir := filepath.Dir(projectDir); ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == root || filepath.Dir(dir) == dir {
			return dirs
		}
	}
}

// invokeHook runs a single hook subprocess and parses its JSONL output. The
// hook kind in `h.kind` is forwarded through the context so the bun-side
// runner can dispatch on it. A hook declaring `preBuild` receives that exact
// kind. The third return value is what the summary reports about the run: its
// status and its raw route evaluation.
func invokeHook(workspaceRoot, projectPath, projectName, mode, bunBin string, h discoveredHook, debug bool, hookConfig map[string]any) (map[string]string, map[string]string, hookReport, error) {
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
		return nil, nil, hookReport{}, fmt.Errorf("marshaling context: %w", err)
	}

	contextFile, err := os.CreateTemp("", "putnami-hook-*.json")
	if err != nil {
		return nil, nil, hookReport{}, fmt.Errorf("creating context file: %w", err)
	}
	contextPath := contextFile.Name()
	defer os.Remove(contextPath)

	if _, err := contextFile.Write(contextData); err != nil {
		contextFile.Close()
		return nil, nil, hookReport{}, fmt.Errorf("writing context file: %w", err)
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
		return nil, nil, hookReport{}, fmt.Errorf("executing hook: %w", err)
	}

	if !result.Success {
		return nil, nil, hookReport{}, fmt.Errorf("hook exited with code %d: %s", result.ExitCode, formatFailure(result.Stdout, result.Stderr))
	}

	// Parse JSONL output for summary event
	exports, assets, report, err := parseSummary(result.Stdout)
	if err != nil {
		return nil, nil, hookReport{}, fmt.Errorf("parsing hook output: %w", err)
	}
	return exports, assets, report, nil
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
// status and route evaluation from the JSONL summary event.
//
// A successful hook always emits a summary (runHookCommand guarantees it), so
// a missing or unparsable summary is a protocol violation, not an empty
// result: returning empty maps here once silently dropped a hook's exports
// when its summary line was truncated at the 64KB pipe buffer, shipping a
// server binary with no static routes. Fail loudly instead.
func parseSummary(output string) (map[string]string, map[string]string, hookReport, error) {
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
				return nil, nil, hookReport{}, fmt.Errorf("summary event is not valid JSON (hook stdout truncated?): %s", truncateTail(line, 200))
			}
			continue
		}

		if event.Type != "summary" {
			continue
		}

		var data hookSummaryData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return nil, nil, hookReport{}, fmt.Errorf("summary event has malformed data: %w", err)
		}

		exports := data.Exports
		if exports == nil {
			exports = map[string]string{}
		}
		assets := data.Assets
		if assets == nil {
			assets = map[string]string{}
		}
		return exports, assets, hookReport{status: data.Status, httpRoutes: data.HTTPRoutes}, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, hookReport{}, fmt.Errorf("scanning hook output: %w", err)
	}

	return nil, nil, hookReport{}, fmt.Errorf("hook exited successfully but emitted no summary event")
}

// expandTemplates replaces {extensionRoot}, {projectRoot}, {workspaceRoot} in a string.
func expandTemplates(s, workspaceRoot, projectRoot, extensionRoot string) string {
	s = strings.ReplaceAll(s, "{extensionRoot}", extensionRoot)
	s = strings.ReplaceAll(s, "{projectRoot}", projectRoot)
	s = strings.ReplaceAll(s, "{workspaceRoot}", workspaceRoot)
	return s
}
