package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func validCommitRequest() CommitRequest {
	return CommitRequest{
		Version:  CommitRequestVersion,
		Protocol: ProtocolBlock{Version: ProviderProtocolVersion, Capabilities: []string{CapabilityExecutionRequestV1, CapabilitySessionBundleV1}},
		Source:   CommitSource{Commit: strings.Repeat("a", 40), Base: strings.Repeat("d", 40)},
		Invocation: InvocationBlock{
			Commands: []string{"lint", "test", "build"},
			Params:   map[string]ParamValue{"coverage": {Type: ParamTypeBool, Value: true}},
			Flags:    ExecutionFlags{ImpactedStrict: true, CacheTrust: "ci", Output: "json", ResourceBudgets: map[string]int{}},
			Cwd:      ".",
		},
		Selection: RequestedSelection{Mode: SelectionModeImpacted},
		Control:   ControlBlock{Caller: CallerCI, IdempotencyKey: strings.Repeat("f", 32), Deadline: "2026-10-04T12:00:00Z"},
	}
}

func TestCommitRequestRoundTripsCanonically(t *testing.T) {
	t.Parallel()
	for name, request := range map[string]CommitRequest{
		"impacted": validCommitRequest(),
		"projects": func() CommitRequest {
			request := validCommitRequest()
			request.Source.Base = ""
			request.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"app", "libs/core"}}
			return request
		}(),
		"all": func() CommitRequest {
			request := validCommitRequest()
			request.Source.Base = ""
			request.Selection = RequestedSelection{Mode: SelectionModeAll}
			request.Control.Caller = CallerCLI
			return request
		}(),
	} {
		canonical, err := CanonicalCommitRequest(request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bound, err := ParseBoundRequest(canonical)
		if err != nil || bound.Snapshot != nil || bound.Commit == nil {
			t.Fatalf("%s: bound parse = %+v, %v", name, bound, err)
		}
		parsed, err := ParseCommitRequest(canonical)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(parsed, request) || !reflect.DeepEqual(*bound.Commit, request) {
			t.Fatalf("%s: round trip changed the request:\n%+v\n%+v", name, parsed, request)
		}
		if !reflect.DeepEqual(bound.Invocation(), request.Invocation) {
			t.Fatalf("%s: bound invocation = %+v", name, bound.Invocation())
		}
		again, err := CanonicalCommitRequest(parsed)
		if err != nil || !bytes.Equal(again, canonical) {
			t.Fatalf("%s: canonical bytes are not stable: %v", name, err)
		}
	}
}

func TestCommitInputDigestExcludesControlAndCapabilities(t *testing.T) {
	t.Parallel()
	base := validCommitRequest()
	digest, err := CommitInputDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	same := validCommitRequest()
	same.Control = ControlBlock{Caller: CallerCLI, IdempotencyKey: strings.Repeat("0", 32), Deadline: "2027-01-01T00:00:00Z"}
	same.Protocol.Capabilities = []string{CapabilityExecutionRequestV1}
	if got, err := CommitInputDigest(same); err != nil || got != digest {
		t.Fatalf("control and negotiated capabilities changed the input digest: %s vs %s (%v)", got, digest, err)
	}
	mutations := map[string]func(*CommitRequest){
		"commit":  func(r *CommitRequest) { r.Source.Commit = strings.Repeat("1", 40) },
		"base":    func(r *CommitRequest) { r.Source.Base = strings.Repeat("2", 40) },
		"command": func(r *CommitRequest) { r.Invocation.Commands = []string{"build"} },
		"param": func(r *CommitRequest) {
			r.Invocation.Params["coverage"] = ParamValue{Type: ParamTypeBool, Value: false}
		},
		"flag":        func(r *CommitRequest) { r.Invocation.Flags.ImpactedStrict = false },
		"cwd":         func(r *CommitRequest) { r.Invocation.Cwd = "app" },
		"providers":   func(r *CommitRequest) { r.Invocation.Providers = []string{InvocationProviderInstall} },
		"publication": func(r *CommitRequest) { r.Invocation.Publication = &PublicationBlock{Barrier: []string{"build"}} },
		"mode-all": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeAll}
		},
		"mode-projects": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"app"}}
		},
		"selectors": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"lib"}}
		},
	}
	seen := map[string]string{digest: "base"}
	for name, mutate := range mutations {
		request := validCommitRequest()
		mutate(&request)
		got, err := CommitInputDigest(request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if previous, collision := seen[got]; collision {
			t.Fatalf("%s did not change the input digest (equals %s)", name, previous)
		}
		seen[got] = name
	}
	// The domain separates the two versions even where their members would
	// marshal alike, so no version 1 input digest names a version 2 input.
	snapshot, err := ExecutionInputDigest(validRequest())
	if err != nil || snapshot == digest {
		t.Fatalf("version 1 and version 2 digests share a value: %s (%v)", snapshot, err)
	}
	if CommitInputDomain == ExecutionInputDomain {
		t.Fatal("version 1 and version 2 share one digest domain")
	}
}

