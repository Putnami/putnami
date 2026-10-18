package scheduler

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// apiCheckTagged opens a lane over the checkout at root, gives the api project
// the tags, plans validate, and returns the cache key of the project's API
// check, the tags its job context carries, and the encoded context. The key
// comes from the pass that keys a run and the context from the builder that
// writes a task's context file.
func apiCheckTagged(
	t *testing.T,
	root string,
	exts []*extension.ExtensionDescription,
	tags []string,
) (string, []string, []byte) {
	t.Helper()
	lane := openReleaseBaselineLane(t, root)
	for _, project := range lane.ws.Projects {
		if project.ID == "/api" {
			project.Tags = tags
		}
	}
	planned, keys := lane.keys(t, exts, []string{"validate"}, nil, releaseBaselineBuildTime)
	for _, job := range planned {
		if job.Project.ID != "/api" || job.Step.Task != "validate-api" {
			continue
		}
		key, keyed := keys[job.Key()]
		if !keyed || key == "" {
			t.Fatalf("%s is planned but not keyed in %v", job.Key(), sortedKeyNames(keys))
		}
		encoded, err := json.Marshal(BuildJobContext(lane.ws, job, nil, nil, lane.versions))
		if err != nil {
			t.Fatal(err)
		}
		parsed, diags := protocoljob.ParseAndValidate(encoded)
		if diag.HasErrors(diags) {
			t.Fatalf("the context of %s violates the job context protocol: %v\n%s", job.Key(), diags, encoded)
		}
		// The key's project metadata digest hashes this view.
		digested := workspace.ProbeViewOf(job.Project, lane.ws.DependencyPathResolver()).Tags
		if !slices.Equal(parsed.Project.Tags, digested) {
			t.Errorf("the context of %s carries the tags %q; the view its key digests holds %q",
				job.Key(), parsed.Project.Tags, digested)
		}
		return key, parsed.Project.Tags, encoded
	}
	t.Fatalf("no validate-api job for /api among %v", sortedKeyNames(keys))
	return "", nil, nil
}

// The job context carries a project's tags in the form the task cache key holds
// them. Two tag lists give the API check one key exactly when they give its
// context one tag list, so a task reads no tag fact its key does not hold: the
// order of the list, a repeated tag and surrounding white space reach neither,
// and a tag added or removed reaches both.
func TestTheJobContextCarriesTheTagsTheCacheKeyHolds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateTreeKeyedGit(t)
	exts := treeKeyedExtensions(t)
	root := releaseBaselineHistory(t, t.TempDir())

	cases := []struct {
		name string
		tags []string
		want []string
	}{
		{"two tags", []string{"go", "protocol"}, []string{"go", "protocol"}},
		{"the same tags in another order", []string{"protocol", "go"}, []string{"go", "protocol"}},
		{"a repeated tag", []string{"go", "protocol", "go"}, []string{"go", "protocol"}},
		{"surrounding white space", []string{" protocol", "go\t", " "}, []string{"go", "protocol"}},
		{"one tag less", []string{"go"}, []string{"go"}},
		{"another tag", []string{"go", "public"}, []string{"go", "public"}},
		{"no tags", nil, []string{}},
		{"cleared tags", []string{}, []string{}},
	}
	type outcome struct {
		key  string
		tags []string
	}
	outcomes := make([]outcome, len(cases))
	for i, tc := range cases {
		key, tags, encoded := apiCheckTagged(t, root, exts, tc.tags)
		if tags == nil {
			t.Fatalf("%s: the context carries no project.tags member:\n%s", tc.name, encoded)
		}
		if !slices.Equal(tags, tc.want) {
			t.Errorf("%s: the context carries the tags %q, want %q", tc.name, tags, tc.want)
		}
		outcomes[i] = outcome{key: key, tags: tags}
	}
	for i := range cases {
		for j := i + 1; j < len(cases); j++ {
			sameKey := outcomes[i].key == outcomes[j].key
			sameTags := slices.Equal(outcomes[i].tags, outcomes[j].tags)
			if sameKey != sameTags {
				t.Errorf("%q and %q: same key = %v, same context tags = %v; the two must agree",
					cases[i].name, cases[j].name, sameKey, sameTags)
			}
		}
	}

	// A project that resolves to no tags says so: the member is present and
	// empty, which a consumer tells apart from an absent member.
	_, _, encoded := apiCheckTagged(t, root, exts, nil)
	if !bytes.Contains(encoded, []byte(`"tags":[]`)) {
		t.Errorf("a project with no tags must encode \"tags\":[]:\n%s", encoded)
	}
}

// The tags the job context carries are the resolved ones: the tags the project
// declares, else the tags a workspace provider reports, else the tags of its
// scopes. The project metadata digest of the cache key moves with them.
func TestTheJobContextTagsFollowTheResolvedTags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		authored []string
		declares bool
		provider []string
		scope    []string
		want     []string
	}{
		{
			name:     "authored tags win",
			authored: []string{"protocol", "go"}, declares: true,
			provider: []string{"provided"}, scope: []string{"scoped"},
			want: []string{"go", "protocol"},
		},
		{
			name:     "provider tags win over scope tags",
			provider: []string{"provided"}, scope: []string{"scoped"},
			want: []string{"provided"},
		},
		{
			name:  "scope tags apply last",
			scope: []string{"scoped", "go"},
			want:  []string{"go", "scoped"},
		},
		{
			name:     "an authored empty list blocks both",
			authored: []string{}, declares: true,
			provider: []string{"provided"}, scope: []string{"scoped"},
			want: []string{},
		},
		{name: "nothing declares a tag", want: []string{}},
	}
	digests := map[string]string{}
	for _, tc := range cases {
		project := &workspace.Project{
			ID: "/lib/greet", Path: "lib/greet",
			Scope: workspace.ScopeContribution{Tags: tc.scope},
		}
		if tc.declares {
			project.Config = &wsproto.ProjectConfig{Tags: tc.authored}
		}
		workspace.ResolveProjectIdentity(project, &wsproto.MergedProject{Path: "lib/greet", Tags: tc.provider})

		got := projectContextTags(project)
		if got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: context tags = %#v, want %#v", tc.name, got, tc.want)
		}

		// Only the resolved tags differ between the cases, so two cases share
		// a metadata digest exactly when they share their context tags.
		digest := testWorkspace("/ws", project).MetadataDigestFor(project)
		spelling, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for other, otherDigest := range digests {
			sameTags := other == string(spelling)
			if sameTags != (otherDigest == digest) {
				t.Errorf("%s: context tags %s against %s: same tags = %v, same metadata digest = %v",
					tc.name, spelling, other, sameTags, otherDigest == digest)
			}
		}
		digests[string(spelling)] = digest
	}
}
