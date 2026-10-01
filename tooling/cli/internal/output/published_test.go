package output

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// ref renders the test fixture's artifact coordinate. Production consumes the
// structured fields directly; this compact spelling exists only for assertions.
func (a publishedArtifact) ref() string {
	if a.version == "" {
		return a.name
	}
	sep := "@"
	if a.registry == "docker" {
		sep = ":"
	}
	return a.name + sep + a.version
}

// publishedEvent builds an artifact event in the post-parse shape emitted by
// publish jobs (top-level name/kind merged into Data alongside the payload).
func publishedEvent(registry, name, version string, tags []any, dryRun bool) jobs.RawJobEvent {
	data := map[string]any{
		"name":     name,
		"kind":     "published",
		"registry": registry,
		"version":  version,
	}
	if tags != nil {
		data["tags"] = tags
	}
	if dryRun {
		data["dryRun"] = true
	}
	return jobs.RawJobEvent{Type: jobs.EventTypeArtifact, Data: data}
}

func TestCollectPublished(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"cli:publish~npm": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("npm", "@putnami/cli", "0.1.0-abc", []any{"latest"}, false),
		}},
		"app:publish~go": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("go", "go.putnami.dev/app", "v0.1.0-abc", nil, false),
		}},
		"app:build": {Status: "success", Events: []jobs.RawJobEvent{
			{Type: jobs.EventTypeArtifact, Data: map[string]any{"name": "bundle", "kind": "archive"}},
		}},
	}

	got := collectPublished(results)
	if len(got) != 2 {
		t.Fatalf("expected 2 published artifacts, got %d: %+v", len(got), got)
	}
	// Sorted by registry: "go" before "npm".
	if got[0].registry != "go" || got[0].ref() != "go.putnami.dev/app@v0.1.0-abc" {
		t.Errorf("unexpected first artifact: %+v", got[0])
	}
	if got[1].registry != "npm" || got[1].ref() != "@putnami/cli@0.1.0-abc" {
		t.Errorf("unexpected second artifact: %+v", got[1])
	}
	if len(got[1].tags) != 1 || got[1].tags[0] != "latest" {
		t.Errorf("expected npm tags [latest], got %v", got[1].tags)
	}
}

func TestCollectPublishedDedup(t *testing.T) {
	ev := publishedEvent("npm", "@putnami/cli", "0.1.0", []any{"latest"}, false)
	results := map[string]*jobs.JobResult{
		"a": {Events: []jobs.RawJobEvent{ev}},
		"b": {Events: []jobs.RawJobEvent{ev}},
	}
	if got := collectPublished(results); len(got) != 1 {
		t.Fatalf("expected dedup to 1 artifact, got %d", len(got))
	}
}

func TestCollectPublishedIgnoresNonPublished(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"a": {Events: []jobs.RawJobEvent{
			{Type: jobs.EventTypeArtifact, Data: map[string]any{"name": "cov", "kind": "coverage"}},
			{Type: jobs.EventTypeSummary, Data: map[string]any{"message": "done"}},
		}},
	}
	if got := collectPublished(results); len(got) != 0 {
		t.Fatalf("expected 0 published artifacts, got %d", len(got))
	}
}

func TestTextRendererFinishPublished(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start(nil)

	results := map[string]*jobs.JobResult{
		"cli:publish~npm": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("npm", "@putnami/cli", "0.1.0-abc", []any{"latest", "next"}, false),
		}},
		"svc:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("docker", "ghcr.io/x/svc", "0.1.0-abc", nil, true),
		}},
	}
	r.Finish(results, jobs.SessionOutcome{})

	got := errOut.String()
	for _, want := range []string{
		"Published:",
		"Version: 0.1.0-abc",
		"2 artifacts",
		"1 dry run",
		"docker 1",
		"npm 1",
		"latest (1)",
		"next (1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"@putnami/cli@0.1.0-abc", "ghcr.io/x/svc:0.1.0-abc"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("did not expect per-artifact ref %q in compact summary, got:\n%s", unwanted, got)
		}
	}
}

