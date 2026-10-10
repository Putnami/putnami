package sdd

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	sdkmanifest "go.putnami.dev/sdk/extension/manifest"
)

// The five SDD MCP tools, authored.
//
// This file holds the AGENT half of the manifest and the assertions that pin
// it, for the same reason manifest_groups_test.go holds the interactive half:
// one authoring program, three readable pieces.
//
// # The names are dot-namespaced, and core's are deleted
//
// D4 settles them as `sdd.list_features`, `sdd.feature_context`,
// `sdd.list_specs` and `sdd.spec_context`, with NO alias for the undotted names
// core carries until the removal commit. Two rules make that the only possible
// shape: an extension tool name must contain a dot
// (isNamespacedExtensionToolName), and a core name always wins a collision
// (registerExtensionToolDefinitions). An extension that tried to keep
// `list_features` would therefore be silently shadowed today and invalid
// tomorrow.
//
// # Every tool declares workspaceSelection
//
// The five answer questions ABOUT THE WORKSPACE, and an extension has no
// workspace loader. `workspaceSelection: true` is the manifest asking the
// orchestrator for the view it already holds — the complete membership, and the
// projection `projects`/`impacted`/`baseline` resolve to — instead of this
// binary deriving a second one. Three of the five take no selection argument at
// all and still declare it: they need the MEMBERSHIP, and their selection is
// honestly "all".
//
// # They are read-only, and say so twice
//
// Once in `annotations` (all four hints, which the extension-tool validator
// requires) and once in `_meta["putnami.dev/contract"]`, whose `readOnly` must
// agree with `annotations.readOnlyHint` or the validator drops the tool. The
// pairing is what an agent harness reads to decide whether a call needs
// confirmation, so it is asserted below rather than trusted.

// toolSpec is one MCP tool as this manifest declares it.
type toolSpec struct {
	// name is the agent-facing tool name, dot-namespaced per D4.
	name string
	// description is the text an agent reads to choose a tool, pinned here
	// verbatim because a paraphrase changes behavior no other test would catch.
	description string
	// inputSchema is the arguments object: its properties, its required list,
	// and additionalProperties: false.
	inputSchema string
}

