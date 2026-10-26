package extension

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// publishTask is a registry publication that declares the channels it serves.
func publishTask(channels ...string) TaskDefinition {
	return TaskDefinition{
		Kind:     "command",
		Command:  "echo",
		Cache:    &TaskCachePolicy{Enabled: boolPtr(false)},
		Declares: &TaskDeclaration{Effects: []string{EffectNetwork, EffectRegistry}, PublishChannels: channels},
	}
}

// A reader before rung 12 judges the channels of a publishing run by another
// rule than the tasks that declare them, so a manifest with a task that
// declares publishChannels needs rung 12, and a lower stamp is refused with
// the reason.
func TestAPublishChannelDeclarationRequiresThePublishChannelsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	manifest := []byte(`{"name":"@test/npm","cliContract":11,"commands":{"publish":{"run":[{"id":"npm","task":"publish-npm"}]}},"tasks":{"publish-npm":{"kind":"command","command":"echo","cache":false,"declares":{"effects":["network","registry"],"publishChannels":["npm"]}}}}`)
	_, err := NegotiateManifest("npm.json", manifest)
	if err == nil {
		t.Fatal("contract-11 stamp accepted a task that declares publish channels")
	}
	for _, fragment := range []string{"requires 12 for its publish channels", "re-package the extension"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal %q does not name %q", err, fragment)
		}
	}

	manifest = []byte(strings.Replace(string(manifest), `"cliContract":11`, `"cliContract":12`, 1))
	m, err := NegotiateManifest("npm.json", manifest)
	if err != nil {
		t.Fatalf("contract-12 stamp refused: %v", err)
	}
	if got := m.Tasks["publish-npm"].Declares.PublishChannels; !reflect.DeepEqual(got, []string{"npm"}) {
		t.Fatalf("publishChannels = %v, want [npm]", got)
	}
	if got := RequiredCLIContract(m); got != protocolcli.PublishChannelsContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.PublishChannelsContract)
	}
	if got := RequiredLoadContract(m); got != protocolcli.PublishChannelsContract {
		t.Fatalf("RequiredLoadContract = %d, want %d: the rung binds every origin", got, protocolcli.PublishChannelsContract)
	}
}

// Every task counts, and a task whose declaration lists no channel leaves the
// floor where its other vocabulary puts it.
func TestRequiredCLIContractRaisesToPublishChannelsOnlyForADeclaringTask(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	serving := publishTask("docker")
	silent := publishTask()
	bound := TaskDefinition{Kind: "command", Command: "echo", Cache: &TaskCachePolicy{SourceBound: true}}

	for _, test := range []struct {
		name  string
		tasks map[string]TaskDefinition
		want  int
	}{
		{"a registry task without channels", map[string]TaskDefinition{"a": silent}, protocolcli.CurrentContract},
		{"beside a source-bound rung", map[string]TaskDefinition{"a": bound, "b": serving}, protocolcli.PublishChannelsContract},
		{"source-bound only", map[string]TaskDefinition{"a": bound, "b": silent}, protocolcli.SourceBoundCacheContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Map iteration order is random; repeat so an early return on a
			// lower rung would show.
			for range 32 {
				if got := RequiredCLIContract(&Manifest{Tasks: test.tasks}); got != test.want {
					t.Fatalf("RequiredCLIContract = %d, want %d", got, test.want)
				}
			}
		})
	}
}

// A declared list names at least one channel and each channel once, and a task
// that serves a channel publishes to a registry. Each rule has its own code.
func TestValidatePublishChannels(t *testing.T) {
	noRegistry := publishTask("npm")
	noRegistry.Declares.Effects = []string{EffectNetwork}

	for _, test := range []struct {
		name  string
		task  TaskDefinition
		codes []string
	}{
		{"one channel", publishTask("npm"), nil},
		{"several channels", publishTask("archives", "template-archives"), nil},
		{"the grammar of a project's publish list", publishTask("Site Content", "registry.example/ns"), nil},
		{"no channel field", publishTask(), nil},
		{"an empty list", publishTask([]string{}...), []string{"invalid-value"}},
		{"a duplicate", publishTask("npm", "docker", "npm"), []string{"duplicate-publish-channel"}},
		{"no registry effect", noRegistry, []string{"effect-conflict"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := diagCodes(validateTaskDeclaration("tasks.t", test.task)); !slices.Equal(got, test.codes) {
				t.Fatalf("codes = %v, want %v", got, test.codes)
			}
		})
	}
}

// The channels are a set: normalization sorts them, so two manifests that list
// the same channels in another order digest identically.
func TestPublishChannelsNormalizeToSortedOrder(t *testing.T) {
	authored := publishTask("template-archives", "archives")
	sorted := publishTask("archives", "template-archives")
	if TaskContractDigest(authored) != TaskContractDigest(sorted) {
		t.Fatal("the order of publishChannels moved the task contract digest")
	}
	m := &Manifest{Tasks: map[string]TaskDefinition{"publish": authored}}
	NormalizeTaskDeclarations(m)
	if got := m.Tasks["publish"].Declares.PublishChannels; !slices.IsSorted(got) {
		t.Fatalf("normalized publishChannels = %v, want sorted", got)
	}
}

// The field is omitted when absent, so the declaration of every task that
// serves no channel keeps its JSON, its contract digest and its cache key.
func TestPublishChannelsAreOmittedWhenAbsent(t *testing.T) {
	data, err := json.Marshal(publishTask().Declares)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "publishChannels") {
		t.Fatalf("an absent publishChannels is serialized: %s", data)
	}
}
