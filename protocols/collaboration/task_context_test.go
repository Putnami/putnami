package collaboration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTaskContextKeepsAbsentApartFromEmpty: a task without context members
// says the provider does not report them, and an empty list or a zero count
// says there is none. Both survive a parse and a re-encoding, which is what
// the orchestrator does to every result it forwards.
func TestTaskContextKeepsAbsentApartFromEmpty(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-context", "a-task-keeps-its-context-members-through-a-re-encoding")
	const task = `"ref":{"source":"local:x","id":"T-1"},"revision":"r1","title":"t","state":"open"`
	for name, tc := range map[string]struct {
		members string
		want    []string
		absent  []string
	}{
		"not reported": {absent: []string{`"parentProgress"`, `"blockedBy"`, `"blocking"`, `"comments"`}},
		"none": {
			members: `,"blockedBy":[],"blocking":[],"comments":{"count":0}`,
			want:    []string{`"blockedBy":[]`, `"blocking":[]`, `"comments":{"count":0}`},
			absent:  []string{`"parentProgress"`},
		},
		"some": {
			members: `,"parent":{"source":"local:x","id":"T-0"},"parentProgress":{"title":"epic","done":1,"total":2}` +
				`,"blockedBy":[{"ref":{"source":"local:x","id":"T-2"},"state":"done"}]` +
				`,"comments":{"count":2,"lastAuthor":"octocat","lastAt":"2026-11-16T10:00:00Z"}`,
			want: []string{
				`"parentProgress":{"title":"epic","done":1,"total":2}`,
				`"blockedBy":[{"ref":{"source":"local:x","id":"T-2"},"state":"done"}]`,
				`"comments":{"count":2,"lastAuthor":"octocat","lastAt":"2026-11-16T10:00:00Z"}`,
			},
			absent: []string{`"blocking"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, diags := ParseResult(ContractTasks, 1, OperationGet, []byte(`{"task":{`+task+tc.members+`}}`))
			if diags != nil {
				t.Fatalf("the result does not parse: %v", diags)
			}
			encoded, err := json.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range tc.want {
				if !strings.Contains(string(encoded), member) {
					t.Errorf("%s lost %s", encoded, member)
				}
			}
			for _, member := range tc.absent {
				if strings.Contains(string(encoded), member) {
					t.Errorf("%s gained %s", encoded, member)
				}
			}
		})
	}
}

// TestInvalidTaskContextIsRefused names each refusal of a context member,
// and that only tasks.get may report one.
func TestInvalidTaskContextIsRefused(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-context", "invalid-context-members-are-refused")
	const task = `"ref":{"source":"local:x","id":"T-1"},"revision":"r1","title":"t","state":"open"`
	const parent = `,"parent":{"source":"local:x","id":"T-0"}`
	for name, tc := range map[string]struct {
		operation string
		document  string
		field     string
	}{
		"progress without parent":    {OperationGet, `{"task":{` + task + `,"parentProgress":{"title":"e","done":0,"total":1}}}`, "task.parentProgress"},
		"progress without title":     {OperationGet, `{"task":{` + task + parent + `,"parentProgress":{"title":"","done":0,"total":1}}}`, "task.parentProgress.title"},
		"negative done":              {OperationGet, `{"task":{` + task + parent + `,"parentProgress":{"title":"e","done":-1,"total":1}}}`, "task.parentProgress.done"},
		"negative total":             {OperationGet, `{"task":{` + task + parent + `,"parentProgress":{"title":"e","done":0,"total":-1}}}`, "task.parentProgress.total"},
		"done above total":           {OperationGet, `{"task":{` + task + parent + `,"parentProgress":{"title":"e","done":2,"total":1}}}`, "task.parentProgress.done"},
		"linked without ref":         {OperationGet, `{"task":{` + task + `,"blockedBy":[{"state":"open"}]}}`, "task.blockedBy[0].ref.source"},
		"linked without state":       {OperationGet, `{"task":{` + task + `,"blocking":[{"ref":{"source":"local:x","id":"T-2"}}]}}`, "task.blocking[0].state"},
		"linked unknown state":       {OperationGet, `{"task":{` + task + `,"blocking":[{"ref":{"source":"local:x","id":"T-2"},"state":"later"}]}}`, "task.blocking[0].state"},
		"negative comment count":     {OperationGet, `{"task":{` + task + `,"comments":{"count":-1}}}`, "task.comments.count"},
		"last comment on zero":       {OperationGet, `{"task":{` + task + `,"comments":{"count":0,"lastAt":"2026-11-16T10:00:00Z"}}}`, "task.comments"},
		"comment time":               {OperationGet, `{"task":{` + task + `,"comments":{"count":1,"lastAt":"yesterday"}}}`, "task.comments.lastAt"},
		"find reports context":       {OperationFind, `{"items":[{` + task + `,"comments":{"count":0}}],"page":{}}`, "items[0].comments"},
		"create reports context":     {OperationCreate, `{"task":{` + task + `,"blocking":[]},"created":true}`, "task.blocking"},
		"update reports context":     {OperationUpdate, `{"task":{` + task + `,"comments":{"count":0}}}`, "task.comments"},
		"transition reports context": {OperationTransition, `{"task":{` + task + `,"blockedBy":[]}}`, "task.blockedBy"},
		"assign reports context":     {OperationAssign, `{"task":{` + task + `,"blocking":[]}}`, "task.blocking"},
		"link reports context":       {OperationLink, `{"task":{` + task + parent + `,"parentProgress":{"title":"e","done":0,"total":1}}}`, "task.parentProgress"},
		"no child under the parent":  {OperationGet, `{"task":{` + task + parent + `,"parentProgress":{"title":"e","done":0,"total":0}}}`, "task.parentProgress.total"},
		"linked to itself":           {OperationGet, `{"task":{` + task + `,"blockedBy":[{"ref":{"source":"local:x","id":"T-1"},"state":"open"}]}}`, "task.blockedBy[0].ref"},
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseResult(ContractTasks, 1, tc.operation, []byte(tc.document))
			for _, d := range diags {
				if d.Field == tc.field {
					return
				}
			}
			t.Fatalf("diagnostics %v name no %s", diags, tc.field)
		})
	}
}

// TestOnlyTasksGetDeclaresTheContextMembers: the catalog lists the context
// members on tasks.get, in canonical order, and no other operation of any
// contract lists a member.
func TestOnlyTasksGetDeclaresTheContextMembers(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-task-context", "only-tasks-get-declares-the-context-members")
	if !slices.IsSorted(TaskContextMembers) {
		t.Errorf("TaskContextMembers %v is not in canonical order", TaskContextMembers)
	}
	for _, spec := range Catalog() {
		for _, op := range spec.Operations {
			if spec.Name == ContractTasks && op.Name == OperationGet {
				if !slices.Equal(op.Members, TaskContextMembers) {
					t.Errorf("tasks.get members = %v, want %v", op.Members, TaskContextMembers)
				}
				continue
			}
			if len(op.Members) > 0 {
				t.Errorf("%s.%s lists members %v", spec.Name, op.Name, op.Members)
			}
		}
	}
}

// TestReadProviderOffer_DeclaredMembers: a provider offers the members the
// catalog lists for the operation, and any other member withdraws the
// contract version.
func TestReadProviderOffer_DeclaredMembers(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "declared-task-context", "an-undeclared-member-withdraws-the-contract-version")
	spectest.Proves(t, "collaboration/provider-contract", "truthful-provider-declarations", "any-declaration-error-withdraws-the-contract-version")
	declare := func(operation string, members ...string) map[string]extension.ProviderDeclaration {
		tasks := fullProvider(ContractTasks)
		declared := tasks.Operations[operation]
		declared.Members = members
		tasks.Operations[operation] = declared
		return map[string]extension.ProviderDeclaration{ContractTasks: tasks}
	}

	offer, diags := ReadProviderOffer(declare(OperationGet, TaskMemberBlockedBy, TaskMemberComments))
	if diag.HasErrors(diags) {
		t.Fatalf("declared members were refused: %v", diags)
	}
	ops, _ := offer.Operations(ContractTasks, 1)
	if got := ops[OperationGet].Members(); !slices.Equal(got, []string{TaskMemberBlockedBy, TaskMemberComments}) {
		t.Errorf("offered members = %v, want [blockedBy comments]", got)
	}
	if got := ops[OperationFind].Members(); got != nil {
		t.Errorf("an operation without members offers %v", got)
	}

	for name, providers := range map[string]map[string]extension.ProviderDeclaration{
		"unknown member":           declare(OperationGet, "labelsHistory"),
		"member of another result": declare(OperationFind, TaskMemberComments),
	} {
		t.Run(name, func(t *testing.T) {
			offer, diags := ReadProviderOffer(providers)
			if len(offer.Versions(ContractTasks)) != 0 {
				t.Fatal("a declaration with an unlisted member is still offered")
			}
			errs := diag.Errors(diags)
			if len(errs) != 1 || errs[0].Code != ErrorCodeInvalidDeclaration || !strings.HasSuffix(errs[0].Field, ".members[0]") {
				t.Fatalf("diagnostics = %v, want one %s at members[0]", errs, ErrorCodeInvalidDeclaration)
			}
		})
	}
}
