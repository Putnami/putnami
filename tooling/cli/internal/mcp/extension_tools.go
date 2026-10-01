package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const defaultExtensionToolTimeout = 30 * time.Second

type extensionToolCandidate struct {
	name string
	// ext is the whole resolved description, not only its root, because a tool
	// may name the extension's PREPARED RUNTIME ({extensionRuntime}) as its
	// command — and resolving that needs the extension's runtime declaration,
	// which lives on the description.
	ext *extension.ExtensionDescription
	def extension.ToolDefinition
}

func (c extensionToolCandidate) extensionRoot() string {
	if c.ext == nil {
		return ""
	}
	return c.ext.Path
}

// discoverExtensions loads the workspace's extension descriptions, with their
// commands' traits already resolved by extension.Resolve. Every reader of
// manifest metadata on this surface goes through it — tool registration and
// run_jobs' side-effect gate — so the two can never disagree about what an
// extension declares.
func (s *Server) discoverExtensions() (*extension.DiscoveryResult, error) {
	if s.opts.WorkspaceRoot == "" {
		return nil, errors.New("no workspace root")
	}
	ws, err := workspace.Load(s.opts.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	cfg := s.opts.Config
	if cfg == nil {
		cfg = wsproto.Load(s.opts.WorkspaceRoot)
	}
	discovered, err := extension.DiscoverExtensionsDetailed(s.opts.WorkspaceRoot, cfg, projectPaths)
	if err != nil {
		return nil, err
	}
	discovered.Extensions = extension.SelectPreparedExtensions(discovered.Extensions, s.opts.PreparedExtensions)
	return discovered, nil
}

// taskIndex resolves the task-level impact index for one loaded workspace, or
// nil when the extensions cannot be discovered — in which case `impacted` and
// `why_impacted` answer at project resolution rather than answering nothing.
//
// Both tools read it, so the pair cannot disagree about a project's task scope
// any more than they can disagree about its reach.
func (s *Server) taskIndex(ws *workspace.Workspace) workspace.TaskImpactIndex {
	discovered, err := s.discoverExtensions()
	if err != nil || discovered == nil {
		return nil
	}
	return workspace.NewTaskIndex(ws, discovered.Extensions)
}

// registerExtensionTools discovers manifest-declared tools once, when the MCP
// server starts. It only reads manifests: extension processes stay lazy and are
// spawned on their first tools/call. Discovery and every candidate failure are
// isolated so a bad extension can never prevent the core server from starting.
func (s *Server) registerExtensionTools() {
	discovered, err := s.discoverExtensions()
	if err != nil {
		return
	}
	// An extension that did not load exposes no tools, and an agent has no way
	// to ask why a tool is absent. Since the require-v3 flip, the common cause is
	// a below-contract extension whose skip Reason already names the remedy, so
	// it is logged once at server start — on stderr, which is outside the
	// JSON-RPC frame stream stdout carries.
	for _, skip := range discovered.Skipped {
		slog.Warn("extension skipped; its tools are not registered",
			"extension", skip.Name, "path", skip.Path, "error", skip.Reason)
	}
	s.registerExtensionToolDefinitions(discovered.Extensions)
	s.registerCollaborationTools(discovered.Extensions)
}

// disabled returns the workspace's disabled job and extension names. Only tool
// registration reads it now: run_jobs is routed through engine.Run, which reads
// the same config itself rather than being handed it.
func (s *Server) disabled() (jobsList, extsList []string) {
	if s.opts.Config != nil && s.opts.Config.Disable != nil {
		return s.opts.Config.Disable.Jobs, s.opts.Config.Disable.Extensions
	}
	return nil, nil
}

// unavailableExtensions names the discovered extensions no tool may come
// from: the ones the workspace disables and the ones that were not prepared
// from the exact lock.
func (s *Server) unavailableExtensions() map[string]bool {
	_, disabledExtensions := s.disabled()
	unavailable := make(map[string]bool, len(disabledExtensions)+len(s.opts.UnavailableExtensions))
	for _, name := range disabledExtensions {
		unavailable[name] = true
	}
	for _, name := range s.opts.UnavailableExtensions {
		unavailable[name] = true
	}
	return unavailable
}

// registerExtensionToolDefinitions aggregates a stable, collision-free view
// of extension tools. Core tool names always win. A name declared by multiple
// extensions is rejected from the aggregate instead of depending on discovery
// order, which makes agent-facing tool selection deterministic.
//
// A tool that implements a collaboration operation is never registered under
// its own name: it is reachable only through the workspace binding, under the
// contract's operation name (registerCollaborationTools). When the workspace
// binds collaboration contracts, the contract namespaces are the binding's,
// and an extension tool declared inside one is refused rather than allowed to
// shadow or be shadowed by the routed operation.
func (s *Server) registerExtensionToolDefinitions(exts []*extension.ExtensionDescription) {
	disabled := s.unavailableExtensions()
	reserveContracts := CollaborationDeclared(s.opts.WorkspaceRoot)

	sortedExts := append([]*extension.ExtensionDescription(nil), exts...)
	sort.SliceStable(sortedExts, func(i, j int) bool { return sortedExts[i].Name < sortedExts[j].Name })
	candidates := make(map[string][]extensionToolCandidate)
	for _, ext := range sortedExts {
		if ext == nil {
			continue
		}
		if disabled[ext.Name] {
			continue
		}
		for name, def := range ext.Tools {
			if _, provider := def.Meta[collab.ProviderMetaKey]; provider {
				continue
			}
			if reserveContracts && inContractNamespace(name) {
				slog.Warn("extension tool uses a collaboration contract namespace the workspace binds; it is not registered",
					"extension", ext.Name, "tool", name)
				continue
			}
			if !validExtensionTool(name, def) || !extensionToolExecutablePresent(s.opts.WorkspaceRoot, ext, def) {
				continue
			}
			candidates[name] = append(candidates[name], extensionToolCandidate{
				name: name,
				ext:  ext,
				def:  def,
			})
		}
	}

	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, coreOwnsName := s.byName[name]; coreOwnsName || len(candidates[name]) != 1 {
			continue
		}
		candidate := candidates[name][0]
		def := mcpToolFromExtension(name, candidate.def)
		s.registerResult(def, func(ctx context.Context, args json.RawMessage) (callToolResult, error) {
			return s.callExtensionTool(ctx, candidate, args)
		})
	}
}

