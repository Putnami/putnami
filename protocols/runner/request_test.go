package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func requestIdentity(project, task, command string) protocolcli.TaskIdentity {
	return protocolcli.TaskIdentity{
		Key: project + ":" + task, Scope: protocolcli.TaskScopeProject,
		Project:  protocolcli.ProjectIdentity{ID: project, Name: strings.TrimPrefix(project, "/")},
		Task:     protocolcli.TaskRef{Name: task, Command: command, Kind: task},
		Provider: protocolcli.ProviderIdentity{Extension: "@fixture/gate", Version: "1.0.0"},
	}
}

func validRequest() ExecutionRequest {
	head := strings.Repeat("a", 40)
	return ExecutionRequest{
		Version:  ExecutionRequestVersion,
		Protocol: ProtocolBlock{Version: ProviderProtocolVersion, Capabilities: []string{CapabilityExecutionRequestV1, CapabilitySessionBundleV1}},
		Source: SourceBlock{
			Digest: BlobDigest([]byte("manifest")), IndexDigest: strings.Repeat("b", 64),
			Git:      GitContext{Head: head, Branch: "feature/portable", Dirty: true},
			Tree:     &TreeIdentity{Fingerprint: strings.Repeat("c", 64), Dirty: true, HeadSHA: head},
			Versions: []LineVersion{{Line: "", Base: "0.3.0", Full: "0.3.0-20260916000000-aaaaaaa-d1d1d1d", SHA: "aaaaaaa", Branch: "feature/portable", Suffix: "20260916000000-aaaaaaa-d1d1d1d", Dirty: true}},
		},
		Invocation: InvocationBlock{
			Commands: []string{"lint", "test", "build"},
			Params: map[string]ParamValue{
				"coverage": {Type: ParamTypeString, Value: "true"},
				"fix":      {Type: ParamTypeBool, Value: false},
				"retries":  {Type: ParamTypeInt, Value: 3},
				"ratio":    {Type: ParamTypeFloat, Value: 0.5},
				"only":     {Type: ParamTypeStrings, Value: []string{"a", "b"}},
			},
			Flags: ExecutionFlags{NoCache: true, NoCacheExplicit: true, ContinueOnError: true, Retry: 1, MaxParallelMode: "auto", CacheTrust: "ci", Output: "json", Profile: "dev", ResourceBudgets: map[string]int{"db": 2}},
			Cwd:   ".",
		},
		Selection: SelectionBlock{
			RequestedMode: SelectionModeImpacted, Mode: SelectionModeImpacted, Scoped: true,
			Projects: []string{"/app", "/lib"}, Baseline: strings.Repeat("d", 40), BaselineSource: "merge-base",
			ChangedPaths: []string{"app/main.go", "lib/lib.go"}, Diagnostics: []string{"baseline: merge-base with main"},
			NoCacheProjects: []string{"/app"},
		},
		Plan: PlanBlock{Tasks: []PlannedTask{
			{Identity: requestIdentity("/app", "build", "build"), DependsOn: []string{"/lib:build"}, SerializeAfter: []string{}, ContractDigest: "tc1:" + strings.Repeat("e", 64), DeadlineMs: 300000, Cacheable: true, Resources: TaskResources{CPUWeight: 1, Reads: []TaskResource{}, Writes: []TaskResource{{ID: "db", Scope: "workspace"}}}},
			{Identity: requestIdentity("/lib", "build", "build"), DependsOn: []string{}, SerializeAfter: []string{}, ContractDigest: "tc1:" + strings.Repeat("e", 64), DeadlineMs: 300000, Cacheable: true, Resources: TaskResources{Heavy: true, CPUWeight: 2, Reads: []TaskResource{{ID: "db", Scope: "workspace"}}, Writes: []TaskResource{}}},
		}},
		Environment: EnvironmentBlock{
			CLI:        PinnedCLI{Source: CLISourceWorkspace},
			Extensions: []PinnedComponent{{Name: "@fixture/gate", Version: "1.0.0"}},
			Toolchains: []PinnedComponent{{Name: "go", Version: "1.26.1"}},
			Platform:   Platform{OS: "linux", Arch: "arm64"},
		},
		Control: ControlBlock{Caller: CallerCLI, IdempotencyKey: strings.Repeat("f", 32), Deadline: "2026-09-16T12:00:00Z"},
	}
}