// sddMCPTools is the agent surface, in D4's order.
func sddMCPTools() []toolSpec {
	return []toolSpec{
		{
			name: "sdd.list_features",
			description: "List one page of the compact product-feature catalog derived from explicit native design declarations and durable putnami.features.json files at the workspace or exact project roots. " +
				"Use this first when the feature id is unknown, then read one feature's declarations, requirements and implementations with sdd.feature_context. " +
				"An entry carries the id, name, a one-line outcome summary, owner, target stage and counts. " +
				"The page is ordered by id, holds at most limit entries within a fixed byte budget, reports the total, and returns next while more entries exist: pass it back as cursor. " +
				"Narrow the catalog with projects or impacted to bound the work: project selection is applied FIRST and decides which design graphs are read at all, " +
				"then optional query terms filter semantic ids, names, outcomes, and owners. Durable declarations stay workspace-wide so feature identity and duplicate detection never depend on the selection.",
			inputSchema: `{"type":"object","properties":{` +
				`"query":{"type":"string","description":"Optional space-separated terms matched across feature id, name, outcome, and owner, applied after project selection"},` +
				`"projects":{"type":"array","items":{"type":"string"},"description":"Project ids or names owning the features to list (default: every project)"},` +
				`"impacted":{"type":"boolean","description":"List the features owned by the projects the current git change reaches, instead of 'projects'"},` +
				`"baseline":{"type":"string","description":"Git baseline ref for 'impacted'. When omitted, Putnami resolves workspace config baseline, nearest configured epic branch, trunk (origin/HEAD, origin/main), local main/master, then the upstream tracking ref — never the current branch itself."},` +
				`"cursor":{"type":"string","description":"The next value of the previous page; omit it for the first page"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"Most entries in the page (default 50, at most 200)"}` +
				`},"additionalProperties":false}`,
		},
		{
			name: "sdd.feature_context",
			description: "Return one explicitly authored feature with its sorted declaration sources and exact provenance. " +
				"Durable manifest intent is always retained; when native design graphs exist, technical facts are grouped into modules, APIs, dependencies, data, events, clients, projects, commands, and representative critical paths. " +
				"Accepts an exact feature id or an unambiguous final id segment and excludes raw graph storage and unrelated workspace diagnostics.",
			inputSchema: `{"type":"object","properties":{` +
				`"feature":{"type":"string","description":"Exact feature id or unambiguous final id segment"}` +
				`},"additionalProperties":false,"required":["feature"]}`,
		},
		{
			name: "sdd.list_specs",
			description: "List the durable feature specifications discovered as direct JSON children of specs/ at the workspace or exact project roots. " +
				"Each row carries the exact feature the spec details, its file path and owning project, its first intended outcomes, and its non-goal, requirement, and decision counts, plus how many authored features still have no spec. " +
				"Narrow it by owning project with projects or impacted; there is deliberately no filename filter, because a spec's identity is its feature field and never its filename. " +
				"Use it to find which spec to read; call sdd.spec_context for one whole document.",
			inputSchema: `{"type":"object","properties":{` +
				`"projects":{"type":"array","items":{"type":"string"},"description":"Project ids or names owning the specs to list (default: every project)"},` +
				`"impacted":{"type":"boolean","description":"List the specs owned by the projects the current git change reaches, instead of 'projects'"},` +
				`"baseline":{"type":"string","description":"Git baseline ref for 'impacted'. When omitted, Putnami resolves workspace config baseline, nearest configured epic branch, trunk (origin/HEAD, origin/main), local main/master, then the upstream tracking ref — never the current branch itself."}` +
				`},"additionalProperties":false}`,
		},
		{
			name: "sdd.spec_context",
			description: "Return one already-authored feature's durable spec: the intended outcomes, non-goals and agreed requirement sentences, the feature declaration that mints its identity, " +
				"every linked decision record with whether that record exists in this worktree, the exact source path and owning project, and the diagnostics scoped to that document. " +
				"Requires an exact feature id and reads no design graph, evidence document, or capability manifest.",
			inputSchema: `{"type":"object","properties":{` +
				`"feature":{"type":"string","description":"Exact authored feature id, e.g. \"billing/invoice-export\""}` +
				`},"additionalProperties":false,"required":["feature"]}`,
		},
		{
			name: "sdd.architecture_context",
			description: "Return one exact authored architecture domain from a worktree-only ARC/DARC evaluation. " +
				"The report carries the domain's complete imports and exports, every declared inbound and outbound edge, every touching observed edge, relevant findings, detector coverage, and structural diagnostics. " +
				"Requires a resolved workspace selection and never reads Git history or an adoption baseline.",
			inputSchema: `{"type":"object","properties":{` +
				`"domain":{"type":"string","description":"Exact manifest-authored architecture domain id"}` +
				`},"additionalProperties":false,"required":["domain"]}`,
		},
	}
}

// sddToolTimeoutMs is the invocation budget. It is stated rather than defaulted
// because the five tools read workspace-authored documents, and the
// default a manifest gets by saying nothing (30s) is the one this surface wants
// — writing it down keeps a later change to that default from silently changing
// this tool's contract.
const sddToolTimeoutMs = 30000

// withSDDMCPTools authors the five tools onto the builder.
func withSDDMCPTools(builder *sdkmanifest.Builder) *sdkmanifest.Builder {
	readOnly, mutating := true, false
	for _, tool := range sddMCPTools() {
		builder = builder.Tool(tool.name, proto.ToolDefinition{
			Description: tool.description,
			InputSchema: json.RawMessage(tool.inputSchema),
			Annotations: &proto.ToolAnnotations{
				ReadOnlyHint: &readOnly,
				// Not destructive, and idempotent: the tools read committed
				// documents and return a projection of them, so calling one twice
				// over an unchanged tree returns the same answer.
				DestructiveHint: &mutating,
				IdempotentHint:  &readOnly,
				// Closed world: everything they read is in this workspace. An
				// agent must not be told a local document read may reach the
				// network.
				OpenWorldHint: &mutating,
			},
			Meta: map[string]any{
				"putnami.dev/contract": map[string]any{
					"access":   "read",
					"readOnly": true,
					// Nothing here writes, so there is no dry run to support.
					// Declaring one would advertise a mode with no meaning.
					"supportsDryRun": false,
				},
			},
			// One executable answers jobs, subcommands and tools; `mcp-tool`
			// selects the bridge (cmd/putnami-sdd/mcptool.go).
			Command:   "{extensionRuntime}",
			Args:      []string{"mcp-tool"},
			Cwd:       "{workspaceRoot}",
			TimeoutMs: sddToolTimeoutMs,
			// The manifest asking the orchestrator for the workspace view it
			// already holds. Without it this binary would have to load a
			// workspace, and it must not (MUST NOT 2 and 3).
			WorkspaceSelection: true,
		})
	}
	return builder
}

