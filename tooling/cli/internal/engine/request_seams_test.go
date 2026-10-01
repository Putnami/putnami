package engine

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// Two adapter seams were added to Request. Both exist because the
// extension alias is a real variant of the lifecycle, not a reduced copy of it,
// and both are easy to break silently — hence the ratchets here.

// TestRequest_PlanExtensions pins that the narrowing is opt-in and total: an
// adapter that supplies none plans against everything discovery found, and one
// that supplies a set plans against exactly that set. An empty (non-nil) slice
// is a deliberate "plan against nothing", not a fallback to everything, so a
// resolution that legitimately yields no extension cannot silently widen.
func TestRequest_PlanExtensions(t *testing.T) {
	t.Parallel()
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@putnami/a"}, {Name: "@putnami/b"},
	}}
	narrowed := []*extension.ExtensionDescription{{Name: "@putnami/only"}}

	cases := []struct {
		name string
		req  Request
		want []string
	}{
		{"nil plans against the discovered set", Request{}, []string{"@putnami/a", "@putnami/b"}},
		{"a narrowed set replaces it", Request{PlanExtensions: narrowed}, []string{"@putnami/only"}},
		{"an empty set is not a fallback", Request{PlanExtensions: []*extension.ExtensionDescription{}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, ext := range c.req.planExtensions(discovered) {
				got = append(got, ext.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("planExtensions = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRequest_PlanExtensionRebindsFreshDiscovery(t *testing.T) {
	t.Parallel()
	freshJob := &extension.JobDefinition{
		Name: "deploy",
		Flags: map[string]extension.FlagDefinition{
			"region": {Type: "string", Default: "fresh-default"},
			"base":   {Type: "boolean", Default: true},
		},
	}
	fresh := &extension.ExtensionDescription{
		Name:    "@putnami/cloud",
		Version: "2.0.0",
		Path:    "/fresh/extension",
		Jobs:    map[string]*extension.JobDefinition{"deploy": freshJob},
	}
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@putnami/other"},
		fresh,
	}}
	req := Request{PlanExtension: &PlanExtensionSelection{
		Name:    "@putnami/cloud",
		Command: "deploy",
		InheritedFlags: map[string]extension.FlagDefinition{
			"region":  {Type: "string", Default: "alias-default"},
			"dry-run": {Type: "boolean", Default: false},
		},
	}}

	got := req.planExtensions(discovered)
	if len(got) != 1 {
		t.Fatalf("planExtensions returned %d extensions, want one", len(got))
	}
	if got[0].Name != fresh.Name || got[0].Version != "2.0.0" || got[0].Path != "/fresh/extension" {
		t.Fatalf("plan extension = %+v, want freshly discovered owner", got[0])
	}
	flags := got[0].Jobs["deploy"].Flags
	if flags["region"].Default != "alias-default" {
		t.Errorf("inherited alias flag did not override fresh base: %+v", flags["region"])
	}
	if flags["base"].Default != true {
		t.Errorf("fresh base flag was lost: %+v", flags["base"])
	}
	if _, ok := flags["dry-run"]; !ok {
		t.Error("inherited alias-only flag was not overlaid")
	}
	if fresh.Jobs["deploy"].Flags["region"].Default != "fresh-default" {
		t.Error("fresh discovery result was mutated by the alias overlay")
	}
	if _, ok := fresh.Jobs["deploy"].Flags["dry-run"]; ok {
		t.Error("fresh discovery result gained an alias-only flag")
	}

	missing := Request{PlanExtension: &PlanExtensionSelection{
		Name:    "@putnami/missing",
		Command: "deploy",
	}}
	if rebound := missing.planExtensions(discovered); len(rebound) != 0 {
		t.Fatalf("missing owner widened to discovered extensions: %+v", rebound)
	}
}

// TestRequest_PreviewsOnly pins that --dry-run stays a plan-only preview for
// every adapter that does not opt out, and that opting out without --dry-run
// changes nothing.
func TestRequest_PreviewsOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dryRun  bool
		forward bool
		want    bool
	}{
		{"no dry-run", false, false, false},
		{"terminal dry-run previews", true, false, true},
		{"alias dry-run executes", true, true, false},
		{"opting out without dry-run is inert", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := Request{ExecutesUnderDryRun: c.forward}
			req.Global.DryRun = c.dryRun
			if got := req.previewsOnly(); got != c.want {
				t.Fatalf("previewsOnly() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestEngine_DryRunDecisionsGoThroughPreviewsOnly is the structural half. Three
// stages ask "does this run actually execute?" — the plan-only preview, the
// production preflight gate, and the unpublished-archives fatality — and every
// one of them must read previewsOnly. A stage that reads Global.DryRun directly
// would silently reinstate the plan-only short circuit for extension aliases,
// turning `putnami <group> <sub> --dry-run` into a no-op that still exits 0.
func TestEngine_DryRunDecisionsGoThroughPreviewsOnly(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine package dir: %v", err)
	}
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			// Comments name the field on purpose (this rule is documented at every
			// site it governs); only real reads count.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "Global.DryRun") {
				sites = append(sites, name+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "engine.go:") {
		t.Fatalf("Global.DryRun read at %v, want exactly one site in engine.go (previewsOnly).\n"+
			"  Every stage deciding whether the run executes must read previewsOnly, or the "+
			"extension-alias adapter loses its --dry-run job param.", sites)
	}
}
