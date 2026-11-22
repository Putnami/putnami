package pkg

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestLocalAliasUsesAFullReferenceAsGiven(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "image-tag-and-port", "a-docker-tag-with-a-repository-is-the-whole-reference")
	cases := []struct {
		tag  string
		want string
	}{
		{tag: "v1.0.0", want: "tasks-api:v1.0.0"},
		{tag: "local", want: "tasks-api:local"},
		{tag: "tasks-api:local", want: "tasks-api:local"},
		{tag: "other:dev", want: "other:dev"},
		{tag: "registry.example.com/team/api", want: "registry.example.com/team/api"},
		{tag: "localhost:5000/team/api:1.0", want: "localhost:5000/team/api:1.0"},
	}
	for _, tc := range cases {
		if got := localAlias("tasks-api", tc.tag); got != tc.want {
			t.Errorf("localAlias(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
}

func TestImagePortOrder(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "image-tag-and-port", "the-image-port-is-the-project-port")
	serve := func(raw string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"serve": json.RawMessage(raw)}
	}
	cases := []struct {
		name        string
		param       string
		options     map[string]json.RawMessage
		want        string
		wantSkipped []string
	}{
		{name: "nothing configured", want: "8080"},
		{name: "flag or options.package.port wins", param: "9000", options: serve(`{"port": 8081}`), want: "9000"},
		{name: "serve port as a number", options: serve(`{"port": 8081}`), want: "8081"},
		{name: "serve port as a string", options: serve(`{"port": "8082"}`), want: "8082"},
		{name: "serve block without a port", options: serve(`{"watch": false}`), want: "8080"},
		{name: "other blocks are ignored", options: map[string]json.RawMessage{"package": json.RawMessage(`{"port": 1}`)}, want: "8080"},
		{
			name: "an invalid flag falls through to serve", param: "abc", options: serve(`{"port": 8081}`),
			want: "8081", wantSkipped: []string{`port "abc"`},
		},
		{
			name: "out-of-range values fall through to 8080", param: "70000", options: serve(`{"port": 0}`),
			want: "8080", wantSkipped: []string{`port "70000"`, "options.serve.port 0"},
		},
		{name: "a fractional serve port is skipped", options: serve(`{"port": 80.5}`), want: "8080", wantSkipped: []string{"options.serve.port 80.5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, skipped := imagePort(tc.param, tc.options)
			if got != tc.want {
				t.Fatalf("imagePort = %q, want %q", got, tc.want)
			}
			if !slices.Equal(skipped, tc.wantSkipped) {
				t.Fatalf("skipped = %q, want %q", skipped, tc.wantSkipped)
			}
		})
	}
}

func TestTheImageSetsAndExposesItsPort(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "image-tag-and-port", "the-image-port-is-the-project-port")
	spec := dockerSpec("base@sha256:"+strings.Repeat("0", 64), "linux/amd64", "/bin/app", "8081")
	if !slices.Equal(spec.Env, []string{"PORT=8081"}) || !slices.Equal(spec.ExposedPorts, []string{"8081/tcp"}) {
		t.Fatalf("spec env %q, exposed %q; want PORT=8081 and 8081/tcp", spec.Env, spec.ExposedPorts)
	}
	for _, builder := range []string{"", "ghcr.io/acme/builder:1"} {
		got := dockerfileFor(builder, "bin/app", "8081")
		if !strings.Contains(got, "ENV PORT=8081\nEXPOSE 8081\n") || !strings.Contains(got, "bin/app /app") {
			t.Fatalf("dockerfileFor(%q) = %q, want PORT and EXPOSE 8081 and the binary copied", builder, got)
		}
	}
}

func TestASkippedImagePortIsWarned(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "image-tag-and-port", "the-image-port-is-the-project-port")
	var port string
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		port = resolveImagePort(emit, "abc", map[string]json.RawMessage{"serve": json.RawMessage(`{"port": 9001}`)})
	})
	if port != "9001" {
		t.Fatalf("port = %q, want the serve port 9001", port)
	}
	var warnings []string
	for _, event := range events {
		if event["severity"] == "warning" {
			message, _ := event["message"].(string)
			warnings = append(warnings, message)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `port "abc"`) {
		t.Fatalf("warnings = %q, want one naming port \"abc\"; events %v", warnings, events)
	}
}
