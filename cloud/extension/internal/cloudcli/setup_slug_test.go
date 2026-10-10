package cloudcli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// setupSlugRun is one `putnami cloud setup` run of the slug tests: a signed-in
// user, a repository named "shop", and the fake control plane.
type setupSlugRun struct {
	workspaceRoot string
	fake          *fakeTransport
	output        []string
}

func newSetupSlugRun(t *testing.T) *setupSlugRun {
	t.Helper()
	run := &setupSlugRun{workspaceRoot: t.TempDir(), fake: newFakeTransport(t)}
	if err := os.WriteFile(filepath.Join(run.workspaceRoot, "putnami.json"), []byte(`{"name":"shop"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}
	return run
}

// setup runs `putnami cloud setup` with args and answers its error.
func (r *setupSlugRun) setup(t *testing.T, args ...string) error {
	t.Helper()
	home := t.TempDir()
	writeTestAuth(t, home)
	return RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": r.workspaceRoot,
		}),
		Stdout: func(line string) { r.output = append(r.output, line) },
		Stderr: func(string) {},
		Client: r.fake.client(),
		Now:    func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) },
	}, append(args, "--control-plane-url", testBaseURL, "--json"))
}

// created answers the body of the workspace creation, or nil when setup sent
// none.
func (r *setupSlugRun) created() map[string]any {
	for _, request := range r.fake.requests {
		if request.Method == http.MethodPost && request.Path == "/v1/workspaces" {
			return request.Body
		}
	}
	return nil
}

// linked reports whether setup wrote the link of the repository.
func (r *setupSlugRun) linked() bool {
	_, err := os.Stat(filepath.Join(r.workspaceRoot, LinkFileRelative))
	return err == nil
}

// requireUsageRefusal checks that setup refused the run as a usage error, and
// answers its message.
func requireUsageRefusal(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("setup succeeded, want a usage error")
	}
	var refused *cliError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %T %v, want a CLI error", err, err)
	}
	if refused.Code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (usage): %v", refused.Code, ExitUsage, err)
	}
	return err.Error()
}

// TestSetupSendsTheSlugOfANewWorkspace proves `cloud setup --slug` sends the
// slug in the request that creates the workspace, with the other fields
// unchanged.
func TestSetupSendsTheSlugOfANewWorkspace(t *testing.T) {
	for _, slug := range []string{"cd-proof", "a", "acme", "acme-prod-eu", "a1-b2", "w-0-db8055e0"} {
		t.Run(slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			if err := run.setup(t, "--workspace-name", "CD proof", "--slug", slug); err != nil {
				t.Fatalf("setup: %v", err)
			}
			body := run.created()
			if body == nil {
				t.Fatalf("setup created no workspace: %+v", run.fake.requests)
			}
			if body["slug"] != slug {
				t.Fatalf("created slug = %v, want %q", body["slug"], slug)
			}
			if body["name"] != "CD proof" {
				t.Fatalf("created name = %v, want %q", body["name"], "CD proof")
			}
			if !run.linked() {
				t.Fatal("setup wrote no link")
			}
		})
	}
}

// TestSetupWithoutSlugSendsNoSlug proves the flag changes nothing when it is
// absent or empty: the request carries no slug member, so identity-api derives
// one.
func TestSetupWithoutSlugSendsNoSlug(t *testing.T) {
	for name, args := range map[string][]string{
		"no flag":    nil,
		"empty flag": {"--slug", ""},
		"blank flag": {"--slug", "  "},
	} {
		t.Run(name, func(t *testing.T) {
			run := newSetupSlugRun(t)
			if err := run.setup(t, args...); err != nil {
				t.Fatalf("setup: %v", err)
			}
			body := run.created()
			if body == nil {
				t.Fatalf("setup created no workspace: %+v", run.fake.requests)
			}
			if value, present := body["slug"]; present {
				t.Fatalf("created body carries slug %v, want none: %v", value, body)
			}
			if body["name"] != "shop" {
				t.Fatalf("created name = %v, want %q", body["name"], "shop")
			}
		})
	}
}

// TestSetupRefusesASlugIdentityWouldRefuse proves setup checks the slug's shape
// the way identity-api does, before the session and any request: no workspace
// is created and no link is written.
func TestSetupRefusesASlugIdentityWouldRefuse(t *testing.T) {
	for _, tc := range []struct{ slug, want string }{
		{"acme-prod-eu1", "is longer than 12 characters"},
		{"averyverylongslug", "is longer than 12 characters"},
		{"Acme", "must be lowercase letters, digits and single dashes, starting with a letter"},
		{"1acme", "must be lowercase letters"},
		{"-acme", "must be lowercase letters"},
		{"acme-", "must be lowercase letters"},
		{"acme--eu", "must be lowercase letters"},
		{"acme.eu", "must be lowercase letters"},
		{"acme_eu", "must be lowercase letters"},
		{"acme eu", "must be lowercase letters"},
		{"acme/eu", "must be lowercase letters"},
		{"acmé", "must be lowercase letters"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			message := requireUsageRefusal(t, run.setup(t, "--slug", tc.slug))
			assertContains(t, message, tc.want)
			assertContains(t, message, `--slug "`+tc.slug+`"`)
			if len(run.fake.requests) != 0 {
				t.Fatalf("setup sent requests for a refused slug: %+v", run.fake.requests)
			}
			if run.linked() {
				t.Fatal("setup wrote a link for a refused slug")
			}
		})
	}
}

// TestSetupRefusesASlugFlagWithoutAValue proves a bare --slug never names a
// workspace: the flag parses as a boolean, which would otherwise be sent as the
// slug "true".
func TestSetupRefusesASlugFlagWithoutAValue(t *testing.T) {
	for _, flag := range []string{"--slug", "--no-slug"} {
		t.Run(flag, func(t *testing.T) {
			run := newSetupSlugRun(t)
			message := requireUsageRefusal(t, run.setup(t, flag))
			assertContains(t, message, "--slug needs a value")
			if len(run.fake.requests) != 0 {
				t.Fatalf("setup sent requests for a slug with no value: %+v", run.fake.requests)
			}
			if run.linked() {
				t.Fatal("setup wrote a link for a slug with no value")
			}
		})
	}
}

// TestSetupRefusesAReservedSlug proves setup refuses the platform's own names
// as the slug of a new workspace, before any request. Runtime gives no topic
// name to such a slug, and a workspace keeps its slug.
//
// The list is written out here. It is the list of reservedEventTopicSlugs in
// the runtime provisioner: a name added there must be added here too.
func TestSetupRefusesAReservedSlug(t *testing.T) {
	want := []string{
		"business", "catalog", "cloud", "config", "control", "data", "delivery",
		"distribution", "events", "identity", "infra", "intelligence",
		"observability", "platform", "putnami", "qa", "runtime", "source",
		"surfaces", "tooling",
	}
	got := make([]string, 0, len(reservedWorkspaceSlugs))
	for slug, reserved := range reservedWorkspaceSlugs {
		if reserved {
			got = append(got, slug)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("reserved slugs = %v, want %v", got, want)
	}

	for _, slug := range want {
		if len(slug) > maxWorkspaceSlugLen {
			// identity-api refuses this one for its length. No workspace has it.
			continue
		}
		t.Run(slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			message := requireUsageRefusal(t, run.setup(t, "--slug", slug))
			assertContains(t, message, `--slug "`+slug+`" is a platform name`)
			assertContains(t, message, "choose another slug")
			if len(run.fake.requests) != 0 {
				t.Fatalf("setup sent requests for a reserved slug: %+v", run.fake.requests)
			}
			if run.linked() {
				t.Fatal("setup wrote a link for a reserved slug")
			}
		})
	}
}

// TestSetupRefusesASlugThatEndsWithAReservedName proves setup refuses a slug
// whose last part after a dash is a platform name, before any request. The
// subscription ID of a push receive of "api-identity.key.revoked" reads
// "identity.key.revoked" after a dash, so Runtime refuses every push receive
// of a topic named under such a slug. A platform name elsewhere in the slug,
// or inside a longer last part, refuses nothing.
func TestSetupRefusesASlugThatEndsWithAReservedName(t *testing.T) {
	for slug, last := range map[string]string{
		"api-identity": "identity",
		"my-data":      "data",
		"plane-source": "source",
		"worker-data":  "data",
		"acme-qa":      "qa",
		"a-b-events":   "events",
	} {
		t.Run(slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			message := requireUsageRefusal(t, run.setup(t, "--slug", slug))
			assertContains(t, message, `--slug "`+slug+`" ends with "-`+last+`", a platform name`)
			assertContains(t, message, "choose another slug")
			if len(run.fake.requests) != 0 {
				t.Fatalf("setup sent requests for a refused slug: %+v", run.fake.requests)
			}
			if run.linked() {
				t.Fatal("setup wrote a link for a refused slug")
			}
		})
	}

	for _, slug := range []string{"data-acme", "qa-team", "acme-datas", "acme-qa1", "acme-xdata"} {
		t.Run(slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			if err := run.setup(t, "--slug", slug); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if body := run.created(); body == nil || body["slug"] != slug {
				t.Fatalf("created body = %v, want the slug %q", body, slug)
			}
		})
	}
}

// TestSetupLinksAnExistingWorkspaceThatHasTheSlug proves the same command runs
// twice: with a workspace that already has the asked slug, setup links it and
// creates nothing. The slug rules of a new workspace do not apply: the
// platform's own workspace has a reserved slug and still links.
func TestSetupLinksAnExistingWorkspaceThatHasTheSlug(t *testing.T) {
	for _, slug := range []string{"acme", "cloud"} {
		t.Run(slug, func(t *testing.T) {
			run := newSetupSlugRun(t)
			run.fake.workspaceSlug = slug
			if err := run.setup(t, "--workspace", "ws-acme", "--slug", slug); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if body := run.created(); body != nil {
				t.Fatalf("setup created a workspace: %v", body)
			}
			var link map[string]any
			readJSONFile(t, filepath.Join(run.workspaceRoot, LinkFileRelative), &link)
			if link["workspace_id"] != "ws-acme" {
				t.Fatalf("linked workspace = %v, want ws-acme", link["workspace_id"])
			}
		})
	}
}

// TestSetupRefusesAnotherSlugForAnExistingWorkspace proves --slug never passes
// silently on a workspace that keeps another slug: setup refuses, creates
// nothing and writes no link.
func TestSetupRefusesAnotherSlugForAnExistingWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, current, asked string
		want                 []string
	}{
		{"another slug", "acme", "shop", []string{`--slug "shop" does not apply`, `workspace ws-acme already exists with the slug "acme"`, "Run setup without --slug"}},
		{"a longer slug", "acme", "acme-eu", []string{`--slug "acme-eu" does not apply`, `the slug "acme"`}},
		{"no slug answered", "", "acme", []string{`--slug "acme" cannot be checked`, "workspace ws-acme answered no slug", "run setup without --slug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newSetupSlugRun(t)
			run.fake.workspaceSlug = tc.current
			message := requireUsageRefusal(t, run.setup(t, "--workspace", "ws-acme", "--slug", tc.asked))
			for _, want := range tc.want {
				assertContains(t, message, want)
			}
			if body := run.created(); body != nil {
				t.Fatalf("setup created a workspace: %v", body)
			}
			if run.linked() {
				t.Fatal("setup wrote a link with a refused slug")
			}
		})
	}
}

// TestSetupAutoSkipsOnAnotherSlug proves `cloud setup --auto` keeps its rule
// with --slug: it never fails the run. It reports the refused slug as a skip.
func TestSetupAutoSkipsOnAnotherSlug(t *testing.T) {
	run := newSetupSlugRun(t)
	run.fake.workspaceSlug = "acme"
	if err := run.setup(t, "--auto", "--workspace", "ws-acme", "--slug", "shop"); err != nil {
		t.Fatalf("setup --auto: %v", err)
	}
	var got map[string]any
	decodeJSON(t, strings.Join(run.output, "\n"), &got)
	if got["configured"] != false || got["skipped"] != true {
		t.Fatalf("result = %v, want a skip", got)
	}
	reason, _ := got["reason"].(string)
	assertContains(t, reason, `--slug "shop" does not apply`)
	if run.linked() {
		t.Fatal("setup --auto wrote a link with a refused slug")
	}
}

// TestExtensionManifestDeclaresTheSetupSlugFlag proves the published extension
// declares --slug on `cloud setup`: the Putnami CLI passes a command only the
// flags its manifest declares.
func TestExtensionManifestDeclaresTheSetupSlugFlag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		Commands map[string]struct {
			Flags map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	flag, ok := manifest.Commands["cloud-setup"].Flags["slug"]
	if !ok {
		t.Fatal("cloud-setup declares no --slug flag")
	}
	if flag.Type != "string" {
		t.Fatalf("--slug type = %q, want string", flag.Type)
	}
	if strings.TrimSpace(flag.Description) == "" {
		t.Fatal("--slug has no description")
	}
}
