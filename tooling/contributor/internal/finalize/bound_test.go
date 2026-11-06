package finalize

import (
	"encoding/json"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"

	"go.putnami.dev/protocol/features/spectest"
)

// boundCapabilities is a bound v1 binding of provider with settings.
func boundCapabilities(provider string, settings map[string]string) collab.Capabilities {
	raw, _ := json.Marshal(settings)
	return collab.Capabilities{
		Status:   collab.BindingStatusBound,
		Provider: &collab.ProviderIdentity{Name: provider},
		Binding:  &collab.Binding{Provider: provider, Version: 1, Settings: raw},
	}
}

// TestTheBoundProviderProvesTheClosingSource pins that an id-only closing
// reference rests on the bindings alone, whatever provider serves them: the
// proposals binding names the provider that issued the task and its own
// settings locate the task's source.
func TestTheBoundProviderProvesTheClosingSource(t *testing.T) {
	spectest.Proves(t, "tooling/contributor-executable", "provider-neutral-publication", "the-bound-provider-proves-the-closing-source")
	const forge = "@acme/forge-collaboration"
	cases := []struct {
		name      string
		source    string
		tasks     string
		proposals string
		settings  map[string]string
		accepted  bool
		// taskSettings are the tasks binding's settings; nil means the
		// proposals binding's.
		taskSettings map[string]string
	}{
		{"github default host", "github:acme/app", forge, forge, map[string]string{"repository": "Acme/App"}, true, nil},
		{"github default host spelled out", "github:acme/app", forge, forge, map[string]string{"repository": "acme/app", "host": "GitHub.com"}, true, nil},
		{"github enterprise", "github:ghe.example.com/acme/app", forge, forge, map[string]string{"repository": "acme/app", "host": "ghe.example.com"}, true, nil},
		{"gitlab.com nested group", "gitlab:gitlab.com/acme/platform/app", forge, forge, map[string]string{"project": "acme/platform/app"}, true, nil},
		{"gitlab self-managed", "gitlab:git.example.com/acme/app", forge, forge, map[string]string{"project": "acme/app", "host": "git.example.com"}, true, nil},
		{"another repository", "github:acme/app", forge, forge, map[string]string{"repository": "acme/other"}, false, nil},
		{"another host", "gitlab:gitlab.com/acme/app", forge, forge, map[string]string{"project": "acme/app", "host": "git.example.com"}, false, nil},
		{"self-managed host without its name", "github:acme/app", forge, forge, map[string]string{"repository": "acme/app", "host": "ghe.example.com"}, false, nil},
		{"another provider serves the tasks", "github:acme/app", "@acme/tracker-collaboration", forge, map[string]string{"repository": "acme/app"}, false, nil},
		{"both repository and project", "gitlab:gitlab.com/acme/app", forge, forge, map[string]string{"repository": "acme/app", "project": "acme/app"}, false, nil},
		{"no location", "github:acme/app", forge, forge, map[string]string{}, false, nil},
		{"dot segment", "gitlab:gitlab.com/acme/../app", forge, forge, map[string]string{"project": "acme/../app"}, false, nil},
		{"a host with a path", "gitlab:evil.example/x/acme/app", forge, forge, map[string]string{"project": "acme/app", "host": "evil.example/x"}, false, nil},
		{"not a code-host source", "jira:acme/app", forge, forge, map[string]string{"repository": "acme/app"}, false, nil},
		{"generic has no default host", "generic:acme/app", forge, forge, map[string]string{"repository": "acme/app"}, false, nil},
		{"tasks on github.com spelled out", "github:acme/app", forge, forge, map[string]string{"repository": "acme/app"}, true,
			map[string]string{"repository": "ACME/app", "host": "github.com", "states": "kept"}},
		{"a project path that spells another host", "gitlab:git.example.com/acme/app", forge, forge,
			map[string]string{"project": "git.example.com/acme/app"}, false, map[string]string{"project": "acme/app", "host": "git.example.com"}},
		{"the tasks binding serves another repository", "github:acme/app", forge, forge, map[string]string{"repository": "acme/app"}, false,
			map[string]string{"repository": "acme/other"}},
		{"the tasks binding has no location", "github:acme/app", forge, forge, map[string]string{"repository": "acme/app"}, false,
			map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy := Policy{}
			policy.Proposals.Source = c.source
			policy.Publication.TaskReference = "Closes #{id}"
			task := collab.Task{Ref: collab.Ref{Source: c.source, ID: "42"}}
			taskSettings := c.taskSettings
			if taskSettings == nil {
				taskSettings = c.settings
			}
			line, err := BoundTaskReference(policy, task, false, boundCapabilities(c.proposals, c.settings), boundCapabilities(c.tasks, taskSettings))
			if c.accepted && (err != nil || line != "Closes #42") {
				t.Fatalf("a binding that locates %s was refused: %q %v", c.source, line, err)
			}
			if !c.accepted && (err == nil || !strings.Contains(err.Error(), "use {url}")) {
				t.Fatalf("a binding that does not locate %s rendered %q: %v", c.source, line, err)
			}
		})
	}
	unbound := boundCapabilities(forge, map[string]string{"repository": "acme/app"})
	unbound.Status = collab.BindingStatusUnbound
	policy := Policy{}
	policy.Proposals.Source = "github:acme/app"
	policy.Publication.TaskReference = "Closes #{id}"
	task := collab.Task{Ref: collab.Ref{Source: "github:acme/app", ID: "42"}, URL: "https://example.test/42"}
	located := boundCapabilities(forge, map[string]string{"repository": "acme/app"})
	if _, err := BoundTaskReference(policy, task, false, unbound, located); err == nil {
		t.Fatal("an unbound proposals binding proved the closing source")
	}
	if _, err := BoundTaskReference(policy, task, false, located, unbound); err == nil {
		t.Fatal("an unbound tasks binding proved the closing source")
	}
	if _, err := BoundTaskReference(policy, task, false, located, collab.Capabilities{Status: collab.BindingStatusBound}); err == nil {
		t.Fatal("a tasks binding without provider proved the closing source")
	}
	if line, err := BoundTaskReference(policy, task, true, unbound, unbound); err != nil || line != "Refs "+task.URL {
		t.Fatalf("partial delivery needs no binding proof: %q %v", line, err)
	}
}
