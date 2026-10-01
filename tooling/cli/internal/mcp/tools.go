package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// toolHandler runs a tool. It returns the value to encode as the tool result
// (marshaled to a JSON text block) or an error, which is reported to the client
// as an isError tool result rather than a JSON-RPC error. A non-nil value that
// accompanies an error is preserved as the first content block so callers can
// still read structured diagnostics.
type toolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// toolResultHandler returns an already-shaped MCP tool result. Extension tools
// use this path because their subprocess protocol returns the MCP result shape
// directly; core tools continue to return ordinary values that are encoded as a
// JSON text block by handleToolsCall.
type toolResultHandler func(ctx context.Context, args json.RawMessage) (callToolResult, error)

// toolEntry pairs an advertised tool definition with its handler.
type toolEntry struct {
	def           Tool
	handler       toolHandler
	resultHandler toolResultHandler
}

// registerTools wires the spike's tool set. The set is read-heavy on purpose:
// the only state-changing tool is run_jobs, which the CLI already exposes.
func (s *Server) registerTools() {
	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolListProjects,
		Description: "List all projects in the Putnami workspace with id, name, path, type, tags, and direct dependencies. Use this to discover what is in the workspace before targeting a project.",
		InputSchema: schemaObject(``, nil),
	}), s.toolListProjects)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolDescribeProject,
		Description: "Describe one project: its dependencies, dependents, extensions, publish channels, and metadata. Accepts a project id (e.g. \"/tooling/cli\") or name.",
		InputSchema: schemaObject(`"project":{"type":"string","description":"Project id or name"}`, []string{"project"}),
	}), s.toolDescribeProject)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolAgentContext,
		Description: "Return one project's deterministic agent-context document built in-memory from committed facts, plus explicit artifact status and any native product-feature summaries from its optional build design graph. Use it to orient in a project without enumerating files by hand.",
		InputSchema: schemaObject(`"project":{"type":"string","description":"Project id or name"}`, []string{"project"}),
	}), s.toolAgentContext)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolWorkspaceMap,
		Description: "Return the workspace orientation map, rebuilt in memory from committed artifacts on every call: every project with its path, tags, dependency edges, runtime intercall edges, endpoint table, config keys, committed schemas, and README summary, plus the workspace docs index. Use it to answer \"which project owns X\", \"what depends on Y\", or \"where are the APIs/config/docs\" without shell exploration. Scope with section and project instead of loading the whole map.",
		InputSchema: schemaObject(`"section":{"type":"string","enum":["projects","dependencies","intercalls","apis","config-keys","schemas","docs","all"],"description":"Which slice of the map to return (default: all)"},"project":{"type":"string","description":"Optional project id or name; narrows the map to that one project"}`, nil),
	}), s.toolWorkspaceMap)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolDeps,
		Description: "Return a project's dependency or dependent projects in the SCHEDULING graph. direction defaults to dependencies; transitive follows the full graph and omits the seed project from the returned project list. direction=dependents answers the ordering family, an activated scope's implicit include-to-scope-self edges included, so it is not the answer to a blast-radius question: what a change actually selects is impacted, and why_impacted explains one such edge.",
		InputSchema: schemaObject(`"project":{"type":"string","description":"Project id or name"},"direction":{"type":"string","enum":["dependencies","dependents"],"description":"Which direction of the scheduling graph to follow (default: dependencies); dependents includes the implicit include-to-scope-self edges of an activated scope, which order the schedule and carry no impact"},"transitive":{"type":"boolean","description":"Return the full transitive graph instead of only direct neighbors"}`, []string{"project"}),
	}), s.toolDeps)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolFindOwner,
		Description: "Find the projects that own a workspace-relative file path: the nearest project whose directory holds the path, plus every project that claims it as an asset. Reading a file is not owning it, so a project that merely declared the path as a file input is not returned.",
		InputSchema: schemaObject(`"path":{"type":"string","description":"Workspace-relative or absolute file path"}`, []string{"path"}),
	}), s.toolFindOwner)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolWhyImpacted,
		Description: "Explain why changing one project impacts another by returning the shortest path over dependency, contract and extension-consumer edges — the same edges impacted widens through — from the changed project to the impacted project, with each edge's kind.",
		InputSchema: schemaObject(`"from":{"type":"string","description":"Changed project id or name"},"to":{"type":"string","description":"Potentially impacted project id or name"}`, []string{"from", "to"}),
	}), s.toolWhyImpacted)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolTopoSort,
		Description: "Return all workspace projects in dependency-first topological order, so every project appears after the projects it depends on.",
		InputSchema: schemaObject(``, nil),
	}), s.toolTopoSort)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolImpacted,
		Description: "List the projects affected by the current git changes versus a baseline branch (transitively, through dependency, contract and extension-consumer edges, with a reason per project: the changed file that claimed it or the edge that reached it). Use this to answer \"what does my change impact?\" before running jobs.",
		InputSchema: schemaObject(`"baseline":{"type":"string","description":"Git baseline ref to diff against. When omitted, Putnami resolves workspace config baseline, nearest configured epic branch, trunk (origin/HEAD, origin/main), local main/master, then the upstream tracking ref — never the current branch itself."}`, nil),
	}), s.toolImpacted)

	s.register(mutatingTool(Tool{
		Name:        commandmeta.MCPToolRunJobs,
		Description: "Select and run one or more workspace jobs (e.g. build, test, lint) through the same planner and engine as the CLI, then return bounded counts and failures with file:line:col diagnostics. Set dryRun=true to return the selected dependency plan without executing subprocesses or changing diagnostic state. Execution refuses long-lived serve mode and jobs declared to mutate external systems. This tool does NOT stream the full event log; after an executed run, call get_diagnostics for its complete diagnostic list.",
		InputSchema: schemaObject(`"commands":{"type":"array","items":{"type":"string"},"description":"Jobs to run, e.g. [\"lint\",\"test\",\"build\"]"},"projects":{"type":"array","items":{"type":"string"},"description":"Project ids or names to target (default: all projects)"},"impacted":{"type":"boolean","description":"Target only git-impacted projects instead of 'projects'"},"baseline":{"type":"string","description":"Git baseline for 'impacted'. When omitted, Putnami resolves workspace config baseline, nearest configured epic branch, trunk (origin/HEAD, origin/main), local main/master, then the upstream tracking ref — never the current branch itself."},"dryRun":{"type":"boolean","description":"Plan jobs and return the DAG without executing subprocesses or updating get_diagnostics state"}`, []string{"commands"}),
	}, "workspace", "cache"), s.toolRunJobs)

	s.register(readOnlyTool(Tool{
		Name:        commandmeta.MCPToolGetDiagnostics,
		Description: "Return file:line:col, severity, code, and message for the most recent executed run_jobs call in this server session, optionally filtered by severity. A dry run does not replace this state. Use it to re-read the complete failure set without rerunning work.",
		InputSchema: schemaObject(`"severity":{"type":"string","enum":["error","warning","info"],"description":"Only return diagnostics of this severity"}`, nil),
	}), s.toolGetDiagnostics)
}

