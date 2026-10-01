package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// jsonSchemaProperties extracts the top-level property names from a JSON Schema file.
func jsonSchemaProperties(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// goTypeJSONFields extracts json tag field names from a struct type.
func goTypeJSONFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// jsonSchemaDefinitionProperties extracts the property names of one named
// definition within a JSON Schema file, so a schema that carries several
// shapes (the probe request/result/merged view) can be drift-checked per shape.
//
// The path parameter stays even though only schemas/probe.json carries multiple
// definitions today: it is the generic form of its two siblings above, and the
// next multi-shape schema must not have to re-derive it.
//
//nolint:unparam // deliberately general, mirroring jsonSchemaProperties/jsonSchemaJobProperties
func jsonSchemaDefinitionProperties(t *testing.T, path, definition string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	def, ok := schema.Definitions[definition]
	if !ok {
		t.Fatalf("no %q definition in %s", definition, path)
	}
	names := make([]string, 0, len(def.Properties))
	for name := range def.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// jsonSchemaMapEntryProperties extracts the field names accepted by a schema's
// top-level map property's additionalProperties object.
func jsonSchemaMapEntryProperties(t *testing.T, schemaPath, property string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	raw, ok := schema.Properties[property]
	if !ok {
		t.Fatalf("schema %s has no top-level property %q", schemaPath, property)
	}
	var entry struct {
		AdditionalProperties struct {
			Properties map[string]any `json:"properties"`
		} `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("parse schema %s property %q: %v", schemaPath, property, err)
	}
	names := make([]string, 0, len(entry.AdditionalProperties.Properties))
	for name := range entry.AdditionalProperties.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// jsonSchemaObjectProperties extracts the field names of a schema's top-level
// object property, for a shape declared inline rather than under definitions.
func jsonSchemaObjectProperties(t *testing.T, schemaPath, property string) []string {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	prop, ok := schema.Properties[property]
	if !ok {
		t.Fatalf("schema %s has no top-level property %q", schemaPath, property)
	}
	names := make([]string, 0, len(prop.Properties))
	for name := range prop.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestDrift_WorkspaceConfig(t *testing.T) {
	schemaFields := jsonSchemaProperties(t, "schemas/workspace.json")
	goFields := goTypeJSONFields(t, Config{})

	assertFieldParity(t, "Config", schemaFields, goFields)
}

func TestDrift_AgentArtifactManifest(t *testing.T) {
	schemaFields := jsonSchemaProperties(t, "schemas/agent-artifact.json")
	goFields := goTypeJSONFields(t, AgentArtifactManifest{})

	assertFieldParity(t, "AgentArtifactManifest", schemaFields, goFields)
}

func TestDrift_Lock(t *testing.T) {
	assertFieldParity(t, "Lock", jsonSchemaProperties(t, "schemas/lock.json"), goTypeJSONFields(t, Lock{}))
	assertFieldParity(t, "LockAgentArtifactEntry",
		jsonSchemaMapEntryProperties(t, "schemas/lock.json", "agentArtifacts"),
		goTypeJSONFields(t, LockAgentArtifactEntry{}))
}

// TestDrift_HookPhaseConfig keeps the hook execution contract addressable from
// the schema. The contract knobs timeoutMs and loginShell are
// exactly the fields an editor completes from the schema, so a field that only
// reaches the Go struct is invisible to the person writing the config.
func TestDrift_HookPhaseConfig(t *testing.T) {
	schemaFields := jsonSchemaDefinitionProperties(t, "schemas/workspace.json", "hookPhaseConfig")
	goFields := goTypeJSONFields(t, HookPhaseConfig{})

	assertFieldParity(t, "HookPhaseConfig", schemaFields, goFields)
}

// TestDrift_SessionsConfig keeps session retention addressable from the schema.
// `keep` is the single knob an editor completes from the schema, so a field that
// only reaches the Go struct is invisible to the person writing the config.
func TestDrift_SessionsConfig(t *testing.T) {
	schemaFields := jsonSchemaObjectProperties(t, "schemas/workspace.json", "sessions")
	goFields := goTypeJSONFields(t, SessionsConfig{})

	assertFieldParity(t, "SessionsConfig", schemaFields, goFields)
}

// TestDrift_SessionsKeepExcludesZero pins the schema's deliberate floor: a
// retained 0 would be readable as "unlimited" or as "delete everything", so the
// value is never accepted and neither reading can be guessed.
func TestDrift_SessionsKeepExcludesZero(t *testing.T) {
	data, err := os.ReadFile("schemas/workspace.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Type    string `json:"type"`
				Minimum *int   `json:"minimum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	keep := schema.Properties["sessions"].Properties["keep"]
	if keep.Type != "integer" {
		t.Errorf("sessions.keep type = %q, want integer", keep.Type)
	}
	if keep.Minimum == nil || *keep.Minimum != 1 {
		t.Errorf("sessions.keep minimum = %v, want 1 — 0 must stay unauthorable", keep.Minimum)
	}
}

func TestDrift_ProjectConfig(t *testing.T) {
	schemaFields := jsonSchemaProperties(t, "schemas/project.json")
	goFields := goTypeJSONFields(t, ProjectConfig{})

	assertFieldParity(t, "ProjectConfig", schemaFields, goFields)
}

func TestDrift_ProjectTypeIncludesFirstClassImage(t *testing.T) {
	data, err := os.ReadFile("schemas/project.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	want := []string{"application", "image", "library"}
	got := append([]string(nil), schema.Properties["type"].Enum...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project type enum = %v, want %v", got, want)
	}
}

func TestDrift_ProjectTaskTuning(t *testing.T) {
	schemaFields := jsonSchemaDefinitionProperties(t, "schemas/project.json", "taskTuning")
	goFields := goTypeJSONFields(t, ProjectTaskTuning{})

	assertFieldParity(t, "ProjectTaskTuning", schemaFields, goFields)
}

// TestDrift_ProjectJobsStayUnmodeled guards the descope: `jobs` is accepted so
// its presence can be reported, and it is opaque on BOTH sides so nothing can
// re-grow a field table for a surface planning never reads.
//
// The failure this prevents is the one the surface already caused once — a
// schema that documents kind/command/timeoutMs/cache, an editor that completes
// them, and a CLI that plans none of it. Re-modeling the shape here without
// wiring it into planning must fail loudly, not ship as documentation.
func TestDrift_ProjectJobsStayUnmodeled(t *testing.T) {
	data, err := os.ReadFile("schemas/project.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Properties  map[string]json.RawMessage `json:"properties"`
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	// No leftover shape to $ref, in either direction.
	for _, gone := range []string{"jobDefinition", "manifestFlagDefinition"} {
		if _, ok := schema.Definitions[gone]; ok {
			t.Errorf("schemas/project.json still defines %q; project-level jobs are not implemented, so there is no job shape to describe", gone)
		}
	}
	if strings.Contains(string(schema.Properties["jobs"]), "$ref") {
		t.Errorf("the `jobs` property references a modeled shape: %s", schema.Properties["jobs"])
	}

	// The description must say the key does nothing — this is the only place an
	// editor user learns it before their config silently no-ops.
	var jobs struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(schema.Properties["jobs"], &jobs); err != nil {
		t.Fatalf("parse jobs property: %v", err)
	}
	if !strings.Contains(strings.ToLower(jobs.Description), "ignored") {
		t.Errorf("the `jobs` description does not say the key is ignored: %q", jobs.Description)
	}

	// The Go side is opaque too: a job entry keeps its raw bytes and models
	// nothing, so no code can read a `command` out of it.
	goType := reflect.TypeOf(ProjectConfig{})
	field, ok := goType.FieldByName("Jobs")
	if !ok {
		t.Fatal("ProjectConfig has no Jobs field; it must stay parseable so its presence is detectable")
	}
	if want := reflect.TypeOf(map[string]json.RawMessage(nil)); field.Type != want {
		t.Errorf("ProjectConfig.Jobs = %s, want %s (opaque, unmodeled)", field.Type, want)
	}
}

func TestDrift_ScopeConfig(t *testing.T) {
	// scope.json is a dual-purpose schema: a putnami.json with `activate: true`
	// also acts as a project, so the schema accepts the union of scope and
	// project fields. The union is enforced/restricted at runtime by the
	// `if/then` clause, but at the property-name level the schema declares
	// both sets, so the drift check compares against ScopeConfig ∪ ProjectConfig.
	schemaFields := jsonSchemaProperties(t, "schemas/scope.json")

	scopeFields := goTypeJSONFields(t, ScopeConfig{})
	projectFields := goTypeJSONFields(t, ProjectConfig{})
	merged := make(map[string]bool, len(scopeFields)+len(projectFields))
	for _, f := range scopeFields {
		merged[f] = true
	}
	for _, f := range projectFields {
		merged[f] = true
	}
	goFields := make([]string, 0, len(merged))
	for f := range merged {
		goFields = append(goFields, f)
	}
	sort.Strings(goFields)

	assertFieldParity(t, "ScopeConfig", schemaFields, goFields)
}

// TestDrift_ProjectDistribution pins the distribution block against its
// schema; the scope schema references the same definition.
func TestDrift_ProjectDistribution(t *testing.T) {
	schemaFields := jsonSchemaObjectProperties(t, "schemas/project.json", "distribution")
	goFields := goTypeJSONFields(t, DistributionConfig{})

	assertFieldParity(t, "DistributionConfig", schemaFields, goFields)
}

// TestDrift_ScopeLine pins the line block against its schema. The tag pattern
// is what an author writes and what `version tag` reads back, so a field that
// only reaches the Go struct is a field no editor completes.
func TestDrift_ScopeLine(t *testing.T) {
	schemaFields := jsonSchemaObjectProperties(t, "schemas/scope.json", "line")
	goFields := goTypeJSONFields(t, LineConfig{})

	assertFieldParity(t, "LineConfig", schemaFields, goFields)
}

func TestWorkspaceConfig_ExtensionsArray(t *testing.T) {
	input := `{"extensions": ["@putnami/go", "@putnami/typescript"]}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal extensions array: %v", err)
	}
	names := cfg.Extensions.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(names))
	}
	// Names() returns sorted order
	if names[0] != "@putnami/go" || names[1] != "@putnami/typescript" {
		t.Errorf("extensions = %v", names)
	}
}

func TestWorkspaceConfig_ExtensionsObject(t *testing.T) {
	input := `{"extensions": {"@putnami/go": "^1.0.0", "@putnami/typescript": "^2.0.0"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal extensions object: %v", err)
	}
	if cfg.Extensions.List["@putnami/go"] != "^1.0.0" {
		t.Errorf("go constraint = %q", cfg.Extensions.List["@putnami/go"])
	}
	if cfg.Extensions.List["@putnami/typescript"] != "^2.0.0" {
		t.Errorf("ts constraint = %q", cfg.Extensions.List["@putnami/typescript"])
	}
}

func TestWorkspaceConfig_ExtensionsRoundtrip(t *testing.T) {
	// Array form round-trips as array
	cfg := Config{
		Extensions: ExtensionsConfig{List: map[string]string{
			"@putnami/go": "",
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var parsed Config
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.Extensions.List["@putnami/go"]; !ok {
		t.Error("expected @putnami/go in round-tripped config")
	}
}

func TestWorkspaceConfig_HooksRoundtrip(t *testing.T) {
	input := `{
		"hooks": {
			"cli": {"before": ["echo start"]},
			"commands": {
				"publish": {"before": ["echo pre-publish"], "after": ["echo done"]}
			}
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	if cfg.Hooks == nil {
		t.Fatal("hooks should not be nil")
	}
	if cfg.Hooks.CLI == nil || len(cfg.Hooks.CLI.Before) != 1 {
		t.Errorf("CLI hooks = %+v", cfg.Hooks.CLI)
	}
	if p := cfg.Hooks.Commands["publish"]; p == nil || len(p.Before) != 1 || len(p.After) != 1 {
		t.Errorf("publish hooks = %+v", p)
	}
}

// TestWorkspaceConfig_HookContractRoundtrip pins the hook contract knobs
// through JSON. Both are pointers so "absent" stays distinguishable from
// "explicitly 0 / false", which is what makes the merge below well-defined.
func TestWorkspaceConfig_HookContractRoundtrip(t *testing.T) {
	input := `{
		"hooks": {
			"cli": {"before": ["./scripts/pre.sh"], "timeoutMs": 5000, "loginShell": true}
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	cli := cfg.Hooks.CLI
	if cli.TimeoutMs == nil || *cli.TimeoutMs != 5000 {
		t.Errorf("timeoutMs = %v, want 5000", cli.TimeoutMs)
	}
	if cli.LoginShell == nil || !*cli.LoginShell {
		t.Errorf("loginShell = %v, want true", cli.LoginShell)
	}

	// Absent stays absent: a group that only lists commands must not claim a
	// bound or a login shell of its own — the CLI default applies instead.
	var bare Config
	if err := json.Unmarshal([]byte(`{"hooks":{"cli":{"before":["true"]}}}`), &bare); err != nil {
		t.Fatalf("unmarshal bare hooks: %v", err)
	}
	if bare.Hooks.CLI.TimeoutMs != nil || bare.Hooks.CLI.LoginShell != nil {
		t.Errorf("bare hook group = %+v, want both contract fields unset", bare.Hooks.CLI)
	}
}

// TestMergeHookPhase_ContractOverridesCommandsAccumulate pins the two different
// merge rules in one place: command lists accumulate across config scopes, the
// execution contract is replaced by the later scope, and silence at the later
// scope leaves the earlier contract intact.
func TestMergeHookPhase_ContractOverridesCommandsAccumulate(t *testing.T) {
	global := &HookPhaseConfig{
		Before:     []string{"global-before"},
		TimeoutMs:  intPtr(1000),
		LoginShell: boolPtr(true),
	}

	// A workspace file that only adds a command must not erase the global bound.
	silent := mergeHookPhase(global, &HookPhaseConfig{Before: []string{"ws-before"}})
	if got := strings.Join(silent.Before, ","); got != "global-before,ws-before" {
		t.Errorf("before = %q, want both scopes", got)
	}
	if silent.TimeoutMs == nil || *silent.TimeoutMs != 1000 {
		t.Errorf("timeoutMs = %v, want the global 1000 to survive", silent.TimeoutMs)
	}
	if silent.LoginShell == nil || !*silent.LoginShell {
		t.Errorf("loginShell = %v, want the global true to survive", silent.LoginShell)
	}

	// A workspace file that states a contract replaces it — including turning a
	// global login shell back off, which a plain bool could not express.
	stated := mergeHookPhase(silent, &HookPhaseConfig{
		TimeoutMs:  intPtr(250),
		LoginShell: boolPtr(false),
	})
	if stated.TimeoutMs == nil || *stated.TimeoutMs != 250 {
		t.Errorf("timeoutMs = %v, want the workspace 250", stated.TimeoutMs)
	}
	if stated.LoginShell == nil || *stated.LoginShell {
		t.Errorf("loginShell = %v, want the workspace false", stated.LoginShell)
	}
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// TestProjectConfig_JobsParseButStayOpaque pins the half of the descope that a
// deleted field would have broken: an EXISTING config that declares a full
// pre-v3 job block must still parse — strictly, unknown-field rejection and
// all — because that is what makes the load-time warning reachable. A dropped
// field would turn the same config into a rejected parse (strict) or an even
// quieter drop (lenient).
func TestProjectConfig_JobsParseButStayOpaque(t *testing.T) {
	input := `{
		"name": "p",
		"jobs": {
			"typecheck": {
				"kind": "command",
				"command": "npx",
				"args": ["tsc", "--noEmit"],
				"timeoutMs": 60000,
				"flags": {"target": {"type": "string"}}
			}
		}
	}`

	c, diags := ParseProjectConfig([]byte(input))
	if diag.HasErrors(diags) {
		t.Fatalf("a config declaring the old job shape must still parse: %v", diags)
	}
	if len(c.Jobs) != 1 {
		t.Fatalf("Jobs = %v, want the declared entry to survive parsing so it can be reported", c.Jobs)
	}
	// Opaque means opaque: the raw bytes are kept verbatim, which is also what
	// keeps re-serialization byte-stable.
	var entry map[string]any
	if err := json.Unmarshal(c.Jobs["typecheck"], &entry); err != nil {
		t.Fatalf("job entry is not retained as raw JSON: %v", err)
	}
	if entry["command"] != "npx" {
		t.Errorf("job entry lost its content: %v", entry)
	}
}

// assertFieldParity checks that schema fields and Go type fields match.
func assertFieldParity(t *testing.T, typeName string, schemaFields, goFields []string) {
	t.Helper()

	schemaSet := make(map[string]bool)
	for _, f := range schemaFields {
		schemaSet[f] = true
	}
	goSet := make(map[string]bool)
	for _, f := range goFields {
		goSet[f] = true
	}

	for _, f := range schemaFields {
		if !goSet[f] {
			t.Errorf("%s: field %q exists in schema but not in Go type", typeName, f)
		}
	}
	for _, f := range goFields {
		if !schemaSet[f] {
			t.Errorf("%s: field %q exists in Go type but not in schema", typeName, f)
		}
	}
}

// The `cli` entry is the one shape whose rule lives in two places: ValidateLock
// enforces it in Go, and schemas/lock.json states it as a two-branch `oneOf` for
// every non-Go consumer. Nothing in this module runs a JSON Schema validator —
// the repository carries no such dependency — so without this test the two could
// drift silently, and the schema would keep publishing a rule the CLI no longer
// applies.
//
// It reads both against lockCLIPublishedPinFields, the single list ValidateLock
// branches on, and additionally pins the structural facts that make the schema's
// two shapes mutually exclusive.
func TestLockSchemaAgreesWithTheCLIEntryValidator(t *testing.T) {
	var schema struct {
		Properties struct {
			CLI struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
				OneOf      []struct {
					Title      string         `json:"title"`
					Required   []string       `json:"required"`
					Properties map[string]any `json:"properties"`
					Not        *struct {
						AnyOf []struct {
							Required []string `json:"required"`
						} `json:"anyOf"`
					} `json:"not"`
				} `json:"oneOf"`
			} `json:"cli"`
		} `json:"properties"`
	}
	data, err := os.ReadFile("schemas/lock.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	cli := schema.Properties.CLI

	// An unconditional `required: ["version"]` is what the sentinel replaced. If
	// it comes back, every source workspace's lock becomes schema-invalid.
	if len(cli.Required) != 0 {
		t.Errorf("cli must not require fields unconditionally (it has two shapes); got required=%v", cli.Required)
	}
	if len(cli.OneOf) != 2 {
		t.Fatalf("cli must declare exactly two shapes; got %d", len(cli.OneOf))
	}

	pin, source := cli.OneOf[0], cli.OneOf[1]
	if !slices.Contains(pin.Required, "version") {
		t.Errorf("the published-pin shape must require a version; got %v", pin.Required)
	}
	if !slices.Contains(source.Required, "source") {
		t.Errorf("the source-workspace shape must require a source; got %v", source.Required)
	}
	if source.Not == nil {
		t.Fatal("the source-workspace shape must exclude every published-pin field")
	}
	excluded := make([]string, 0, len(source.Not.AnyOf))
	for _, branch := range source.Not.AnyOf {
		excluded = append(excluded, branch.Required...)
	}
	sort.Strings(excluded)
	want := slices.Clone(lockCLIPublishedPinFields)
	sort.Strings(want)
	if !reflect.DeepEqual(excluded, want) {
		t.Errorf("schema excludes %v from a source workspace, ValidateLock rejects %v", excluded, want)
	}

	// Every excluded name must be a real property, or the schema is excluding
	// something no lock can carry while the Go validator checks a live field.
	for _, name := range lockCLIPublishedPinFields {
		if _, ok := cli.Properties[name]; !ok {
			t.Errorf("cli.%s is validated but is not a declared schema property", name)
		}
	}

	// The two shapes must be mutually exclusive on `source`, or a sentinel would
	// satisfy both branches and `oneOf` would reject a valid lock.
	if !strings.Contains(string(data), `"not": { "const": "workspace" }`) {
		t.Error("the published-pin shape must exclude source == \"workspace\", or oneOf matches twice")
	}
	if fmt.Sprint(source.Properties["source"]) != fmt.Sprintf("map[const:%s]", LockCLISourceWorkspace) {
		t.Errorf("the source-workspace shape must pin source to %q; got %v",
			LockCLISourceWorkspace, source.Properties["source"])
	}
}