func TestCommitRequestRejectsSemanticViolations(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*CommitRequest){
		"version":               func(r *CommitRequest) { r.Version = 1 },
		"protocol":              func(r *CommitRequest) { r.Protocol.Version = 2 },
		"unsorted caps":         func(r *CommitRequest) { r.Protocol.Capabilities = []string{"b", "a"} },
		"no commit":             func(r *CommitRequest) { r.Source.Commit = "" },
		"short commit":          func(r *CommitRequest) { r.Source.Commit = "abc1234" },
		"uppercase commit":      func(r *CommitRequest) { r.Source.Commit = strings.Repeat("A", 40) },
		"short base":            func(r *CommitRequest) { r.Source.Base = "abc1234" },
		"mixed object format":   func(r *CommitRequest) { r.Source.Base = strings.Repeat("d", 64) },
		"no commands":           func(r *CommitRequest) { r.Invocation.Commands = []string{} },
		"shell command":         func(r *CommitRequest) { r.Invocation.Commands = []string{"build; rm -rf /"} },
		"params nil":            func(r *CommitRequest) { r.Invocation.Params = nil },
		"budgets nil":           func(r *CommitRequest) { r.Invocation.Flags.ResourceBudgets = nil },
		"absolute cwd":          func(r *CommitRequest) { r.Invocation.Cwd = "/tmp" },
		"unknown provider":      func(r *CommitRequest) { r.Invocation.Providers = []string{"deploy"} },
		"unlisted barrier":      func(r *CommitRequest) { r.Invocation.Publication = &PublicationBlock{Barrier: []string{"qualify"}} },
		"mode":                  func(r *CommitRequest) { r.Selection.Mode = "some" },
		"impacted without base": func(r *CommitRequest) { r.Source.Base = "" },
		"base without impacted": func(r *CommitRequest) { r.Selection.Mode = SelectionModeAll },
		"projects without selectors": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects}
		},
		"empty selectors": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{}}
		},
		"projects outside projects mode": func(r *CommitRequest) { r.Selection.Projects = []string{"app"} },
		"empty list outside projects mode": func(r *CommitRequest) {
			r.Selection.Projects = []string{}
		},
		"unsorted selectors": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"lib", "app"}}
		},
		"repeated selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"app", "app"}}
		},
		"comma selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"app,lib"}}
		},
		"all selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"*"}}
		},
		"impacted selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{"[impacted]"}}
		},
		"padded selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{" app"}}
		},
		"empty selector": func(r *CommitRequest) {
			r.Source.Base = ""
			r.Selection = RequestedSelection{Mode: SelectionModeProjects, Projects: []string{""}}
		},
		"caller":   func(r *CommitRequest) { r.Control.Caller = "bot" },
		"key":      func(r *CommitRequest) { r.Control.IdempotencyKey = "abc" },
		"deadline": func(r *CommitRequest) { r.Control.Deadline = "2026-10-04T12:00:00+02:00" },
	}
	for name, mutate := range cases {
		request := validCommitRequest()
		mutate(&request)
		if err := ValidateCommitRequest(request); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := CommitInputDigest(request); err == nil {
			t.Errorf("%s: digested", name)
		}
	}
}

