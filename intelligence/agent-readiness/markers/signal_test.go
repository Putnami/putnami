package markers

import (
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/contract"
)

// TestCountsSignalLeavesTextAndSettingsOut excludes fixture strings,
// declarations without calls, and retry settings from executable test signals.
func TestCountsSignalLeavesTextAndSettingsOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		lines []string
		want  bool
	}{
		// Still counted.
		{"Go unguarded skip", "store/store_test.go", []string{"\tt.Skip(\"later\")"}, true},
		{"TypeScript focused test", "web/app.test.ts", []string{"it.only('renders', () => {"}, true},
		{"JavaScript skipped suite", "web/app.spec.js", []string{"describe.skip('renders', () => {"}, true},
		{"pytest skip marker", "tests/test_api.py", []string{"@pytest.mark.skip(reason=\"later\")"}, true},
		{"Playwright configure", "e2e/login.spec.ts", []string{"test.describe.configure({ retries: 2 });"}, true},
		{"Cypress test options", "cypress/e2e/login.cy.ts", []string{"it('logs in', { retries: 2 }, () => {"}, true},
		{"Cypress test options on the next line", "e2e/login.spec.ts", []string{"it('logs in', {", "  retries: { runMode: 2 },"}, true},
		{"runner configuration", "playwright.config.ts", []string{"  retries: 2,"}, true},
		{"CI configuration", ".github/workflows/ci.yml", []string{"      - run: \"gotestsum --rerun-fails ./...\""}, true},
		{"unknown extension keeps its tokens", "web/app.test.cjs", []string{"it.only('renders', () => {"}, true},
		// Left out.
		{"Go skip inside a string", "lint/skip_guard_test.go", []string{"const unguarded = \"func TestLater(t *testing.T) {\\n\\tt.Skip(\\\"later\\\")\\n}\""}, false},
		{"pytest fixture in a Go test", "jobs/spec_plugin_test.go", []string{"@pytest.mark.skip(reason=\"deliberate\")"}, false},
		{"TypeScript fixture in a Go test", "cmd/skip_guard_test.go", []string{"\twrite(t, \"a.test.ts\", \"test.only('a', () => {});\\n\")"}, false},
		{"Go method named only", "otlp/otlp_test.go", []string{"\tspans := col.only(t)"}, false},
		{"Rails query in a Ruby spec", "spec/model_spec.rb", []string{"  scope.only(:id)"}, false},
		{"CLI flag in Go test data", "cli/registry_test.go", []string{"\t\"unknown flag\": {\"/app\", \"--retries\", \"3\"},"}, false},
		{"setting of the code under test", "database/test/logging.test.ts", []string{"      durationUs: 2000,", "      retries: 0,"}, false},
		{"client option in a test", "events/test/client.test.ts", []string{"    const client = eventClient({", "      reconnect: { retries: 1 },"}, false},
		{"logged data under a test", "runtime/test/logger.test.ts", []string{"    it('redacts nested keys', () => {", "      logger.info('config', { retries: 3 });"}, false},
		{"guarded skip", "store/store_test.go", []string{"\tif runtime.GOOS == \"windows\" {", "\t\tt.Skip(\"symlinks\")"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountsSignal(tc.file, tc.lines, len(tc.lines)-1); got != tc.want {
				t.Fatalf("CountsSignal(%s, %q) = %v, want %v", tc.file, tc.lines, got, tc.want)
			}
		})
	}
}

// TestAreaDocsReadsThePutnamiDocsCheck checks that a Putnami workspace whose
// CI runs lint or validate-workspace has a docs check: both check every
// README and doc tree's links.
func TestAreaDocsReadsThePutnamiDocsCheck(t *testing.T) {
	for _, run := range []string{"./putnamiw lint,test,build --impacted", "putnami validate-workspace"} {
		c := computationOf([]string{"README.md", "putnami.workspace.json", ".github/workflows/ci.yml"}, map[string]string{
			".github/workflows/ci.yml": "on: pull_request\njobs: {c: {steps: [{run: " + run + "}]}}",
		})
		if m := c.areaDocs(-1); m.State != contract.MarkerEnforced {
			t.Errorf("area docs with CI %q = %+v, want enforced", run, m)
		}
	}
}