func readOnlyTool(t Tool) Tool {
	trueVal := true
	falseVal := false
	t.Annotations = &toolAnnotations{
		ReadOnlyHint:    &trueVal,
		DestructiveHint: &falseVal,
		IdempotentHint:  &trueVal,
		OpenWorldHint:   &falseVal,
	}
	t.Meta = contractMeta("read", nil, false)
	return t
}

func mutatingTool(t Tool, mutates ...string) Tool {
	falseVal := false
	t.Annotations = &toolAnnotations{
		ReadOnlyHint:    &falseVal,
		DestructiveHint: &falseVal,
		IdempotentHint:  &falseVal,
		OpenWorldHint:   &falseVal,
	}
	t.Meta = contractMeta("mutating", mutates, true)
	return t
}

func contractMeta(access string, mutates []string, supportsDryRun bool) map[string]any {
	contract := map[string]any{
		"access":         access,
		"readOnly":       access == "read",
		"supportsDryRun": supportsDryRun,
	}
	if len(mutates) > 0 {
		contract["mutates"] = mutates
	}
	return map[string]any{"putnami.dev/contract": contract}
}

// register adds a tool, panicking on a duplicate name (a programming error that
// should surface at startup, mirroring the CLI command registry).
func (s *Server) register(def Tool, handler toolHandler) {
	if _, exists := s.byName[def.Name]; exists {
		panic("mcp: duplicate tool registration " + def.Name)
	}
	entry := toolEntry{def: def, handler: handler}
	s.tools = append(s.tools, entry)
	s.byName[def.Name] = entry
}

// registerResult adds a tool whose handler already returns the MCP tools/call
// result shape. Like register, it panics for duplicate programming mistakes;
// extension discovery filters collisions before this method is reached.
func (s *Server) registerResult(def Tool, handler toolResultHandler) {
	if _, exists := s.byName[def.Name]; exists {
		panic("mcp: duplicate tool registration " + def.Name)
	}
	entry := toolEntry{def: def, resultHandler: handler}
	s.tools = append(s.tools, entry)
	s.byName[def.Name] = entry
}

// toolDefs returns the advertised tool definitions in registration order.
func (s *Server) toolDefs() []Tool {
	defs := make([]Tool, len(s.tools))
	for i, t := range s.tools {
		defs[i] = t.def
	}
	return defs
}

// handleToolsCall dispatches a tools/call request to the named tool.
func (s *Server) handleToolsCall(ctx context.Context, msg *rpcMessage) *rpcResponse {
	var p callToolParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return s.fail(msg.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	entry, ok := s.byName[p.Name]
	if !ok {
		return s.fail(msg.ID, codeInvalidParams, "unknown tool: "+p.Name)
	}
	if entry.resultHandler != nil {
		result, err := entry.resultHandler(ctx, p.Arguments)
		if err != nil {
			return s.ok(msg.ID, callToolResult{
				Content: []toolContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
		}
		return s.ok(msg.ID, result)
	}

	data, err := entry.handler(ctx, p.Arguments)
	if err != nil {
		if data != nil {
			text, encodeErr := json.MarshalIndent(data, "", "  ")
			if encodeErr == nil {
				return s.ok(msg.ID, callToolResult{
					Content: []toolContent{
						{Type: "text", Text: string(text)},
						{Type: "text", Text: err.Error()},
					},
					IsError: true,
				})
			}
		}
		return s.ok(msg.ID, callToolResult{
			Content: []toolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
	}

	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return s.ok(msg.ID, callToolResult{
			Content: []toolContent{{Type: "text", Text: "encode result: " + err.Error()}},
			IsError: true,
		})
	}
	return s.ok(msg.ID, callToolResult{Content: []toolContent{{Type: "text", Text: string(text)}}})
}

// schemaObject builds a JSON Schema for a tool's input. props is the raw
// comma-separated property entries (empty for a no-arg tool); required lists the
// required property names. Keeping the schema as assembled raw JSON avoids a
// map-ordering dance for what are small, static shapes.
func schemaObject(props string, required []string) json.RawMessage {
	req := ""
	if len(required) > 0 {
		parts, _ := json.Marshal(required)
		req = fmt.Sprintf(`,"required":%s`, parts)
	}
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s},"additionalProperties":false%s}`, props, req))
}