// A version 2 request reaches an engine with no CLI submitter in front of it,
// so the protocol refuses each command no portable run carries, wherever it
// sits in the list, and names it.
func TestCommitRequestRefusesUnportableCommands(t *testing.T) {
	t.Parallel()
	if !slices.IsSorted(UnportableCommands) || len(UnportableCommands) != 5 {
		t.Fatalf("UnportableCommands = %v; want the five sorted commands", UnportableCommands)
	}
	for _, command := range UnportableCommands {
		for _, commands := range [][]string{{command}, {"build", command}} {
			request := validCommitRequest()
			request.Invocation.Commands = commands
			want := fmt.Sprintf("invocation.commands names %q, which no portable run carries", command)
			if err := ValidateCommitRequest(request); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%v: %v; want a refusal naming %q", commands, err, command)
			}
			if _, err := CommitInputDigest(request); err == nil {
				t.Errorf("%v: digested", commands)
			}
		}
	}
}

// TestBoundRequestRefusesEveryVersionMix pins the precise refusal of each
// member one version carries and the other does not, so a caller learns which
// version it mixed in rather than meeting a bare unknown field.
func TestBoundRequestRefusesEveryVersionMix(t *testing.T) {
	t.Parallel()
	commit := func(edit func(map[string]any)) []byte {
		return editedRequest(t, mustCanonicalCommit(t, validCommitRequest()), edit)
	}
	snapshot := func(edit func(map[string]any)) []byte {
		canonical, err := CanonicalExecutionRequest(validRequest())
		if err != nil {
			t.Fatal(err)
		}
		return editedRequest(t, canonical, edit)
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"v2 with a plan": {commit(func(root map[string]any) { root["plan"] = map[string]any{"tasks": []any{}} }), "carries no plan"},
		"v2 with environment": {commit(func(root map[string]any) {
			root["environment"] = map[string]any{}
		}), "carries no environment"},
		"v2 with a plan and environment": {commit(func(root map[string]any) {
			root["plan"] = map[string]any{"tasks": []any{}}
			root["environment"] = map[string]any{}
		}), "carries no plan"},
		"v1 with a commit source": {snapshot(func(root map[string]any) {
			root["source"].(map[string]any)["commit"] = strings.Repeat("a", 40)
		}), "source.commit addresses a commit"},
		"v1 with a commit base": {snapshot(func(root map[string]any) {
			root["source"].(map[string]any)["base"] = strings.Repeat("a", 40)
		}), "source.base addresses a commit"},
		"v1 with caller ci": {snapshot(func(root map[string]any) {
			root["control"].(map[string]any)["caller"] = CallerCI
		}), "calls a version 2 request only"},
		"unsupported version": {commit(func(root map[string]any) { root["version"] = 3 }), "unsupported execution request version 3"},
		"missing version":     {commit(func(root map[string]any) { delete(root, "version") }), "missing field \"version\""},
		"string version":      {commit(func(root map[string]any) { root["version"] = "2" }), "version must be an integer"},
		"fractional version":  {commit(func(root map[string]any) { root["version"] = 2.5 }), "version must be an integer"},
	}
	for _, member := range snapshotSourceMembers {
		cases["v2 with source."+member] = struct {
			data []byte
			want string
		}{commit(func(root map[string]any) { root["source"].(map[string]any)[member] = "x" }), "source." + member + " addresses a snapshot"}
	}
	for _, member := range frozenSelectionMembers {
		cases["v2 with selection."+member] = struct {
			data []byte
			want string
		}{commit(func(root map[string]any) { root["selection"].(map[string]any)[member] = "x" }), "selection." + member + " is a frozen selection output"}
	}
	for name, test := range cases {
		_, err := ParseBoundRequest(test.data)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: refusal %v does not name %q", name, err, test.want)
		}
	}
	if _, err := ParseBoundRequest(commit(func(root map[string]any) { root["plan"] = map[string]any{} })); !errors.Is(err, ErrFrozenPlan) {
		t.Errorf("an empty plan is not refused as a frozen plan: %v", err)
	}
	if _, err := ParseCommitRequest(mustCanonicalSnapshot(t)); err == nil || !strings.Contains(err.Error(), "has version 2, not 1") {
		t.Errorf("the version 2 parser read a version 1 request: %v", err)
	}
	if _, err := ParseExecutionRequest(mustCanonicalCommit(t, validCommitRequest())); err == nil {
		t.Error("the version 1 parser read a version 2 request")
	}
	oversized := append(bytes.Repeat([]byte(" "), MaxRequestBytes), '{', '}')
	if _, err := ParseBoundRequest(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized bound request: %v", err)
	}
}