// TestManifestDeclaresTheFiveTools pins the agent surface: the four names D4
// moved out of core plus the worktree-only architecture context added later.
//
// A name without a dot is rejected by the CLI's extension-tool validator, and a
// name core already owns is dropped in favor of core's — so an extension that
// tried to keep `list_features` would be invisible today (shadowed) and invalid
// tomorrow (undotted). Both failures are silent at run time, which is why they
// are checked here.
func TestManifestDeclaresTheFiveTools(t *testing.T) {
	m := committedManifest(t)
	// The undotted names core carried until the removal commit. Each tool must be
	// `sdd.` plus one of them, and must not be spelled like one bare.
	coreNames := map[string]bool{
		"list_features": true, "feature_context": true,
		"list_specs": true, "spec_context": true,
	}
	want := []string{"sdd.architecture_context", "sdd.feature_context", "sdd.list_features", "sdd.list_specs", "sdd.spec_context"}
	if got := sortedKeys(m.Tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest tools = %v, want %v", got, want)
	}
	for _, name := range want {
		if coreNames[name] {
			t.Errorf("tool %q is a core tool name; core always wins the collision and this tool would never register", name)
		}
		namespace, bare, found := strings.Cut(name, ".")
		if !found {
			t.Errorf("tool %q is not namespaced; the CLI's extension-tool validator drops it", name)
			continue
		}
		if namespace != "sdd" {
			t.Errorf("tool %q uses namespace %q, want sdd", name, namespace)
		}
		if name != "sdd.architecture_context" && !coreNames[bare] {
			t.Errorf("tool %q does not replace a core tool; D4 names exactly the four that move", name)
		}
	}
}

// TestEveryToolDeclaresTheFullReadOnlyContract is the extension-tool validator's
// own bar, asserted here so a tool that would be silently dropped from
// tools/list fails in this project's test run instead.
//
// The two halves must AGREE: `_meta` says access "read" and readOnly true, and
// `annotations.readOnlyHint` must equal that readOnly. A mismatch is not a
// warning anywhere — validExtensionTool simply returns false and the tool
// disappears, which is the hardest kind of failure to notice.
func TestEveryToolDeclaresTheFullReadOnlyContract(t *testing.T) {
	spectest.Proves(t, "architecture/executable-contracts", "contained-read-only-tooling", "the-tools-declare-and-honor-read-only")
	spectest.Proves(t, "tooling/specification-driven-development", "architecture-agent-context", "architecture-context-is-read-only-and-workspace-scoped")
	m := committedManifest(t)
	for _, name := range sortedKeys(m.Tools) {
		tool := m.Tools[name]
		if tool.Annotations == nil {
			t.Errorf("tool %q declares no annotations", name)
			continue
		}
		// hint -> the value every SDD tool must state. All four hints are required, so
		// an unset one is a failure and not a default.
		for _, hint := range []struct {
			name string
			got  *bool
			want bool
			why  string
		}{
			{"readOnlyHint", tool.Annotations.ReadOnlyHint, true, "every SDD tool reads committed documents and writes nothing"},
			{"destructiveHint", tool.Annotations.DestructiveHint, false, "nothing here destroys anything"},
			{"idempotentHint", tool.Annotations.IdempotentHint, true, "two calls over one tree return one answer"},
			{"openWorldHint", tool.Annotations.OpenWorldHint, false, "nothing it reads leaves this workspace"},
		} {
			if hint.got == nil {
				t.Errorf("tool %q leaves %s unset; all four hints are required", name, hint.name)
				continue
			}
			if *hint.got != hint.want {
				t.Errorf("tool %q %s = %v, want %v; %s", name, hint.name, *hint.got, hint.want, hint.why)
			}
		}

		contract, ok := tool.Meta["putnami.dev/contract"].(map[string]any)
		if !ok {
			t.Errorf("tool %q carries no putnami.dev/contract metadata", name)
			continue
		}
		if contract["access"] != "read" {
			t.Errorf("tool %q access = %v, want read", name, contract["access"])
		}
		if contract["readOnly"] != true {
			t.Errorf("tool %q contract readOnly = %v, want true", name, contract["readOnly"])
		}
		if contract["supportsDryRun"] != false {
			t.Errorf("tool %q supportsDryRun = %v, want false; nothing here writes", name, contract["supportsDryRun"])
		}
		if readOnly, isBool := contract["readOnly"].(bool); isBool &&
			tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("tool %q: annotations and contract disagree about readOnly; the validator drops it", name)
		}
	}
}