// extensionToolExecutablePresent catches a reaped installed extension binary
// before its descriptor reaches tools/list. It is intentionally a stat/lookup
// only: no extension code runs during MCP startup or discovery.
//
// A tool that names {extensionRuntime} is answered from the DECLARATION rather
// than from the filesystem, and that is the same rule stated differently: the
// prepared runtime is built on first use (see extensionToolCommand), so there is
// nothing to stat yet and probing would either hide the tool from every fresh
// checkout or force a compile during startup. The declaration is what makes the
// tool answerable; a runtime that then fails to prepare is reported as the tool
// call's error, which is where the agent can read the reason.
func extensionToolExecutablePresent(
	workspaceRoot string, ext *extension.ExtensionDescription, def extension.ToolDefinition,
) bool {
	if ext == nil {
		return false
	}
	extensionRoot := ext.Path
	if referencesExtensionRuntime(def.Command) {
		return ext.Runtime != nil
	}
	vars := extension.BuildTemplateVars(workspaceRoot, workspaceRoot, extensionRoot, "")
	command := extension.ExpandTemplateVars(def.Command, vars)
	if filepath.IsAbs(command) {
		return extensionToolFilePresent(command)
	}
	// exec.Command searches PATH only for a bare name, one equal to its own
	// filepath.Base; anything else runs from cmd.Dir. Ask the same question, so
	// a relative bin/tool on Windows, where both separators count, is probed
	// in the tool's Cwd rather than looked up on PATH.
	if filepath.Base(command) != command {
		return extensionToolFilePresent(filepath.Join(extensionToolWorkingDir(workspaceRoot, vars, def), command))
	}
	_, err := exec.LookPath(command)
	return err == nil
}

// extensionToolWorkingDir resolves Cwd once for both preflight and invocation.
// A relative command is interpreted by exec.Command from cmd.Dir, so probing it
// from the workspace root would make tools/list disagree with tools/call.
func extensionToolWorkingDir(workspaceRoot string, vars map[string]string, def extension.ToolDefinition) string {
	cwd := workspaceRoot
	if def.Cwd != "" {
		cwd = extension.ExpandTemplateVars(def.Cwd, vars)
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(workspaceRoot, cwd)
	}
	return cwd
}

func validExtensionTool(name string, def extension.ToolDefinition) bool {
	// The protocol package performs this validation during extension package
	// validation. Repeat the inexpensive shape checks here because normal
	// discovery intentionally tolerates older/bad manifests and MCP must never
	// surface an ambiguous or unsafe descriptor.
	if !isNamespacedExtensionToolName(name) || strings.TrimSpace(def.Description) == "" || strings.TrimSpace(def.Command) == "" || def.Annotations == nil {
		return false
	}
	if def.Annotations.ReadOnlyHint == nil || def.Annotations.DestructiveHint == nil || def.Annotations.IdempotentHint == nil || def.Annotations.OpenWorldHint == nil {
		return false
	}
	if def.TimeoutMs < 0 || len(def.InputSchema) == 0 {
		return false
	}
	var schema map[string]any
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil || schema == nil {
		return false
	}
	contract, ok := def.Meta["putnami.dev/contract"].(map[string]any)
	if !ok {
		return false
	}
	access, accessOK := contract["access"].(string)
	readOnly, readOnlyOK := contract["readOnly"].(bool)
	_, dryRunOK := contract["supportsDryRun"].(bool)
	return accessOK && (access == "read" || access == "mutating") && readOnlyOK && dryRunOK &&
		readOnly == (access == "read") && *def.Annotations.ReadOnlyHint == readOnly
}