func TestCommitRequestSchemaTracksWireShape(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("schemas", "execution-request-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		AdditionalProperties bool                       `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	var schema object
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || strings.Join(schema.Required, ",") != "version,protocol,source,invocation,selection,control" {
		t.Fatalf("schema root diverged: %v", schema.Required)
	}
	var version struct {
		Const int `json:"const"`
	}
	if err := json.Unmarshal(schema.Properties["version"], &version); err != nil || version.Const != CommitRequestVersion {
		t.Fatalf("schema version diverged: %d (%v)", version.Const, err)
	}
	var source, selection, control object
	for raw, target := range map[string]*object{"source": &source, "selection": &selection, "control": &control} {
		if err := json.Unmarshal(schema.Properties[raw], target); err != nil || target.AdditionalProperties {
			t.Fatalf("schema %s is not strict (%v)", raw, err)
		}
	}
	if strings.Join(source.Required, ",") != "commit" || len(source.Properties) != 2 || source.Properties["base"] == nil {
		t.Fatalf("schema source diverged: %v", source.Required)
	}
	if strings.Join(selection.Required, ",") != "mode" || len(selection.Properties) != 2 || selection.Properties["projects"] == nil {
		t.Fatalf("schema selection diverged: %v", selection.Required)
	}
	var mode, caller struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(selection.Properties["mode"], &mode); err != nil || strings.Join(mode.Enum, ",") != strings.Join([]string{SelectionModeAll, SelectionModeImpacted, SelectionModeProjects}, ",") {
		t.Fatalf("schema selection modes diverged: %v (%v)", mode.Enum, err)
	}
	if err := json.Unmarshal(control.Properties["caller"], &caller); err != nil || strings.Join(caller.Enum, ",") != CallerCLI+","+CallerCI {
		t.Fatalf("schema callers diverged: %v (%v)", caller.Enum, err)
	}
	// The unportable commands sit beside the shared invocation block, not in it.
	var unportable struct {
		AllOf []struct {
			Properties struct {
				Invocation struct {
					Properties struct {
						Commands struct {
							Items struct {
								Not struct {
									Enum []string `json:"enum"`
								} `json:"not"`
							} `json:"items"`
						} `json:"commands"`
					} `json:"properties"`
				} `json:"invocation"`
			} `json:"properties"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(data, &unportable); err != nil || len(unportable.AllOf) != 1 ||
		!slices.Equal(unportable.AllOf[0].Properties.Invocation.Properties.Commands.Items.Not.Enum, UnportableCommands) {
		t.Fatalf("schema unportable commands diverged from UnportableCommands (%v)", err)
	}
	// Both versions share one invocation block, so its schema is the version 1 one.
	v1, err := os.ReadFile(filepath.Join("schemas", "execution-request-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v1Schema object
	if err := json.Unmarshal(v1, &v1Schema); err != nil {
		t.Fatal(err)
	}
	for _, block := range []string{"protocol", "invocation"} {
		var got, want any
		if err := json.Unmarshal(schema.Properties[block], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(v1Schema.Properties[block], &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("schema %s block diverged from version 1", block)
		}
	}
}

func mustCanonicalCommit(t *testing.T, request CommitRequest) []byte {
	t.Helper()
	data, err := CanonicalCommitRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustCanonicalSnapshot(t *testing.T) []byte {
	t.Helper()
	data, err := CanonicalExecutionRequest(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// editedRequest decodes a canonical request into generic JSON, applies edit
// and encodes it again.
func editedRequest(t *testing.T, canonical []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(canonical, &root); err != nil {
		t.Fatal(err)
	}
	edit(root)
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