// TestEveryToolRunsTheBridgeWithTheWorkspaceView keeps the manifest and the
// dispatch table together, and pins the answer to the tension this task had to
// resolve.
//
// Every tool must reach the `mcp-tool` argument this binary routes on, through
// the prepared runtime rather than through some other executable.
//
// And all five report on the workspace while this binary has no loader and may
// not grow one. `workspaceSelection: true` is what makes the orchestrator
// publish the membership and the resolved selection on the request; without it
// every tool would receive a root and nothing else, and would answer —
// confidently, and emptily.
func TestEveryToolRunsTheBridgeWithTheWorkspaceView(t *testing.T) {
	m := committedManifest(t)
	for _, name := range sortedKeys(m.Tools) {
		tool := m.Tools[name]
		if tool.Command != "{extensionRuntime}" {
			t.Errorf("tool %q runs %q, want the prepared runtime", name, tool.Command)
		}
		if !reflect.DeepEqual(tool.Args, []string{"mcp-tool"}) {
			t.Errorf("tool %q args = %v, want [mcp-tool]", name, tool.Args)
		}
		if tool.Cwd != "{workspaceRoot}" {
			t.Errorf("tool %q cwd = %q, want the workspace root", name, tool.Cwd)
		}
		if tool.TimeoutMs != sddToolTimeoutMs {
			t.Errorf("tool %q timeoutMs = %d, want %d", name, tool.TimeoutMs, sddToolTimeoutMs)
		}
		if !tool.WorkspaceSelection {
			t.Errorf("tool %q does not declare workspaceSelection; it would be handed a workspace root and no membership", name)
		}
	}
}

// TestToolInputSchemasAreClosedObjects pins the argument surface: a closed
// object, so an argument the tool does not understand is a rejection and never
// a silently ignored key, and the three exact-target tools require their target.
//
// The property tables also pin the canonical selection vocabulary, which is the
// other half of workspaceSelection: the orchestrator resolves `projects`,
// `impacted` and `baseline` by NAME, so a catalog tool that spelled one of them
// differently would declare the member and never receive a narrowing.
func TestToolInputSchemasAreClosedObjects(t *testing.T) {
	m := committedManifest(t)
	wantRequired := map[string][]string{
		"sdd.architecture_context": {"domain"},
		"sdd.list_features":        nil,
		"sdd.list_specs":           nil,
		"sdd.feature_context":      {"feature"},
		"sdd.spec_context":         {"feature"},
	}
	wantProperties := map[string][]string{
		"sdd.architecture_context": {"domain"},
		"sdd.list_features":        {"baseline", "cursor", "impacted", "limit", "projects", "query"},
		"sdd.list_specs":           {"baseline", "impacted", "projects"},
		"sdd.feature_context":      {"feature"},
		"sdd.spec_context":         {"feature"},
	}
	for _, name := range sortedKeys(m.Tools) {
		var schema struct {
			Type                 string          `json:"type"`
			Properties           map[string]any  `json:"properties"`
			AdditionalProperties *bool           `json:"additionalProperties"`
			Required             []string        `json:"required"`
			Raw                  json.RawMessage `json:"-"`
		}
		if err := json.Unmarshal(m.Tools[name].InputSchema, &schema); err != nil {
			t.Errorf("tool %q input schema does not decode: %v", name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("tool %q input schema type = %q, want object", name, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("tool %q accepts additional properties; an unknown argument must be a rejection", name)
		}
		properties := make([]string, 0, len(schema.Properties))
		for property := range schema.Properties {
			properties = append(properties, property)
		}
		sort.Strings(properties)
		if !reflect.DeepEqual(properties, wantProperties[name]) {
			t.Errorf("tool %q properties = %v, want %v", name, properties, wantProperties[name])
		}
		if !reflect.DeepEqual(schema.Required, wantRequired[name]) {
			t.Errorf("tool %q required = %v, want %v", name, schema.Required, wantRequired[name])
		}
	}
}
