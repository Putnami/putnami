package finalize

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/screenshots"

	"go.putnami.dev/tooling/contributor/internal/host"
	"go.putnami.dev/tooling/contributor/internal/screens"
)

const (
	screensFeature  = "tooling/change-screenshots"
	finalizeScreens = "finalize-asks-for-screenshots"
)

// noScreens is the TouchedScreens of a change that touches no web page.
func noScreens(context.Context, string, string, []string) ([]TouchedProject, error) { return nil, nil }

// webScreens touches one page of apps/web, on both sides.
func webScreens(context.Context, string, string, []string) ([]TouchedProject, error) {
	return []TouchedProject{{Project: "apps/web", Pages: []screens.Page{
		{File: "src/app/tasks/page.tsx", Route: "/tasks", Before: true, After: true},
		{File: "src/app/old/page.tsx", Route: "/old", Before: true},
	}}}, nil
}

// writeManifest writes a complete manifest of /tasks for head into dir, and
// returns its path.
func writeManifest(t *testing.T, dir, project, head string) string {
	t.Helper()
	return writeManifestOf(t, dir, project, head, screenshots.ProtocolVersion)
}

// writeManifestOf writes a complete manifest of /tasks, in a version, whose
// page proves the requirements given.
func writeManifestOf(t *testing.T, dir, project, head string, version int, proves ...screens.Proof) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := screens.Page{File: "src/app/tasks/page.tsx", Route: "/tasks", Path: "/tasks", Before: true, After: true, Proves: proves}
	manifest := screens.Manifest{ProtocolVersion: version, Project: project, BaseSHA: strings.Repeat("b", 40), HeadSHA: head,
		Pages: []screens.Page{page}}
	for _, side := range []string{screens.SideBefore, screens.SideAfter} {
		for _, viewport := range screens.Viewports {
			for _, scheme := range screens.Schemes {
				name := screens.CaptureName(0, page, side, viewport, scheme)
				data := []byte(fmt.Sprintf("png %s %s %s", side, viewport.Name, scheme))
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
				manifest.Captures = append(manifest.Captures, screens.Capture{File: name, Route: page.Route, Side: side,
					Viewport: viewport.Name, Scheme: scheme, SHA256: screens.Digest(data), Status: 200, URL: "http://127.0.0.1:3000/tasks"})
			}
		}
	}
	file, err := screens.WriteManifest(dir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// TestAReadyWebChangeWithoutScreenshotsIsRefused stops a ready publication
// of a change that touches a page, before any write, and names the project
// and its page.
func TestAReadyWebChangeWithoutScreenshotsIsRefused(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	f.Screens = webScreens
	_, err := f.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "apps/web has no --screenshots manifest for its touched pages /tasks") ||
		!strings.Contains(err.Error(), "--waive-screenshots <reason>") {
		t.Fatalf("err = %v", err)
	}
	if git.pushes != 0 || provider.upserts != 0 || provider.reviews != 0 {
		t.Fatalf("a refused run wrote: pushes=%d upserts=%d reviews=%d", git.pushes, provider.upserts, provider.reviews)
	}
	f.Screens = nil
	if _, err := f.Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "web project ownership unavailable") {
		t.Fatalf("a run without the seam: %v", err)
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-ready-web-change-without-screenshots-is-refused")
}

