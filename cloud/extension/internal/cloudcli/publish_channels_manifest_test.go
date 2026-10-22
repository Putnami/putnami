package cloudcli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestPublishTasksDeclareTheChannelsTheyServe pins the publish channels each
// step of Cloud's publish command serves. A publishing run counts a project's
// declared channel as served only by a planned task that declares it. The
// archive upload serves the archives of an extension and of a template, and
// the site content upload serves site content. The configuration, migration
// and deployment uploads serve no channel a project declares, so they declare
// none, and planning them for every project serves nothing.
func TestPublishTasksDeclareTheChannelsTheyServe(t *testing.T) {
	spectest.Proves(t, "cloud/publish-channels", "publish-tasks-declare-their-channels", "the-cloud-extension-declares-its-publish-channels")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) != 0 {
		t.Fatalf("strict extension parse diagnostics = %+v", diagnostics)
	}

	want := map[string][]string{
		"cloud-publish-archives-project":     {"archives", "template-archives"},
		"cloud-publish-config-project":       nil,
		"cloud-publish-deployment-project":   nil,
		"cloud-publish-migration-project":    nil,
		"cloud-publish-site-content-project": {"site-content"},
	}
	seen := map[string]bool{}
	for _, step := range manifest.Commands["publish"].Run {
		expected, known := want[step.Task]
		if !known {
			t.Errorf("publish step %q runs task %q, which this table does not pin", step.ID, step.Task)
			continue
		}
		seen[step.Task] = true
		var got []string
		if declares := manifest.Tasks[step.Task].Declares; declares != nil {
			got = declares.PublishChannels
		}
		if !slices.Equal(got, expected) {
			t.Errorf("task %q publishChannels = %v, want %v", step.Task, got, expected)
		}
	}
	for task := range want {
		if !seen[task] {
			t.Errorf("publish runs no step of task %q", task)
		}
	}
	for name, task := range manifest.Tasks {
		if _, pinned := want[name]; !pinned && task.Declares != nil && len(task.Declares.PublishChannels) > 0 {
			t.Errorf("task %q declares publishChannels %v outside the publish command", name, task.Declares.PublishChannels)
		}
	}
}