func TestPublishedArtifactRef(t *testing.T) {
	tests := []struct {
		name string
		a    publishedArtifact
		want string
	}{
		{"npm", publishedArtifact{registry: "npm", name: "@putnami/cli", version: "0.1.0"}, "@putnami/cli@0.1.0"},
		{"go", publishedArtifact{registry: "go", name: "go.putnami.dev/app", version: "v0.1.0"}, "go.putnami.dev/app@v0.1.0"},
		{"docker", publishedArtifact{registry: "docker", name: "registry.example.com/app", version: "1.2.3"}, "registry.example.com/app:1.2.3"},
		{"no version", publishedArtifact{registry: "docker", name: "registry.example.com/app"}, "registry.example.com/app"},
	}
	for _, tt := range tests {
		if got := tt.a.ref(); got != tt.want {
			t.Errorf("%s: ref() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestTextRendererFinishNoPublished(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start(nil)
	r.Finish(map[string]*jobs.JobResult{"a": {Status: "success"}}, jobs.SessionOutcome{})
	if strings.Contains(errOut.String(), "Published:") {
		t.Errorf("did not expect a Published block, got:\n%s", errOut.String())
	}
}

func TestLiveRendererFinishPublished(t *testing.T) {
	pub := makeLiveTestJob("cli", "publish~npm")
	r, errOut := newTestRenderer(t, 120, pub)

	ev := publishedEvent("npm", "@putnami/cli", "0.1.0-abc1234", []any{"latest"}, false)
	r.JobStart(pub)
	r.JobComplete(pub, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{ev}})
	r.Finish(map[string]*jobs.JobResult{
		pub.Key(): {Status: "success", Events: []jobs.RawJobEvent{ev}},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	for _, want := range []string{"Published:", "Version: 0.1.0-abc1234", "1 artifact", "npm 1", "latest (1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in live summary, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "@putnami/cli@0.1.0-abc1234") {
		t.Errorf("did not expect per-artifact ref in compact live summary, got:\n%s", out)
	}
}

func TestPublishedSummaryMixedGoVersion(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"cli:publish~npm": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("npm", "@putnami/cli", "0.1.0-abc", []any{"latest", "canary"}, false),
		}},
		"app:publish~go": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("go", "go.putnami.dev/app", "v0.1.0-abc", []any{"latest"}, false),
		}},
	}

	lines := formatPublishedSummaryLines(summarizePublished(collectPublished(results)))
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"Version: 0.1.0-abc",
		"Go module version: v0.1.0-abc",
		"2 artifacts",
		"go 1",
		"npm 1",
		"latest (2)",
		"canary (1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in summary, got:\n%s", want, got)
		}
	}
}

func TestPublishedSummaryGoOnlyKeepsGoVersion(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"app:publish~go": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("go", "go.putnami.dev/app", "v0.1.0-abc", []any{"latest"}, false),
		}},
	}

	lines := formatPublishedSummaryLines(summarizePublished(collectPublished(results)))
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "Version: v0.1.0-abc") {
		t.Errorf("expected Go-only summary to keep v-prefixed version, got:\n%s", got)
	}
	if strings.Contains(got, "Go module version:") {
		t.Errorf("did not expect separate Go module version for Go-only publish, got:\n%s", got)
	}
}

// publishedDockerEvent is publishedEvent plus the docker contentStatus field
// ("pushed", "retagged", or "reused") that docker publish jobs emit.
func publishedDockerEvent(name, version, contentStatus string, tags []any) jobs.RawJobEvent {
	ev := publishedEvent("docker", name, version, tags, false)
	ev.Data["contentStatus"] = contentStatus
	return ev
}

func TestPublishedSummaryDockerContentLine(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"a:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedDockerEvent("ghcr.io/x/a", "0.1.0-abc", "pushed", []any{"latest"}),
		}},
		"b:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedDockerEvent("ghcr.io/x/b", "0.1.0-abc", "retagged", []any{"latest"}),
		}},
		"c:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedDockerEvent("ghcr.io/x/c", "0.1.0-abc", "retagged", []any{"latest"}),
		}},
		"d:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedDockerEvent("ghcr.io/x/d", "0.1.0-abc", "reused", nil),
		}},
	}

	lines := formatPublishedSummaryLines(summarizePublished(collectPublished(results)))
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "Docker content: 1 pushed · 2 unchanged (retagged)") {
		t.Errorf("expected docker content line distinguishing pushed vs retagged, got:\n%s", got)
	}
	if !strings.Contains(got, "1 unchanged (reused)") {
		t.Errorf("expected docker content line to preserve exact reuse, got:\n%s", got)
	}
	if !strings.Contains(got, "Version: 0.1.0-abc") {
		t.Errorf("expected single session version, got:\n%s", got)
	}
}

func TestPublishedSummaryNoDockerContentLineWithoutStatus(t *testing.T) {
	results := map[string]*jobs.JobResult{
		"cli:publish~npm": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("npm", "@putnami/cli", "0.1.0-abc", []any{"latest"}, false),
		}},
		// Dry-run docker publishes carry no contentStatus (nothing was checked
		// against the registry).
		"svc:publish~docker": {Status: "success", Events: []jobs.RawJobEvent{
			publishedEvent("docker", "ghcr.io/x/svc", "0.1.0-abc", nil, true),
		}},
	}

	lines := formatPublishedSummaryLines(summarizePublished(collectPublished(results)))
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "Docker content:") {
		t.Errorf("expected no docker content line without contentStatus, got:\n%s", got)
	}
}