func isNamespacedExtensionToolName(name string) bool {
	if strings.Count(name, ".") == 0 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func mcpToolFromExtension(name string, def extension.ToolDefinition) Tool {
	annotations := &toolAnnotations{
		ReadOnlyHint:    def.Annotations.ReadOnlyHint,
		DestructiveHint: def.Annotations.DestructiveHint,
		IdempotentHint:  def.Annotations.IdempotentHint,
		OpenWorldHint:   def.Annotations.OpenWorldHint,
	}
	meta := make(map[string]any, len(def.Meta))
	for key, value := range def.Meta {
		meta[key] = value
	}
	return Tool{
		Name:        name,
		Description: def.Description,
		InputSchema: append(json.RawMessage(nil), def.InputSchema...),
		Annotations: annotations,
		Meta:        meta,
	}
}

func (s *Server) callExtensionTool(ctx context.Context, candidate extensionToolCandidate, args json.RawMessage) (callToolResult, error) {
	agent := s.propagatedAgentIdentity()
	request := proto.ToolCallRequest{
		Name:          candidate.name,
		Arguments:     args,
		WorkspaceRoot: s.opts.WorkspaceRoot,
		ExtensionRoot: candidate.extensionRoot(),
		Agent:         agent,
	}
	// A tool that declared it answers about the workspace gets the orchestrator's
	// own resolved view, so it never has to derive one. The failure is the tool
	// call's, not the session's: a selector naming no project must read as
	// "projects not found: x", exactly as it does for a core tool.
	if candidate.def.WorkspaceSelection {
		if err := s.attachToolWorkspaceView(ctx, &request, args); err != nil {
			return callToolResult{}, err
		}
	}
	result, err := invokeExtensionTool(ctx, s.opts.WorkspaceRoot, candidate, request)
	if err != nil {
		return callToolResult{}, err
	}
	content := make([]toolContent, len(result.Content))
	for i, item := range result.Content {
		content[i] = toolContent{Type: item.Type, Text: item.Text}
	}
	return callToolResult{Content: content, IsError: result.IsError}, nil
}

// invocationStage is how far a failed tool invocation got. It is what decides
// whether a mutation can have happened: nothing ran before the process
// started, and anything may have run after.
type invocationStage int

const (
	// stagePrepare: the request or the runtime could not be prepared.
	stagePrepare invocationStage = iota
	// stageStart: the process could not start.
	stageStart
	// stageRun: the process ran and exited unsuccessfully.
	stageRun
	// stageTimeout: the tool's deadline killed the process.
	stageTimeout
	// stageCanceled: the caller canceled while the process ran.
	stageCanceled
	// stageOutput: the process exited successfully without one readable result.
	stageOutput
	// stageTimeoutBeforeStart: the tool's deadline passed before the process
	// started, so nothing ran.
	stageTimeoutBeforeStart
	// stageCanceledBeforeStart: the caller canceled before the process
	// started, so nothing ran.
	stageCanceledBeforeStart
)

// extensionToolError is a failed invocation and the stage it reached.
type extensionToolError struct {
	stage invocationStage
	err   error
	// withoutStderr is the same failure described without anything the
	// subprocess wrote to stderr, for a caller that must not publish it.
	withoutStderr string
}

func (e *extensionToolError) Error() string { return e.err.Error() }

func (e *extensionToolError) Unwrap() error { return e.err }

// summary describes the failure without the subprocess's stderr.
func (e *extensionToolError) summary() string {
	if e.withoutStderr != "" {
		return e.withoutStderr
	}
	return e.err.Error()
}

func invocationFailure(stage invocationStage, format string, args ...any) error {
	return &extensionToolError{stage: stage, err: fmt.Errorf(format, args...)}
}

// invokeExtensionTool runs one manifest-declared tool: it prepares the
// extension's runtime when the tool names it, writes request to the tool's
// stdin, and reads exactly one ToolCallResult of text content from its stdout.
// Every failure is an *extensionToolError naming the stage it reached. The
// subprocess's stderr is folded into a run failure's message; callers that
// must not publish it withhold the message.
func invokeExtensionTool(ctx context.Context, workspaceRoot string, candidate extensionToolCandidate, request proto.ToolCallRequest) (proto.ToolCallResult, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return proto.ToolCallResult{}, invocationFailure(stagePrepare, "encode extension tool request: %w", err)
	}

	timeout := defaultExtensionToolTimeout
	if candidate.def.TimeoutMs > 0 {
		timeout = time.Duration(candidate.def.TimeoutMs) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := extensionToolTemplateVars(callCtx, workspaceRoot, candidate)
	if err != nil {
		return proto.ToolCallResult{}, &extensionToolError{stage: stagePrepare, err: err}
	}
	command := extension.ExpandTemplateVars(candidate.def.Command, vars)
	argsForCommand := make([]string, len(candidate.def.Args))
	for i, arg := range candidate.def.Args {
		argsForCommand[i] = extension.ExpandTemplateVars(arg, vars)
	}
	cwd := extensionToolWorkingDir(workspaceRoot, vars, candidate.def)

	// A tool runs in the workspace, like a job: it counts as repository code
	// (runcredential.MarkRepositoryCodeStarted).
	runcredential.MarkRepositoryCodeStarted("extension tool " + candidate.name)
	cmd := exec.CommandContext(callCtx, command, argsForCommand...)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = append(extensionToolBaseEnv(),
		"PUTNAMI_WORKSPACE_ROOT="+workspaceRoot,
		"PUTNAMI_EXTENSION_ROOT="+candidate.extensionRoot(),
	)
	for key, value := range candidate.def.Env {
		cmd.Env = append(cmd.Env, key+"="+extension.ExpandTemplateVars(value, vars))
	}
	// Identity variables are reserved and appended last so a manifest cannot
	// disagree with the structured ToolCallRequest.Agent source of truth.
	if agent := request.Agent; agent != nil {
		if agent.Harness != "" {
			cmd.Env = append(cmd.Env, proto.AgentHarnessEnv+"="+agent.Harness)
		}
		if agent.Model != "" {
			cmd.Env = append(cmd.Env, proto.AgentModelEnv+"="+agent.Model)
		}
		if agent.UserAgent != "" {
			cmd.Env = append(cmd.Env, proto.AgentUserAgentEnv+"="+agent.UserAgent)
		}
	}
	stdout, err := cmd.Output()
	if err != nil {
		// Only an exit error proves the process started; every other error is
		// a start failure, after which nothing can have run.
		var exitErr *exec.ExitError
		started := errors.As(err, &exitErr)
		deadline := callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		switch {
		case !started && deadline:
			return proto.ToolCallResult{}, invocationFailure(stageTimeoutBeforeStart, "extension tool %s timed out after %s before it started", request.Name, timeout)
		case !started && ctx.Err() != nil:
			return proto.ToolCallResult{}, invocationFailure(stageCanceledBeforeStart, "extension tool %s was canceled before it started: %w", request.Name, ctx.Err())
		case !started:
			return proto.ToolCallResult{}, invocationFailure(stageStart, "extension tool %s failed: %w", request.Name, err)
		case deadline:
			return proto.ToolCallResult{}, invocationFailure(stageTimeout, "extension tool %s timed out after %s", request.Name, timeout)
		case ctx.Err() != nil:
			return proto.ToolCallResult{}, invocationFailure(stageCanceled, "extension tool %s was canceled: %w", request.Name, ctx.Err())
		}
		if msg := boundedToolStderr(stderr.String()); msg != "" {
			return proto.ToolCallResult{}, &extensionToolError{
				stage:         stageRun,
				err:           fmt.Errorf("extension tool %s failed: %s", request.Name, msg),
				withoutStderr: fmt.Sprintf("extension tool %s failed: %v", request.Name, err),
			}
		}
		return proto.ToolCallResult{}, invocationFailure(stageRun, "extension tool %s failed: %w", request.Name, err)
	}

	var result proto.ToolCallResult
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(&result); err != nil {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned invalid JSON: %w", request.Name, err)
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned invalid JSON: %w", request.Name, err)
	}
	if len(result.Content) == 0 {
		return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned no content", request.Name)
	}
	for _, item := range result.Content {
		if item.Type != "text" {
			return proto.ToolCallResult{}, invocationFailure(stageOutput, "extension tool %s returned unsupported content type %q", request.Name, item.Type)
		}
	}
	return result, nil
}

func (s *Server) propagatedAgentIdentity() *proto.AgentIdentity {
	if !s.opts.PropagateAgentIdentity {
		return nil
	}
	s.identityMu.RLock()
	identity := s.identity
	s.identityMu.RUnlock()
	if identity.UserAgent == "" {
		return nil
	}
	return &identity
}

func extensionToolBaseEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case proto.AgentHarnessEnv, proto.AgentModelEnv, proto.AgentUserAgentEnv:
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func boundedToolStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	const max = 4 << 10
	if len(stderr) > max {
		return stderr[:max] + "…"
	}
	return stderr
}