// TestAManifestOfTheHeadCoveringTheTouchedPagesIsAccepted publishes with a
// manifest of the head, and names it in the verification comment and the
// result.
func TestAManifestOfTheHeadCoveringTheTouchedPagesIsAccepted(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	provider.reviewOffered = true
	f.Screens = webScreens
	opts.Screenshots = []string{"shots/screenshots.json"}
	writeManifest(t, filepath.Join(f.Root, "shots"), "apps/web", git.head)
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "apps/web, 8 captures of 1 page"
	if result.Screenshots != want || len(provider.reviewBodies) != 1 || !strings.Contains(provider.reviewBodies[0], "- screenshots: "+want) {
		t.Fatalf("screenshots %q, comments %q", result.Screenshots, provider.reviewBodies)
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-manifest-of-the-head-covering-the-touched-pages-is-accepted")
}

// TestAStaleOrIncompleteManifestIsRefused refuses a manifest of another
// head, another project, with a missing or altered file, or that misses a
// touched page.
func TestAStaleOrIncompleteManifestIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, root, file string, f *Finalizer)
		want   string
	}{
		{"another head", func(t *testing.T, root, _ string, _ *Finalizer) {
			writeManifest(t, filepath.Join(root, "shots"), "apps/web", "ffffffffffffffffffffffffffffffffffffffff")
		}, "it captures ffffffffffff, not the head 012345678901"},
		{"another project", func(t *testing.T, root, _ string, _ *Finalizer) {
			writeManifest(t, filepath.Join(root, "shots"), "apps/admin", "0123456789012345678901234567890123456789")
		}, "apps/web has no --screenshots manifest"},
		{"a missing file", func(t *testing.T, root, _ string, _ *Finalizer) {
			_ = os.Remove(filepath.Join(root, "shots", "01-tasks-after-mobile-dark.png"))
		}, "the capture 01-tasks-after-mobile-dark.png is missing"},
		{"an altered file", func(t *testing.T, root, _ string, _ *Finalizer) {
			_ = os.WriteFile(filepath.Join(root, "shots", "01-tasks-after-desktop-light.png"), []byte("other"), 0o644)
		}, "does not hold the bytes the manifest hashed"},
		{"a manifest the protocol refuses", func(t *testing.T, _, file string, _ *Finalizer) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(file, []byte(strings.Replace(string(data), `"side": "after"`, `"side": "later"`, 1)), 0o644)
		}, "--screenshots shots/screenshots.json: not a screenshots manifest:\n  - [error] captures[4].side: side \"later\" is not in the set: before, after (screenshots.invalid_side)"},
		{"a page it misses", func(t *testing.T, _, _ string, f *Finalizer) {
			f.Screens = func(context.Context, string, string, []string) ([]TouchedProject, error) {
				return []TouchedProject{{Project: "apps/web", Pages: []screens.Page{
					{File: "src/app/tasks/page.tsx", Route: "/tasks", After: true},
					{File: "src/app/new/page.tsx", Route: "/new", After: true}}}}, nil
			}
		}, "the touched page /new (src/app/new/page.tsx) is not captured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, git, provider, opts := finalizerFixture(t)
			f.Screens = webScreens
			opts.Screenshots = []string{"shots/screenshots.json"}
			file := writeManifest(t, filepath.Join(f.Root, "shots"), "apps/web", git.head)
			tc.mutate(t, f.Root, file, &f)
			_, err := f.Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if git.pushes != 0 || provider.upserts != 0 {
				t.Fatalf("a refused run wrote: pushes=%d upserts=%d", git.pushes, provider.upserts)
			}
		})
	}
	f, _, _, opts := finalizerFixture(t)
	f.Screens = webScreens
	opts.Screenshots = []string{"nowhere/screenshots.json"}
	if _, err := f.Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "--screenshots nowhere/screenshots.json") {
		t.Fatalf("a missing manifest: %v", err)
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-stale-or-incomplete-manifest-is-refused")
}