func TestExecutionRequestRoundTripsCanonically(t *testing.T) {
	t.Parallel()
	request := validRequest()
	canonical, err := CanonicalExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExecutionRequest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, request) {
		t.Fatalf("round trip changed the request:\n%+v\n%+v", parsed, request)
	}
	again, err := CanonicalExecutionRequest(parsed)
	if err != nil || !bytes.Equal(again, canonical) {
		t.Fatalf("canonical bytes are not stable: %v", err)
	}
	native, err := NativeParams(parsed.Invocation.Params)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"coverage": "true", "fix": false, "retries": 3, "ratio": 0.5, "only": []string{"a", "b"}}
	if !reflect.DeepEqual(native, want) {
		t.Fatalf("native params = %#v, want %#v", native, want)
	}
}

func TestExecutionInputDigestExcludesPlanAndControl(t *testing.T) {
	t.Parallel()
	base := validRequest()
	digest, err := ExecutionInputDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	same := validRequest()
	same.Control.IdempotencyKey = strings.Repeat("0", 32)
	same.Control.Deadline = "2027-01-01T00:00:00Z"
	same.Plan.Tasks = same.Plan.Tasks[1:]
	same.Protocol.Capabilities = []string{CapabilityExecutionRequestV1}
	same.Selection.Diagnostics = []string{"a different human notice"}
	if got, err := ExecutionInputDigest(same); err != nil || got != digest {
		t.Fatalf("control, plan, negotiated capabilities and diagnostics changed the input digest: %s vs %s (%v)", got, digest, err)
	}
	mutations := map[string]func(*ExecutionRequest){
		"source":  func(r *ExecutionRequest) { r.Source.Digest = BlobDigest([]byte("other")) },
		"tree":    func(r *ExecutionRequest) { r.Source.Tree = nil },
		"index":   func(r *ExecutionRequest) { r.Source.IndexDigest = strings.Repeat("1", 64) },
		"git":     func(r *ExecutionRequest) { r.Source.Git.Dirty = false },
		"version": func(r *ExecutionRequest) { r.Source.Versions[0].Full = "0.3.0" },
		"command": func(r *ExecutionRequest) { r.Invocation.Commands = []string{"build"} },
		"param-type": func(r *ExecutionRequest) {
			r.Invocation.Params["coverage"] = ParamValue{Type: ParamTypeBool, Value: true}
		},
		"param-value":         func(r *ExecutionRequest) { r.Invocation.Params["retries"] = ParamValue{Type: ParamTypeInt, Value: 4} },
		"explicit-flag":       func(r *ExecutionRequest) { r.Invocation.Flags.NoCacheExplicit = false },
		"budget":              func(r *ExecutionRequest) { r.Invocation.Flags.ResourceBudgets["db"] = 3 },
		"cwd":                 func(r *ExecutionRequest) { r.Invocation.Cwd = "app" },
		"projects":            func(r *ExecutionRequest) { r.Selection.Projects = []string{"/app"} },
		"baseline":            func(r *ExecutionRequest) { r.Selection.Baseline = strings.Repeat("9", 40) },
		"mode":                func(r *ExecutionRequest) { r.Selection.RequestedMode = SelectionModeAll },
		"no-cache-proj":       func(r *ExecutionRequest) { r.Selection.NoCacheProjects = []string{} },
		"task-scope":          func(r *ExecutionRequest) { r.Selection.TaskScopes = map[string][]string{"/app": {"/lib"}} },
		"cli":                 func(r *ExecutionRequest) { r.Environment.CLI = PinnedCLI{Source: CLISourcePublished, Version: "1.0.0"} },
		"extension":           func(r *ExecutionRequest) { r.Environment.Extensions[0].Version = "2.0.0" },
		"toolchain":           func(r *ExecutionRequest) { r.Environment.Toolchains = []PinnedComponent{} },
		"platform":            func(r *ExecutionRequest) { r.Environment.Platform.Arch = "amd64" },
		"bound":               func(r *ExecutionRequest) { r.Source.Bound = []string{"app/conf/local.txt"} },
		"providers":           func(r *ExecutionRequest) { r.Invocation.Providers = []string{InvocationProviderInstall} },
		"publication":         func(r *ExecutionRequest) { publishAfterBuild(r, "build") },
		"publication-barrier": func(r *ExecutionRequest) { publishAfterBuild(r, "build", "lint") },
	}
	seen := map[string]string{digest: "base"}
	for name, mutate := range mutations {
		request := validRequest()
		mutate(&request)
		got, err := ExecutionInputDigest(request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if previous, collision := seen[got]; collision {
			t.Fatalf("%s did not change the input digest (equals %s)", name, previous)
		}
		seen[got] = name
	}
}

func TestExecutionRequestRejectsSemanticViolations(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*ExecutionRequest){
		"version":            func(r *ExecutionRequest) { r.Version = 2 },
		"protocol":           func(r *ExecutionRequest) { r.Protocol.Version = 2 },
		"unsorted caps":      func(r *ExecutionRequest) { r.Protocol.Capabilities = []string{"b", "a"} },
		"source digest":      func(r *ExecutionRequest) { r.Source.Digest = "sha1:abc" },
		"index digest":       func(r *ExecutionRequest) { r.Source.IndexDigest = "abc" },
		"tree":               func(r *ExecutionRequest) { r.Source.Tree.HeadSHA = "short" },
		"versions nil":       func(r *ExecutionRequest) { r.Source.Versions = nil },
		"bound unsorted":     func(r *ExecutionRequest) { r.Source.Bound = []string{"b", "a"} },
		"bound duplicate":    func(r *ExecutionRequest) { r.Source.Bound = []string{"a", "a"} },
		"bound git":          func(r *ExecutionRequest) { r.Source.Bound = []string{".git/config"} },
		"bound traversal":    func(r *ExecutionRequest) { r.Source.Bound = []string{"../outside"} },
		"no commands":        func(r *ExecutionRequest) { r.Invocation.Commands = []string{} },
		"duplicate commands": func(r *ExecutionRequest) { r.Invocation.Commands = []string{"build", "build"} },
		"shell command":      func(r *ExecutionRequest) { r.Invocation.Commands = []string{"build; rm -rf /"} },
		"params nil":         func(r *ExecutionRequest) { r.Invocation.Params = nil },
		"param type":         func(r *ExecutionRequest) { r.Invocation.Params["x"] = ParamValue{Type: "map", Value: "1"} },
		"param mismatch":     func(r *ExecutionRequest) { r.Invocation.Params["x"] = ParamValue{Type: ParamTypeInt, Value: "1"} },
		"negative retry":     func(r *ExecutionRequest) { r.Invocation.Flags.Retry = -1 },
		"budgets nil":        func(r *ExecutionRequest) { r.Invocation.Flags.ResourceBudgets = nil },
		"absolute cwd":       func(r *ExecutionRequest) { r.Invocation.Cwd = "/tmp" },
		"parent cwd":         func(r *ExecutionRequest) { r.Invocation.Cwd = "../x" },
		"empty providers":    func(r *ExecutionRequest) { r.Invocation.Providers = []string{} },
		"unsorted providers": func(r *ExecutionRequest) { r.Invocation.Providers = []string{"publish", "install"} },
		"unknown provider":   func(r *ExecutionRequest) { r.Invocation.Providers = []string{"deploy"} },
		"repeated provider":  func(r *ExecutionRequest) { r.Invocation.Providers = []string{"install", "install"} },
		"empty barrier":      func(r *ExecutionRequest) { r.Invocation.Publication = &PublicationBlock{} },
		"unlisted barrier":   func(r *ExecutionRequest) { r.Invocation.Publication = &PublicationBlock{Barrier: []string{"qualify"}} },
		"unsorted barrier": func(r *ExecutionRequest) {
			r.Invocation.Publication = &PublicationBlock{Barrier: []string{"test", "lint"}}
		},
		"publishing barrier": func(r *ExecutionRequest) {
			r.Invocation.Commands = []string{"build", "publish"}
			r.Invocation.Publication = &PublicationBlock{Barrier: []string{"publish"}}
		},
		"publish unauthorized": func(r *ExecutionRequest) {
			r.Plan.Tasks[0].Identity = requestIdentity("/app", "publish", "publish")
		},
		"mode":              func(r *ExecutionRequest) { r.Selection.Mode = "some" },
		"scoped":            func(r *ExecutionRequest) { r.Selection.Scoped = false },
		"empty projects":    func(r *ExecutionRequest) { r.Selection.Projects = []string{} },
		"unsorted projects": func(r *ExecutionRequest) { r.Selection.Projects = []string{"/lib", "/app"} },
		"short baseline":    func(r *ExecutionRequest) { r.Selection.Baseline = "abc" },
		"baseline on all": func(r *ExecutionRequest) {
			r.Selection.Mode, r.Selection.Scoped, r.Selection.RequestedMode = SelectionModeAll, false, SelectionModeAll
		},
		"diagnostics nil":   func(r *ExecutionRequest) { r.Selection.Diagnostics = nil },
		"scope unselected":  func(r *ExecutionRequest) { r.Selection.TaskScopes = map[string][]string{"/other": {"/lib"}} },
		"scope empty":       func(r *ExecutionRequest) { r.Selection.TaskScopes = map[string][]string{"/app": {}} },
		"scope unsorted":    func(r *ExecutionRequest) { r.Selection.TaskScopes = map[string][]string{"/app": {"/lib", "/ext"}} },
		"plan nil":          func(r *ExecutionRequest) { r.Plan.Tasks = nil },
		"plan unsorted":     func(r *ExecutionRequest) { r.Plan.Tasks[0], r.Plan.Tasks[1] = r.Plan.Tasks[1], r.Plan.Tasks[0] },
		"plan key":          func(r *ExecutionRequest) { r.Plan.Tasks[0].Identity.Key = "/app:other" },
		"plan scope":        func(r *ExecutionRequest) { r.Plan.Tasks[0].Identity.Scope = "global" },
		"plan dangling":     func(r *ExecutionRequest) { r.Plan.Tasks[0].DependsOn = []string{"/missing:build"} },
		"plan self edge":    func(r *ExecutionRequest) { r.Plan.Tasks[0].DependsOn = []string{"/app:build"} },
		"plan deadline":     func(r *ExecutionRequest) { r.Plan.Tasks[0].DeadlineMs = 0 },
		"plan weight":       func(r *ExecutionRequest) { r.Plan.Tasks[0].Resources.CPUWeight = 0 },
		"plan resource nil": func(r *ExecutionRequest) { r.Plan.Tasks[0].Resources.Reads = nil },
		"plan contract":     func(r *ExecutionRequest) { r.Plan.Tasks[0].ContractDigest = "xyz" },
		"cli source":        func(r *ExecutionRequest) { r.Environment.CLI.Source = "baked" },
		"cli version":       func(r *ExecutionRequest) { r.Environment.CLI.Version = "1.0.0" },
		"published":         func(r *ExecutionRequest) { r.Environment.CLI = PinnedCLI{Source: CLISourcePublished} },
		"extensions nil":    func(r *ExecutionRequest) { r.Environment.Extensions = nil },
		"platform":          func(r *ExecutionRequest) { r.Environment.Platform.OS = "" },
		"caller":            func(r *ExecutionRequest) { r.Control.Caller = "ci" },
		"key":               func(r *ExecutionRequest) { r.Control.IdempotencyKey = "abc" },
		"deadline":          func(r *ExecutionRequest) { r.Control.Deadline = "2026-09-16T12:00:00+02:00" },
	}
	for name, mutate := range cases {
		request := validRequest()
		mutate(&request)
		if err := ValidateExecutionRequest(request); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestExecutionRequestFixtures reads the corpus through the bound-request
// channel's parser, which dispatches on version. Each version's own parser
// accepts exactly that version's valid fixtures, so a version 1 document parses,
// encodes and digests as it does without a version 2.
func TestExecutionRequestFixtures(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("fixtures", "execution-request", "digests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("fixtures", "execution-request", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			if validity == "valid" {
				valid++
			}
			t.Run(validity+"/"+filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				bound, err := ParseBoundRequest(data)
				_, snapshotErr := ParseExecutionRequest(data)
				_, commitErr := ParseCommitRequest(data)
				if validity == "invalid" {
					if err == nil || snapshotErr == nil || commitErr == nil {
						t.Fatalf("invalid fixture accepted (bound %v, v1 %v, v2 %v): %s", err, snapshotErr, commitErr, data)
					}
					if want := fixtureRefusals[filepath.Base(path)]; want != "" && !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q does not name %q", err, want)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var canonical []byte
				var digest string
				switch {
				case bound.Snapshot != nil && bound.Commit == nil:
					if snapshotErr != nil || commitErr == nil {
						t.Fatalf("a version 1 fixture: v1 parser %v, v2 parser %v", snapshotErr, commitErr)
					}
					canonical, err = CanonicalExecutionRequest(*bound.Snapshot)
					if err == nil {
						digest, err = ExecutionInputDigest(*bound.Snapshot)
					}
				case bound.Commit != nil && bound.Snapshot == nil:
					if commitErr != nil || snapshotErr == nil {
						t.Fatalf("a version 2 fixture: v2 parser %v, v1 parser %v", commitErr, snapshotErr)
					}
					canonical, err = CanonicalCommitRequest(*bound.Commit)
					if err == nil {
						digest, err = CommitInputDigest(*bound.Commit)
					}
				default:
					t.Fatalf("bound request sets %v and %v", bound.Snapshot != nil, bound.Commit != nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, data); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(canonical, compact.Bytes()) {
					t.Fatalf("noncanonical fixture:\n%s\n%s", canonical, compact.Bytes())
				}
				if digest != golden[filepath.Base(path)] {
					t.Fatalf("digest = %s; want %s", digest, golden[filepath.Base(path)])
				}
			})
		}
	}
	if len(golden) != valid {
		t.Fatalf("digests.json pins %d digests for %d valid fixtures", len(golden), valid)
	}
}

// fixtureRefusals names, for the invalid fixtures that mix the two versions or
// break a version 2 pairing rule, what the refusal must say.
var fixtureRefusals = map[string]string{
	"wrong-version.json":                         "carries no plan",
	"caller-unknown.json":                        "calls a version 2 request only",
	"snapshot-commit-source.json":                "source.commit addresses a commit",
	"version-unsupported.json":                   "unsupported execution request version 3",
	"commit-with-plan.json":                      "a frozen plan names no commit it was planned from",
	"commit-with-environment.json":               "carries no environment",
	"commit-with-source-digest.json":             "source.digest addresses a snapshot",
	"commit-with-source-versions.json":           "source.versions addresses a snapshot",
	"commit-with-frozen-selection.json":          "selection.requestedMode is a frozen selection output",
	"commit-base-without-impacted.json":          "source.base is the impacted baseline",
	"commit-impacted-without-base.json":          "an impacted selection needs source.base",
	"commit-projects-without-selectors.json":     "a projects selection must list its selectors",
	"commit-projects-outside-projects-mode.json": "selection.projects belongs to the projects mode",
	"commit-projects-empty.json":                 "selection.projects must be a non-empty array",
	"commit-base-empty.json":                     "source.base must be a full commit id",
	"commit-short-commit.json":                   "source.commit must be a full lowercase commit id",
	"commit-mixed-object-format.json":            "one object format",
	"commit-selector-comma.json":                 "holds a comma",
	"commit-selector-mode.json":                  "spells a selection mode",
	"commit-caller-unknown.json":                 "control.caller \"bot\" is not supported",
	"commit-unknown-root-field.json":             "unknown field \"extra\"",
}

func TestExecutionRequestSchemaTracksWireShape(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("schemas", "execution-request-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool     `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           struct {
			Version struct {
				Const int `json:"const"`
			} `json:"version"`
			Invocation struct {
				Properties struct {
					Params struct {
						AdditionalProperties struct {
							Properties struct {
								Type struct {
									Enum []string `json:"enum"`
								} `json:"type"`
							} `json:"properties"`
						} `json:"additionalProperties"`
					} `json:"params"`
					Providers struct {
						MaxItems int `json:"maxItems"`
						Items    struct {
							Enum []string `json:"enum"`
						} `json:"items"`
					} `json:"providers"`
					Publication struct {
						AdditionalProperties bool     `json:"additionalProperties"`
						Required             []string `json:"required"`
					} `json:"publication"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"invocation"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || schema.Properties.Version.Const != ExecutionRequestVersion {
		t.Fatal("schema version or strict fields diverged")
	}
	if strings.Join(schema.Required, ",") != "version,protocol,source,invocation,selection,plan,environment,control" {
		t.Fatalf("schema block order diverged: %v", schema.Required)
	}
	if strings.Join(schema.Properties.Invocation.Properties.Params.AdditionalProperties.Properties.Type.Enum, ",") != strings.Join([]string{ParamTypeString, ParamTypeBool, ParamTypeInt, ParamTypeFloat, ParamTypeStrings}, ",") {
		t.Fatal("schema parameter types diverged")
	}
	invocation := schema.Properties.Invocation
	if strings.Join(invocation.Required, ",") != "commands,params,flags,cwd" {
		t.Fatalf("schema invocation required members diverged: %v", invocation.Required)
	}
	if strings.Join(invocation.Properties.Providers.Items.Enum, ",") != strings.Join(InvocationProviders, ",") || invocation.Properties.Providers.MaxItems != len(InvocationProviders) {
		t.Fatal("schema invocation providers diverged")
	}
	if invocation.Properties.Publication.AdditionalProperties || strings.Join(invocation.Properties.Publication.Required, ",") != "barrier" {
		t.Fatal("schema invocation publication diverged")
	}
}

func TestParamProjectionKeepsGoTypes(t *testing.T) {
	t.Parallel()
	for value, want := range map[any]string{"1": ParamTypeString, true: ParamTypeBool, 1: ParamTypeInt, int64(2): ParamTypeInt, 1.5: ParamTypeFloat} {
		param, err := NewParam(value)
		if err != nil || param.Type != want {
			t.Errorf("NewParam(%#v) = %+v, %v; want %s", value, param, err, want)
		}
	}
	if _, err := NewParam(map[string]any{}); err == nil {
		t.Error("map parameter projected")
	}
	if _, err := NewParam(uint(1)); err == nil {
		t.Error("unsigned parameter projected")
	}
	list, err := NewParam([]any{"a", "b"})
	if err != nil || !reflect.DeepEqual(list.Value, []string{"a", "b"}) {
		t.Errorf("list projection = %+v, %v", list, err)
	}
	var decoded ParamValue
	if err := json.Unmarshal([]byte(`{"type":"int","value":1.5}`), &decoded); err == nil {
		t.Error("fractional int decoded")
	}
	if err := json.Unmarshal([]byte(`{"type":"int","value":"1"}`), &decoded); err == nil {
		t.Error("string decoded as int")
	}
	if err := json.Unmarshal([]byte(`{"type":"strings","value":[1]}`), &decoded); err == nil {
		t.Error("numeric list decoded as strings")
	}
	if err := json.Unmarshal([]byte(`{"type":"int","value":7}`), &decoded); err != nil || decoded.Value != 7 {
		t.Errorf("int decode = %#v, %v", decoded.Value, err)
	}
}

// TestInvocationExtensionsKeepAbsentMembersOffTheWire pins that a request
// enabling no provider and planning no publication encodes exactly as it did
// before the members existed: the canonical bytes carry neither name.
func TestInvocationExtensionsKeepAbsentMembersOffTheWire(t *testing.T) {
	t.Parallel()
	canonical, err := CanonicalExecutionRequest(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"providers"`, `"publication"`} {
		if bytes.Contains(canonical, []byte(member)) {
			t.Errorf("canonical request carries %s when unset", member)
		}
	}
	request := validRequest()
	request.Invocation.Providers = []string{InvocationProviderInstall, InvocationProviderPublish}
	canonical, err = CanonicalExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExecutionRequest(canonical)
	if err != nil || !reflect.DeepEqual(parsed.Invocation.Providers, request.Invocation.Providers) {
		t.Fatalf("providers did not round-trip: %v, %v", parsed.Invocation.Providers, err)
	}
	if !KnownInvocationProvider(InvocationProviderInstall) || KnownInvocationProvider("deploy") {
		t.Error("KnownInvocationProvider drifted")
	}
}

// publishAfterBuild plans a publish task after /app:build and authorizes it
// with barrier.
func publishAfterBuild(r *ExecutionRequest, barrier ...string) {
	r.Invocation.Commands = append(r.Invocation.Commands, "publish")
	r.Invocation.Publication = &PublicationBlock{Barrier: barrier}
	r.Plan.Tasks = append(r.Plan.Tasks, PlannedTask{
		Identity: requestIdentity("/app", "publish", "publish"), DependsOn: []string{"/app:build"}, SerializeAfter: []string{},
		DeadlineMs: 1, Resources: TaskResources{CPUWeight: 1, Reads: []TaskResource{}, Writes: []TaskResource{}},
	})
	slices.SortFunc(r.Plan.Tasks, func(a, b PlannedTask) int { return strings.Compare(a.Identity.Key, b.Identity.Key) })
}

func publicationPlan(publishDependsOn ...string) PlanBlock {
	return PlanBlock{Tasks: []PlannedTask{
		{Identity: requestIdentity("/app", "publish", "publish"), DependsOn: publishDependsOn, SerializeAfter: []string{}, DeadlineMs: 1, Resources: TaskResources{CPUWeight: 1, Reads: []TaskResource{}, Writes: []TaskResource{}}},
		{Identity: requestIdentity("/app", "test", "test"), DependsOn: []string{"/lib:test"}, SerializeAfter: []string{}, DeadlineMs: 1, Resources: TaskResources{CPUWeight: 1, Reads: []TaskResource{}, Writes: []TaskResource{}}},
		{Identity: requestIdentity("/lib", "test", "test"), DependsOn: []string{}, SerializeAfter: []string{}, DeadlineMs: 1, Resources: TaskResources{CPUWeight: 1, Reads: []TaskResource{}, Writes: []TaskResource{}}},
	}}
}

// TestValidatePublicationRequiresTheBarrier pins the publication rules: a
// task publishes only under the block, and a publication task waits, through
// transitive edges, for every task of every barrier command.
func TestValidatePublicationRequiresTheBarrier(t *testing.T) {
	t.Parallel()
	invocation := InvocationBlock{Commands: []string{"test", "publish"}, Publication: &PublicationBlock{Barrier: []string{"test"}}}
	if err := ValidatePublication(invocation, publicationPlan("/app:test"), IsPublicationTask); err != nil {
		t.Fatalf("a publication waiting transitively for every barrier task was refused: %v", err)
	}
	if err := ValidatePublication(invocation, publicationPlan(), IsPublicationTask); err == nil || !strings.Contains(err.Error(), "does not wait") {
		t.Fatalf("a publication that waits for nothing was accepted: %v", err)
	}
	if err := ValidatePublication(InvocationBlock{Commands: invocation.Commands}, publicationPlan("/app:test"), IsPublicationTask); err == nil || !strings.Contains(err.Error(), "no invocation.publication") {
		t.Fatalf("a publication without a block was accepted: %v", err)
	}
	// An engine that classifies by declared traits may call a task of a
	// barrier command a publication; the barrier can never be one.
	byTrait := func(task PlannedTask) bool { return task.Identity.Task.Command != "publish" }
	if err := ValidatePublication(invocation, publicationPlan("/app:test"), byTrait); err == nil || !strings.Contains(err.Error(), "itself a publication") {
		t.Fatalf("a publishing barrier task was accepted: %v", err)
	}
	never := func(PlannedTask) bool { return false }
	if err := ValidatePublication(InvocationBlock{Commands: invocation.Commands}, publicationPlan(), never); err != nil {
		t.Fatalf("a plan the classifier calls side-effect free was refused: %v", err)
	}
	// The rule is one-way: a block over a plan in which the classifier finds
	// no publication task is accepted, because a command-name classifier
	// cannot see a registry write under another command. The executing
	// engine refuses a block over a plan without effects.
	if err := ValidatePublication(invocation, publicationPlan("/app:test"), never); err != nil {
		t.Fatalf("a publication block over a plan the classifier calls side-effect free was refused: %v", err)
	}
}

// TestCanonicalProvidersOmitsAnEmptyList pins the request form of
// invocation.providers: sorted and unique, and nil (an absent member) when
// nothing remains, so a request that enables no provider keeps its digest.
func TestCanonicalProvidersOmitsAnEmptyList(t *testing.T) {
	t.Parallel()
	for _, providers := range [][]string{nil, {}, {""}} {
		if got := CanonicalProviders(providers); got != nil {
			t.Errorf("CanonicalProviders(%q) = %q, want nil", providers, got)
		}
	}
	got := CanonicalProviders([]string{InvocationProviderPublish, InvocationProviderInstall, InvocationProviderPublish})
	if !slices.Equal(got, []string{InvocationProviderInstall, InvocationProviderPublish}) {
		t.Fatalf("CanonicalProviders = %q", got)
	}
	if err := validateProviders(got); err != nil {
		t.Fatalf("the canonical form is refused: %v", err)
	}
}