// TestAManifestMissingATouchedRequirementIsRefused refuses a manifest whose
// page does not name a touched requirement it proves at the head, and
// accepts the manifest that names it.
func TestAManifestMissingATouchedRequirementIsRefused(t *testing.T) {
	filters := screens.Proof{Feature: "acme/tasks", Requirement: "filters-by-owner"}
	lists := screens.Proof{Feature: "acme/tasks", Requirement: "lists-open-tasks"}
	touched := func(context.Context, string, string, []string) ([]TouchedProject, error) {
		return []TouchedProject{{Project: "apps/web", Pages: []screens.Page{
			{File: "src/app/tasks/page.tsx", Route: "/tasks", Before: true, After: true, Proves: []screens.Proof{lists, filters}}},
			Touched: map[screens.Requirement]bool{{Feature: filters.Feature, Requirement: filters.Requirement}: true}}}, nil
	}
	for _, tc := range []struct {
		name   string
		proves []screens.Proof
		want   string
	}{
		{"no proof", nil, "the page /tasks does not name the touched requirement acme/tasks#filters-by-owner it proves"},
		{"another proof", []screens.Proof{lists}, "does not name the touched requirement acme/tasks#filters-by-owner"},
		{"the touched proof", []screens.Proof{filters}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, git, _, opts := finalizerFixture(t)
			f.Screens = touched
			opts.Screenshots = []string{"shots/screenshots.json"}
			writeManifestOf(t, filepath.Join(f.Root, "shots"), "apps/web", git.head, screenshots.ProtocolVersion, tc.proves...)
			_, err := f.Run(context.Background(), opts)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-manifest-missing-a-touched-requirement-is-refused")
}

// TestAVersion1ManifestIsRefused refuses a manifest written before pages
// named the requirements they prove, and says to capture again.
func TestAVersion1ManifestIsRefused(t *testing.T) {
	f, git, provider, opts := finalizerFixture(t)
	f.Screens = webScreens
	opts.Screenshots = []string{"shots/screenshots.json"}
	writeManifestOf(t, filepath.Join(f.Root, "shots"), "apps/web", git.head, screenshots.ProtocolVersionV1)
	_, err := f.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "it is a version 1 manifest, which names no requirement: run putnami change screenshots again") {
		t.Fatalf("err = %v", err)
	}
	if git.pushes != 0 || provider.upserts != 0 {
		t.Fatalf("a refused run wrote: pushes=%d upserts=%d", git.pushes, provider.upserts)
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-version-1-manifest-is-refused")
}

// TestAWaiverIsNamedInTheVerification publishes a web change with a waiver
// and writes its reason into the verification comment; a malformed waiver,
// or one with screenshots, is refused.
func TestAWaiverIsNamedInTheVerification(t *testing.T) {
	f, _, provider, opts := finalizerFixture(t)
	provider.reviewOffered = true
	f.Screens = webScreens
	opts.WaiveScreenshots = "the change only renames a CSS class"
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "waived: the change only renames a CSS class"
	if result.Screenshots != want || len(provider.reviewBodies) != 1 || !strings.Contains(provider.reviewBodies[0], "- screenshots: "+want) {
		t.Fatalf("screenshots %q, comments %q", result.Screenshots, provider.reviewBodies)
	}
	for _, bad := range []Options{
		{WaiveScreenshots: "two\nlines"},
		{WaiveScreenshots: strings.Repeat("x", maxWaiverBytes+1)},
		{WaiveScreenshots: "reason", Screenshots: []string{"a.json"}},
	} {
		o := opts
		o.WaiveScreenshots, o.Screenshots = bad.WaiveScreenshots, bad.Screenshots
		if _, err := f.Run(context.Background(), o); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-waiver-is-named-in-the-verification")
}

// TestADraftOrAChangeWithoutPagesNeedsNone publishes a draft of a web change
// with no screenshots, refuses screenshots on a draft, and publishes a ready
// change that touches no page.
func TestADraftOrAChangeWithoutPagesNeedsNone(t *testing.T) {
	f, _, provider, opts := finalizerFixture(t)
	provider.reviewOffered = true
	f.Screens = webScreens
	draft := opts
	draft.Draft, draft.VerificationFile = true, ""
	if _, err := f.Run(context.Background(), draft); err != nil {
		t.Fatalf("a draft of a web change: %v", err)
	}
	draft.WaiveScreenshots = "reason"
	if _, err := f.Run(context.Background(), draft); err == nil || !strings.Contains(err.Error(), "draft accepts no screenshots") {
		t.Fatalf("a draft with a waiver: %v", err)
	}
	f.Screens = noScreens
	result, err := f.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Screenshots != "no page touched" || !strings.Contains(provider.reviewBodies[len(provider.reviewBodies)-1], "- screenshots: no page touched") {
		t.Fatalf("screenshots %q", result.Screenshots)
	}
	unused := opts
	unused.WaiveScreenshots = "nothing to show"
	if result, err := f.Run(context.Background(), unused); err != nil || result.Screenshots != "no page touched, so the screenshots given are unused" {
		t.Fatalf("an unused waiver: %q, %v", result.Screenshots, err)
	}
	spectest.Proves(t, screensFeature, finalizeScreens, "a-draft-or-a-change-without-pages-needs-none")
}

// TestHostScreensReadsTheInventoriesOfTheOwningProjects resolves the touched
// pages of a real repository through the host's workspace.owners, from a
// workspace root below the repository's top level.
func TestHostScreensReadsTheInventoriesOfTheOwningProjects(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	writeFile := func(file, content string) {
		full := filepath.Join(repo, file)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inventory, _ := json.Marshal(map[string]any{"routes": []map[string]any{
		{"match": "exact", "path": "/", "methods": []string{"GET", "HEAD"}, "publicEdge": true,
			"provenance": map[string]any{"sourceKind": "file-route", "evidencePath": "src/app/page.tsx"}},
	}})
	run("init", "-q", "-b", "main")
	writeFile("ws/apps/web/schema/http-routes.json", string(inventory))
	writeFile("ws/apps/web/src/app/page.tsx", "1\n")
	writeFile("ws/lib/src/x.ts", "1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	writeFile("ws/apps/web/src/app/page.tsx", "2\n")
	writeFile("ws/lib/src/x.ts", "2\n")
	run("commit", "-qam", "change")
	head := run("rev-parse", "HEAD")

	h := newOwnersHost(t, map[string][]wire.OwningProject{
		"apps/web/src/app/page.tsx":      {{ID: "@acme/web", Path: "apps/web"}},
		"lib/src/x.ts":                   {{ID: "@acme/lib", Path: "lib"}},
		"apps/tasks/features/tasks.json": {{ID: "@acme/tasks", Path: "apps/tasks"}},
	}, nil)
	t.Setenv(wire.SocketEnv, h.socket)
	t.Setenv(wire.TokenEnv, h.token)
	channel := host.Take("")
	defer func() { _ = channel.Close() }()
	touched, err := HostScreens(filepath.Join(repo, "ws"), channel.Owners)(context.Background(), base, head,
		[]string{"ws/apps/web/src/app/page.tsx", "ws/lib/src/x.ts", "other/README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 1 || touched[0].Project != "apps/web" || len(touched[0].Pages) != 1 || touched[0].Pages[0].Route != "/" ||
		!touched[0].Pages[0].Before || !touched[0].Pages[0].After {
		t.Fatalf("touched = %+v", touched)
	}

	// A change to a requirement of another project touches the web page that
	// proves it, though the change holds no file of the web project.
	proving, _ := json.Marshal(map[string]any{"routes": []map[string]any{
		{"match": "exact", "path": "/", "methods": []string{"GET", "HEAD"}, "publicEdge": true,
			"provenance": map[string]any{"sourceKind": "file-route", "evidencePath": "src/app/page.tsx"},
			"proves":     []map[string]string{{"feature": "acme/tasks", "requirement": "lists-open-tasks", "at": "/"}}},
	}})
	feature := func(stage string) string {
		return `{"protocolVersion": 2, "namespace": "acme", "feature": {"id": "acme/tasks", "type": "feature", "name": "Tasks",
  "outcome": "A person sees the tasks", "owner": "web", "target": "coded", "requirements": [
  {"id": "lists-open-tasks", "stage": "` + stage + `", "evidenceKinds": ["attestation"]}]}}`
	}
	writeFile("ws/apps/web/schema/http-routes.json", string(proving))
	writeFile("ws/apps/tasks/features/tasks.json", feature("coded"))
	run("add", ".")
	run("commit", "-qam", "proves")
	base = run("rev-parse", "HEAD")
	writeFile("ws/apps/tasks/features/tasks.json", feature("operated"))
	run("commit", "-qam", "requirement")
	head = run("rev-parse", "HEAD")
	touched, err = HostScreens(filepath.Join(repo, "ws"), channel.Owners)(context.Background(), base, head,
		[]string{"ws/apps/tasks/features/tasks.json"})
	if err != nil {
		t.Fatal(err)
	}
	want := screens.Proof{Feature: "acme/tasks", Requirement: "lists-open-tasks"}
	if len(touched) != 1 || touched[0].Project != "apps/web" || len(touched[0].Pages) != 1 || touched[0].Pages[0].Route != "/" ||
		len(touched[0].Pages[0].Proves) != 1 || touched[0].Pages[0].Proves[0] != want ||
		!touched[0].Touched[screens.Requirement{Feature: want.Feature, Requirement: want.Requirement}] {
		t.Fatalf("a requirement change touched %+v", touched)
	}
	spectest.Proves(t, screensFeature, "touched-pages", "a-page-proving-a-touched-requirement-is-touched")
}
