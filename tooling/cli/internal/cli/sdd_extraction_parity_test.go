package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The extraction acceptance: for every one of the
// fourteen SDD subcommands, `@putnami/sdd` must produce the bytes the built-in
// command produced.
//
// # The oracle is recorded, because the oracle is gone
//
// Until Task 8 the oracle ran live: this file called core's own dispatcher
// (App.runStructuredCommand, the function that wrote what `putnami features
// list --output=json` wrote) beside the extension and compared the two streams.
// Task 8 deletes that dispatcher, so the live comparison cannot survive it.
//
// Deleting the test with the oracle was the wrong disposition. This file IS the
// evidence that the extraction preserved behavior, and evidence that only
// existed while both implementations did is evidence nobody can check
// afterwards. So the last thing core did before it was removed was answer all
// 53 invocations below, and its answers were written verbatim into
// testdata/sdd-parity/ in the SAME commit that removed it. The comparison is
// unchanged in what it asserts; only the oracle's storage changed, from a
// function call to a file.
//
// # What that costs, stated plainly
//
// A recorded oracle cannot be regenerated: there is no longer an implementation
// that can produce these bytes except the one under test, and regenerating them
// FROM the extension would turn the acceptance into a tautology. So there is
// deliberately no -update flag here. A recorded answer changes only by a human
// editing the file, in a commit that says which user-visible output changed and
// why — which is the review the acceptance exists to force.
//
// Provenance: captured from tooling/cli/internal/commands/sdd at the parent of
// the Task 8 commit, through App.runStructuredCommand, over the fixture
// parityWorkspace builds below. The fixture's git commit is byte-deterministic
// (parityCommitDate), so the revision each answer carries is compared verbatim
// rather than normalized away.
//
// # How the extension side runs
//
// Through the real dispatch: runExtensionStructuredCommand — the interactive
// path, the real job context, the real subprocess — with a manifest that is the
// shipped putnami.extension.json with `{extensionRuntime}` replaced by a binary
// this test compiles. Nothing about it is a stand-in except the runtime's
// location. The group name is supplied to ParseArgs as an extension group,
// which is what discovery now does for real, since Task 8 removed the built-in
// that used to shadow it.

// parityCase is one subcommand invocation, compared against its recorded
// answer.
type parityCase struct {
	// name identifies the subtest AND, through paritySlug, the file holding the
	// recorded answer; args is the full CLI invocation.
	name string
	args []string
	// before prepares the tree ahead of the run. It reproduces exactly what the
	// tree looked like when core answered.
	before func(t *testing.T, root string)
	// wantExit is the exit code the recorded answer must carry. It is stated
	// rather than merely read from the file so a recording that was edited into
	// a different verdict is visible here instead of silently accepted.
	wantExit int
}

// TestSDDSubcommandsMatchTheRecordedBuiltIn is the acceptance: same fixture
// workspace, same arguments, the same stdout, stderr and exit code the built-in
// produced.
func TestSDDSubcommandsMatchTheRecordedBuiltIn(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/sdd-extraction", "recorded-parity-with-the-replaced-surface", "every-subcommand-matches-the-recorded-builtin-byte-for-byte")
	runtimePath := parityExtensionRuntime(t)

	for _, sdd := range parityCases() {
		t.Run(sdd.name, func(t *testing.T) {
			t.Parallel()
			limitParityConcurrency(t)
			// A FRESH tree per case, which is how the answers were recorded: a
			// shared root would let a writing subcommand change the tree a later
			// case is compared on.
			root := parityWorkspace(t)
			extensions := parityExtensions(t, root, runtimePath)
			if sdd.before != nil {
				sdd.before(t, root)
			}

			out, errOut, code := runParityExtension(t, root, extensions, sdd.args)
			assertRecordedParityAnswer(t, filepath.Join("structured", paritySlug(sdd.name)),
				root, sdd.wantExit, code, out, errOut)
		})
	}
}

// assertRecordedParityAnswer compares one live answer with the recorded one.
func assertRecordedParityAnswer(t *testing.T, name, root string, wantExit, code int, stdout, stderr string) {
	t.Helper()

	recorded := readRecordedParityAnswer(t, name)
	if recorded.exit != wantExit {
		t.Errorf("the recorded answer exits %d, but this case claims %d — the table and the recording disagree",
			recorded.exit, wantExit)
	}
	if code != recorded.exit {
		t.Errorf("exit code: extension %d, recorded built-in %d", code, recorded.exit)
	}
	if got := normalizeParityBytes(stdout, root); got != recorded.stdout {
		t.Errorf("stdout differs from the recorded built-in answer.\n--- recorded ---\n%s\n--- extension ---\n%s", recorded.stdout, got)
	}
	if got := normalizeParityBytes(stderr, root); got != recorded.stderr {
		t.Errorf("stderr differs from the recorded built-in answer.\n--- recorded ---\n%s\n--- extension ---\n%s", recorded.stderr, got)
	}
}

// parityCases is the inventory table, executed.
//
// Every subcommand appears at least once in a structured mode, because that is
// the byte-for-byte requirement; the human mode is covered by
// TestSDDHumanOutputMatchesTheRecordedBuiltIn below, over the same fixture.
// Selection flags appear exactly where the inventory says they are honored, and
// exactly where it says they are refused.
func parityCases() []parityCase {
	return []parityCase{
		{name: "features list", args: []string{"features", "list", "--output=json"}},
		{name: "features list query", args: []string{"features", "list", "invoice", "--output=json"}},
		{name: "features list projects", args: []string{"features", "list", "--projects", "@acme/billing", "--output=json"}},
		{name: "features list jsonl", args: []string{"features", "list", "--output=jsonl"}},
		{name: "features validate", args: []string{"features", "validate", "--output=json"}},
		{name: "features validate projects", args: []string{"features", "validate", "--projects", "@acme/billing", "--output=json"}},
		{name: "features snapshot", args: []string{"features", "snapshot", "--output=json"}},
		{name: "features inspect", args: []string{"features", "inspect", "billing/invoice", "--output=json"}},
		{
			name:     "features inspect unknown",
			args:     []string{"features", "inspect", "billing/nope", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "features inspect refuses selection",
			args:     []string{"features", "inspect", "billing/invoice", "--projects", "@acme/billing", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "features inspect refuses --all",
			args:     []string{"features", "inspect", "billing/invoice", "--all", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "features inspect refuses --baseline",
			args:     []string{"features", "inspect", "billing/invoice", "--baseline", "HEAD", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "features inspect arity",
			args:     []string{"features", "inspect", "--output=json"},
			wantExit: 2,
		},
		{name: "features diff", args: []string{"features", "diff", "HEAD", "HEAD", "--output=json"}},
		{
			name:     "features diff unresolvable",
			args:     []string{"features", "diff", "refs/heads/nope", "HEAD", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "features diff arity",
			args:     []string{"features", "diff", "HEAD", "--output=json"},
			wantExit: 2,
		},
		{name: "specs list", args: []string{"specs", "list", "--output=json"}},
		{name: "specs list projects", args: []string{"specs", "list", "--projects", "@acme/billing", "--output=json"}},
		{name: "specs validate", args: []string{"specs", "validate", "--output=json"}},
		{name: "specs validate impacted", args: []string{"specs", "validate", "--impacted", "--output=json"}},
		{name: "specs inspect", args: []string{"specs", "inspect", "billing/invoice", "--output=json"}},
		{
			name:     "specs inspect refuses selection",
			args:     []string{"specs", "inspect", "billing/invoice", "--projects", "@acme/billing", "--output=json"},
			wantExit: 2,
		},
		{name: "specs init dry-run", args: []string{"specs", "init", "shipping/labels", "--dry-run", "--output=json"}},
		{
			// The one WRITING case, which is why every case gets its own tree:
			// core answered on a tree where the file did not exist yet, and so
			// must the extension.
			name: "specs init writes",
			args: []string{"specs", "init", "shipping/labels", "--output=json"},
		},
		{
			name:     "specs init duplicate",
			args:     []string{"specs", "init", "billing/invoice", "--output=json"},
			wantExit: 2,
		},
		{name: "architecture validate", args: []string{"architecture", "validate", "--output=json"}},
		{name: "architecture validate baseline", args: []string{"architecture", "validate", "--baseline", "HEAD", "--output=json"}},
		{name: "architecture snapshot", args: []string{"architecture", "snapshot", "--output=json"}},
		{name: "architecture inspect", args: []string{"architecture", "inspect", "billing", "--output=json"}},
		{
			name:     "architecture inspect unknown",
			args:     []string{"architecture", "inspect", "absent", "--output=json"},
			wantExit: 2,
		},
		{
			// Generate is idempotent — the second run rewrites identical bytes —
			// so it needs no restore between the two: the report names the
			// artifacts, not what changed.
			name: "contracts generate",
			args: []string{"contracts", "generate", "--project", "@acme/billing", "--output=json"},
		},
		{
			name: "contracts check clean",
			args: []string{"contracts", "check", "--project", "@acme/billing", "--output=json"},
			// Generate first so the committed artifacts exist and match.
			before: func(t *testing.T, root string) { generateParityContracts(t, root) },
		},
		{
			name: "contracts check drift",
			args: []string{"contracts", "check", "--project", "@acme/billing", "--output=json"},
			before: func(t *testing.T, root string) {
				generateParityContracts(t, root)
				driftParityContracts(t, root)
			},
			wantExit: 2,
		},
		{
			name:     "contracts without a target",
			args:     []string{"contracts", "check", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "contracts with an unknown project",
			args:     []string{"contracts", "check", "--project", "@acme/nope", "--output=json"},
			wantExit: 2,
		},
		{
			// `--projects` is the global fallback the handler reads when
			// --project is absent, and its RAW spelling is what the extension
			// receives: resolving it first would accept a selector core refuses.
			name: "contracts via the global selector",
			args: []string{"contracts", "check", "--projects", "@acme/billing", "--output=json"},
			before: func(t *testing.T, root string) {
				generateParityContracts(t, root)
			},
		},
		{
			name:     "contracts refuses --all as a target",
			args:     []string{"contracts", "check", "--all", "--output=json"},
			wantExit: 2,
		},
		{
			name:     "contracts refuses --impacted as a target",
			args:     []string{"contracts", "check", "--impacted", "--output=json"},
			wantExit: 2,
		},
	}
}

// TestSDDHumanOutputMatchesTheRecordedBuiltIn is the same comparison without
// `--output`, where the renderer owns stdout instead of the envelope. It is a
// separate test so a failure names which half broke: a payload difference and a
// rendering difference are fixed in different files.
func TestSDDHumanOutputMatchesTheRecordedBuiltIn(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/sdd-extraction", "recorded-parity-with-the-replaced-surface", "the-human-output-matches-the-recorded-builtin")
	runtimePath := parityExtensionRuntime(t)

	for _, args := range parityHumanCases() {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			limitParityConcurrency(t)
			root := parityWorkspace(t)
			extensions := parityExtensions(t, root, runtimePath)
			// `check` compares the committed artifacts against a fresh render, so
			// it needs them present. They are restored from the recording rather
			// than generated, which makes this case an assertion about the
			// extension's renderer instead of a comparison with itself.
			if args[0] == "contracts" && args[1] == "check" {
				generateParityContracts(t, root)
			}

			out, errOut, code := runParityExtension(t, root, extensions, args)
			recorded := readRecordedParityAnswer(t, filepath.Join("human", paritySlug(strings.Join(args, " "))))
			if code != recorded.exit {
				t.Errorf("exit code: extension %d, recorded built-in %d", code, recorded.exit)
			}
			if got := normalizeParityBytes(out, root); got != recorded.stdout {
				t.Errorf("human stdout differs from the recorded built-in answer.\n--- recorded ---\n%s\n--- extension ---\n%s", recorded.stdout, got)
			}
			if got := normalizeParityBytes(errOut, root); got != recorded.stderr {
				t.Errorf("human stderr differs from the recorded built-in answer.\n--- recorded ---\n%s\n--- extension ---\n%s", recorded.stderr, got)
			}
		})
	}
}

func parityHumanCases() [][]string {
	return [][]string{
		{"features", "list"},
		{"features", "validate"},
		{"features", "snapshot"},
		{"features", "inspect", "billing/invoice"},
		{"features", "inspect", "billing/nope"},
		{"features", "diff", "HEAD", "HEAD"},
		{"specs", "list"},
		{"specs", "validate"},
		{"specs", "inspect", "billing/invoice"},
		{"specs", "init", "shipping/labels", "--dry-run"},
		{"architecture", "validate"},
		{"architecture", "snapshot"},
		{"architecture", "inspect", "billing"},
		{"contracts", "generate", "--project", "@acme/billing"},
		{"contracts", "check", "--project", "@acme/billing"},
	}
}

// runParityExtension runs one invocation through the extension dispatch path —
// the one `putnami features list` takes now that Task 8 removed the built-in.
//
// The group name is supplied to ParseArgs as an extension group, which is what
// discovery does for real: it selects the same parse (a structured command's
// leading positional is its subcommand, not a project selector) and skips the
// built-in flag validation that a manifest-served command does not get.
func runParityExtension(
	t *testing.T,
	root string,
	extensions []*extension.ExtensionDescription,
	args []string,
) (stdout, stderr string, code int) {
	t.Helper()
	groups := map[string]bool{args[0]: true}
	parsed := ParseArgs(args, nil, groups)
	if parsed.Err != nil {
		t.Fatalf("the parser rejected %v: %v", args, parsed.Err)
	}
	cfg := wsproto.Load(root)
	app := &App{}
	var out, errOut bytes.Buffer
	code = app.runExtensionStructuredCommand(
		context.Background(), parsed, cfg, root, extensions,
		os.Stdin, &out, &errOut,
	)
	return out.String(), errOut.String(), code
}

// parityExtensionRuntime compiles the extension once per test binary and
// returns the executable's path.
//
// It builds with GOWORK=off, exactly as tooling/sdd-extension/bin/prepare does,
// so the artifact depends on that module's own go.mod and nothing ambient. The
// build is a real dependency of this test and not a convenience: comparing
// against a stale binary would report parity the current source does not have.
func parityExtensionRuntime(t *testing.T) string {
	t.Helper()
	parityRuntimeOnce.Do(func() {
		// NOT t.TempDir(): it is removed when the first test that triggered the
		// build finishes, and every later test in this file needs the binary.
		// The whole directory is removed by TestMain instead, or by the next run
		// when this one is killed.
		dir, err := scratch.New("putnami-sdd-parity-")
		if err != nil {
			parityRuntimeErr = err.Error()
			return
		}
		parityRuntimeDir = dir
		output := filepath.Join(dir.Path(), pkgmeta.ExecutableName(runtime.GOOS, "putnami-sdd"))
		cmd := exec.Command("go", "build", "-o", output, "./cmd/putnami-sdd")
		cmd.Dir = filepath.Join("..", "..", "..", "sdd-extension")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			parityRuntimeErr = string(out)
			return
		}
		parityRuntimePath = output
	})
	if parityRuntimePath == "" {
		t.Fatalf("could not build the @putnami/sdd runtime, so parity cannot be measured:\n%s", parityRuntimeErr)
	}
	return parityRuntimePath
}

// TestMain removes the compiled runtime this file stages outside the test
// framework's own temporary directories. It also drops a hosted run's
// invocation broker: registry fixtures in this package stand up their own
// server, and the broker wins over every authored route while answering 401
// to a test archive. Tests that exercise the broker set it with t.Setenv. As
// the runtime of a fixture extension, this binary answers the runtime-info
// handshake and exits (answerRuntimeHandshake). GORACE has every copy of this
// binary the tests start exit without the race runtime's 1 s exit sleep.
func TestMain(m *testing.M) {
	if answerRuntimeHandshake() {
		os.Exit(0)
	}
	os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	_ = os.Setenv("GORACE", "atexit_sleep_ms=0")
	code := m.Run()
	_ = parityRuntimeDir.Remove()
	_ = parityWorkspaceTemplateScratch.Remove()
	os.Exit(code)
}

var (
	parityRuntimeOnce sync.Once
	parityRuntimeDir  *scratch.Dir
	parityRuntimePath string
	parityRuntimeErr  string

	parityWorkspaceTemplateOnce    sync.Once
	parityWorkspaceTemplateScratch *scratch.Dir
	parityWorkspaceTemplateDir     string
	parityConcurrency              = make(chan struct{}, max(1, min(4, runtime.GOMAXPROCS(0))))
)

// limitParityConcurrency keeps the parity acceptance from trading one long
// serial pole for a fork storm. At most four independent fixture copies overlap
// their mostly waiting extension subprocesses; the immutable template removes
// the former per-case Git commit burst that made an unbounded fan-out unsafe.
func limitParityConcurrency(t *testing.T) {
	t.Helper()
	parityConcurrency <- struct{}{}
	t.Cleanup(func() { <-parityConcurrency })
}

// parityExtensions resolves the SHIPPED manifest with its runtime pointed at
// the freshly built binary.
//
// The manifest is the real one — the commands, the groups, the interactive
// flags and the task arguments a user's workspace would load — because a parity
// test against a hand-written manifest would prove the binary right and the
// declaration unchecked.
func parityExtensions(t *testing.T, root, runtimePath string) []*extension.ExtensionDescription {
	t.Helper()
	manifestPath := filepath.Join("..", "..", "..", "sdd-extension", "putnami.extension.json")
	manifest, err := extension.LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("the shipped manifest did not load: %v", err)
	}
	description := extension.Resolve(manifest, filepath.Join(root, ".sdd-extension"))
	// The runtime is normally prepared by the CLI into the artifact store. Here
	// it is already built, so the resolved executable is stated directly; every
	// task's {extensionRuntime} expands to it.
	description.RuntimeExecutable = runtimePath
	return []*extension.ExtensionDescription{description}
}

// --- the fixture ------------------------------------------------------------

// parityWorkspace writes one workspace that exercises all four verticals, and
// commits it, because `features diff` reads git objects rather than the
// worktree.
//
// Two properties are deliberate. It declares NO publish channels and no
// featureAuthority: both are facts the job-context wire carries only for the
// job's OWN project (see internal/wsview's package doc), so a fixture that used
// them would be measuring a documented wire gap instead of this task's work.
// TestSDDSpecCompletenessDivergesOnAPublishableSibling below states that gap
// separately, as a difference rather than as a silence.
func parityWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyParityTree(t, parityWorkspaceTemplate(t), root)
	return root
}

// parityWorkspaceTemplate builds the byte-identical committed fixture once per
// test binary. Every parity case still receives its own writable copy, but it
// no longer pays four Git subprocesses to recreate the same immutable history.
func parityWorkspaceTemplate(t *testing.T) string {
	t.Helper()
	parityWorkspaceTemplateOnce.Do(func() {
		var err error
		parityWorkspaceTemplateScratch, err = scratch.New("putnami-sdd-parity-workspace-")
		if err != nil {
			t.Fatalf("create parity workspace template: %v", err)
		}
		// A subdirectory: the scratch root holds its owner lock file, which
		// every case's copy of the template would otherwise carry.
		parityWorkspaceTemplateDir = filepath.Join(parityWorkspaceTemplateScratch.Path(), "workspace")
		if err := os.Mkdir(parityWorkspaceTemplateDir, 0o755); err != nil {
			t.Fatalf("create parity workspace template: %v", err)
		}
		populateParityWorkspace(t, parityWorkspaceTemplateDir)
	})
	if parityWorkspaceTemplateDir == "" {
		t.Fatal("parity workspace template was not created")
	}
	return parityWorkspaceTemplateDir
}

func copyParityTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy parity workspace template: %v", err)
	}
}

func populateParityWorkspace(t *testing.T, root string) {
	t.Helper()

	writeParityFile(t, root, "putnami.workspace.json",
		`{"name":"sdd-parity","includes":["billing","shipping"]}`)
	writeParityFile(t, root, "billing/putnami.json",
		`{"name":"@acme/billing","type":"application","dependencies":["@acme/shipping"]}`)
	writeParityFile(t, root, "shipping/putnami.json",
		`{"name":"@acme/shipping","type":"library"}`)

	writeParityFile(t, root, "billing/putnami.features.json", `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	writeParityFile(t, root, "shipping/putnami.features.json", `{
  "protocolVersion": 1,
  "namespace": "shipping",
  "features": [{
    "id": "shipping/labels",
    "type": "feature",
    "name": "Shipping labels",
    "outcome": "Operators print shipping labels",
    "owner": "shipping",
    "target": "modeled"
  }]
}`)
	writeParityFile(t, root, "billing/specs/billing.invoice.json", `{
  "$schema": "https://putnami.dev/schemas/putnami-spec.json",
  "protocolVersion": 1,
  "feature": "billing/invoice",
  "outcomes": ["Customers export issued invoices"],
  "requirements": []
}`)

	// Two domains and one real cross-domain edge, so validate/snapshot/inspect
	// each have something to report and the observed-edge detector has a mapped
	// dependency to find.
	writeParityFile(t, root, "shipping/putnami.architecture.json", `{
  "protocolVersion": 1,
  "domain": "shipping",
  "owner": "shipping-team",
  "projects": ["/shipping"],
  "exports": [{
    "id": "shipping.label.v1",
    "version": 1,
    "status": "active",
    "description": "Stable shipping label reference.",
    "facts": [{"name":"id","authority":"shipping","classification":"internal","personalData":"none"}],
    "modes": ["reference"],
    "compatibility": {"strategy":"additive","minimumConsumerVersion":1}
  }],
  "imports": []
}`)
	writeParityFile(t, root, "billing/putnami.architecture.json", `{
  "protocolVersion": 1,
  "domain": "billing",
  "owner": "billing-team",
  "projects": ["/billing"],
  "exports": [],
  "imports": [{
    "id": "billing.shipping-label.v1",
    "version": 1,
    "from": {"domain":"shipping","export":"shipping.label.v1"},
    "as": "billing.shipping-label",
    "mode": "reference",
    "status": "active",
    "facts": ["id"],
    "justification": "Billing stores the authoritative shipping label identity only",
    "bindings": [{"kind":"project-dependency","consumerProject":"/billing","producerProject":"/shipping"}]
  }]
}`)

	// An EMPTY frozen baseline, so `--baseline <ref>` has something to resolve
	// against and its presence is visible in the answer. Without the file the
	// flag is inert, and a parity case that passed it would prove nothing.
	writeParityFile(t, root, "architecture.baseline.json", `{
  "protocolVersion": 1,
  "findings": []
}`)

	writeParityFile(t, root, "billing/schema/contracts.json", `{
  "protocolVersion": 1,
  "name": "go.putnami.dev/acme/billing",
  "enums": [
    { "name": "Currency", "values": [ { "name": "USD", "value": "usd" } ] }
  ],
  "scopes": [ { "name": "billing:read" } ],
  "capabilities": [ { "name": "readInvoices", "scopes": ["billing:read"] } ],
  "grants": [ { "name": "billingReader", "capability": "readInvoices" } ],
  "claims": [ { "name": "sub", "type": "string", "required": true } ]
}`)

	commitParityWorkspace(t, root)
}

func writeParityFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// parityContractArtifacts are the files `contracts generate` writes for the
// fixture's one contract-bearing project, in the order the report names them.
// The authored manifest is first: it is an INPUT, and recording it beside the
// three generated files is what makes the recorded set restorable as a whole.
var parityContractArtifacts = []string{
	"billing/schema/contracts.json",
	"billing/schema/contracts.gen.go",
	"billing/schema/contracts.schema.json",
	"billing/schema/contracts.md",
}

// generateParityContracts materializes the committed contract artifacts so a
// following `contracts check` has something to compare against.
//
// It RESTORES the bytes core's `contracts generate` wrote rather than running
// the extension's own generator. That is the difference between two assertions:
// a check against self-generated artifacts passes whatever the generator
// renders, while a check against core's artifacts fails the moment the
// extension's renderer drifts by one byte from what users have committed.
func generateParityContracts(t *testing.T, root string) {
	t.Helper()
	for _, rel := range parityContractArtifacts {
		data, err := os.ReadFile(filepath.Join(recordedParityDir(), "contracts", recordedContractName(rel)))
		if err != nil {
			t.Fatalf("read the recorded contract artifact %s: %v", rel, err)
		}
		writeParityFile(t, root, rel, string(data))
	}
}

// recordedContractName is the flat file name one artifact was recorded under.
func recordedContractName(rel string) string {
	return strings.ReplaceAll(rel, "/", "_")
}

// TestContractsGenerateWritesTheRecordedArtifactBytes closes the gap the report
// comparison leaves open: `contracts generate`'s ENVELOPE only names the four
// artifacts, so an extension that wrote different bytes into them would still
// match the recorded report. The artifacts are what land in a user's repository,
// so they are compared directly.
func TestContractsGenerateWritesTheRecordedArtifactBytes(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	root := parityWorkspace(t)
	extensions := parityExtensions(t, root, parityExtensionRuntime(t))

	if _, errOut, code := runParityExtension(t, root, extensions,
		[]string{"contracts", "generate", "--project", "@acme/billing", "--output=json"}); code != 0 {
		t.Fatalf("contracts generate exited %d: %s", code, errOut)
	}

	for _, rel := range parityContractArtifacts {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read the generated %s: %v", rel, err)
		}
		want, err := os.ReadFile(filepath.Join(recordedParityDir(), "contracts", recordedContractName(rel)))
		if err != nil {
			t.Fatalf("read the recorded %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the bytes the built-in generated", rel)
		}
	}
}

// recordedParityDir is the root of the frozen answers.
func recordedParityDir() string { return filepath.Join("testdata", "sdd-parity") }

// recordedParityAnswer is one frozen invocation: what the built-in wrote and
// how it exited.
type recordedParityAnswer struct {
	exit   int
	stdout string
	stderr string
}

// The two section markers a recording is framed with. The capture asserted
// neither appears inside a captured stream, and readRecordedParityAnswer fails
// on a file that does not carry exactly one of each, so a recording that was
// hand-edited into an ambiguous shape is a failure rather than a silent
// truncation.
const (
	recordedStdoutMarker = "--- stdout ---\n"
	recordedStderrMarker = "--- stderr ---\n"
)

func readRecordedParityAnswer(t *testing.T, name string) recordedParityAnswer {
	t.Helper()
	path := filepath.Join(recordedParityDir(), filepath.FromSlash(name)+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the recorded answer %s: %v\n"+
			"There is no regenerating this file: the built-in that produced it was removed. "+
			"A missing recording means a case was added without recording what the built-in answered, "+
			"which this acceptance cannot evaluate.", path, err)
	}
	body := string(data)

	head, rest, found := strings.Cut(body, recordedStdoutMarker)
	if !found {
		t.Fatalf("%s carries no %q section", path, strings.TrimSuffix(recordedStdoutMarker, "\n"))
	}
	stdout, stderr, found := strings.Cut(rest, recordedStderrMarker)
	if !found {
		t.Fatalf("%s carries no %q section", path, strings.TrimSuffix(recordedStderrMarker, "\n"))
	}
	if strings.Contains(stderr, recordedStdoutMarker) || strings.Contains(stderr, recordedStderrMarker) {
		t.Fatalf("%s repeats a section marker, so its framing is ambiguous", path)
	}

	exit, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "exit")))
	if err != nil {
		t.Fatalf("%s does not start with an `exit <code>` line: %v", path, err)
	}
	return recordedParityAnswer{exit: exit, stdout: stdout, stderr: stderr}
}

// TestRecordedParityAnswersCoverEveryCaseAndNothingElse keeps the recording and
// the tables in step, in both directions.
//
// A case with no recording fails its own subtest already. A RECORDING WITH NO
// CASE is the failure this adds: it is a frozen answer nothing compares against
// any more, which is how an acceptance quietly shrinks while every test still
// passes. It also proves paritySlug is injective over these tables — two cases
// colliding on one file would show up here as a missing recording.
func TestRecordedParityAnswersCoverEveryCaseAndNothingElse(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/sdd-extraction", "recorded-parity-with-the-replaced-surface", "the-recording-covers-every-case-and-nothing-else")
	expected := map[string]bool{}
	for _, sdd := range parityCases() {
		expected[filepath.Join("structured", paritySlug(sdd.name)+".txt")] = true
	}
	for _, args := range parityHumanCases() {
		expected[filepath.Join("human", paritySlug(strings.Join(args, " "))+".txt")] = true
	}
	for _, tool := range mcpParityCases() {
		expected[filepath.Join("mcp", paritySlug(tool.name)+".txt")] = true
	}
	for _, rel := range parityContractArtifacts {
		expected[filepath.Join("contracts", recordedContractName(rel))] = true
	}

	if want := len(parityCases()) + len(parityHumanCases()) + len(mcpParityCases()) + len(parityContractArtifacts); len(expected) != want {
		t.Fatalf("the tables name %d recordings but only %d distinct files: paritySlug collided", want, len(expected))
	}

	found := map[string]bool{}
	root := recordedParityDir()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found[rel] = true
		if !expected[rel] {
			t.Errorf("%s is a recorded built-in answer no case compares against — delete it, or restore the case it belongs to",
				filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for rel := range expected {
		if !found[rel] {
			t.Errorf("%s has no recorded built-in answer", filepath.ToSlash(rel))
		}
	}
	if len(found) == 0 {
		t.Fatal("no recorded answers were found at all, so every comparison above would have failed on read")
	}
}

// paritySlug turns a case name into the file name that holds its recorded
// answer. It is injective over the tables in this file, which
// TestRecordedParityAnswersCoverEveryCase proves by counting.
func paritySlug(name string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, name)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return strings.Trim(slug, "-")
}

// normalizeParityBytes replaces the fixture workspace's absolute root, which is
// a per-run temporary directory, with a stable token.
//
// It is the ONLY normalization applied. The commit SHA every answer carries is
// deterministic by construction (parityCommitDate), so it is compared verbatim:
// a normalized revision would let an answer about the wrong tree compare equal.
func normalizeParityBytes(body, root string) string {
	if root == "" {
		return body
	}
	return strings.ReplaceAll(body, root, parityRootToken)
}

const parityRootToken = "{{WORKSPACE_ROOT}}"

// driftParityContracts corrupts one GENERATED artifact — never the authored
// manifest — so `check` reports drift and exits 2, which is the exit-code
// contract D2 keeps interactive. Both runs see the same corrupted tree because
// `check` writes nothing.
func driftParityContracts(t *testing.T, root string) {
	t.Helper()
	drifted := filepath.Join(root, "billing", "schema", "contracts.gen.go")
	if _, err := os.Stat(drifted); err != nil {
		t.Fatalf("no generated contract artifact to drift: %v", err)
	}
	if err := os.WriteFile(drifted, []byte("// drifted\n"), 0o644); err != nil {
		t.Fatalf("drift %s: %v", drifted, err)
	}
}

// parityCommitDate pins the fixture commit's author and committer timestamps.
//
// A git commit's SHA is a hash of its tree, parents, message, and the two
// identity+timestamp lines. Everything but the timestamps was already fixed, so
// pinning these makes the fixture repository's HEAD the SAME 40 hex digits on
// every machine and every run — which is what lets the recorded answers below
// carry the revision they were produced under verbatim, instead of being
// normalized past the one field that proves which tree they describe.
const parityCommitDate = "2026-01-01T00:00:00+00:00"

// commitParityWorkspace makes the fixture a git repository with one commit, so
// `features diff` and `contracts check`'s prior-IR loader have history to read.
func commitParityWorkspace(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "parity@example.test"},
		{"config", "user.name", "Parity Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-m", "sdd parity fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_DATE="+parityCommitDate, "GIT_COMMITTER_DATE="+parityCommitDate)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestParityCasesCoverTheWholeInventory keeps the table above honest: a
// subcommand missing from it would make this file's acceptance a partial one
// while still passing.
func TestParityCasesCoverTheWholeInventory(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/sdd-extraction", "recorded-parity-with-the-replaced-surface", "the-parity-cases-cover-the-whole-inventory")
	inventory := map[string][]string{
		"features":     {"list", "validate", "snapshot", "inspect", "diff"},
		"specs":        {"list", "validate", "inspect", "init"},
		"architecture": {"validate", "snapshot", "inspect"},
		"contracts":    {"generate", "check"},
	}
	covered := map[string]bool{}
	for _, sdd := range parityCases() {
		if len(sdd.args) >= 2 {
			covered[sdd.args[0]+" "+sdd.args[1]] = true
		}
	}
	for group, subs := range inventory {
		for _, sub := range subs {
			if !covered[group+" "+sub] {
				t.Errorf("%s %s is in the inventory and in no parity case", group, sub)
			}
		}
	}
}

// TestBaselineActuallyReachesTheExtension keeps the `--baseline` parity case
// from being vacuous.
//
// `--baseline` is a global the CLI consumes before dispatch and the resolved
// `selection` block cannot carry unless `--impacted` used it, so it reaches the
// extension only through the param the interactive path forwards. If that
// forwarding broke, `architecture validate --baseline HEAD` would silently
// become `architecture validate` — and the byte comparison would still pass,
// because BOTH sides would be compared against each other and not against a
// baseline. This asserts the flag CHANGES the answer.
func TestBaselineActuallyReachesTheExtension(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	spectest.Proves(t, "cli/sdd-extraction", "recorded-parity-with-the-replaced-surface", "the-baseline-actually-reaches-the-extension")
	root := parityWorkspace(t)
	extensions := parityExtensions(t, root, parityExtensionRuntime(t))

	// An empty answer is a failed run, not a flag that changed nothing: the
	// failure reports its exit code, where -1 is a child that a signal ended,
	// and its stderr.
	answer := func(args ...string) string {
		t.Helper()
		out, errOut, code := runParityExtension(t, root, extensions, args)
		if out == "" {
			t.Fatalf("%v wrote no answer and exited %d:\n%s", args, code, errOut)
		}
		return out
	}
	without := answer("architecture", "validate", "--output=json")
	with := answer("architecture", "validate", "--baseline", "HEAD", "--output=json")

	if with == without {
		t.Fatal("--baseline changed nothing; the flag never reached the extension")
	}
	if !strings.Contains(with, `"requested": "HEAD"`) {
		t.Errorf("the baseline the user asked for is not in the answer:\n%s", with)
	}
	if strings.Contains(without, `"requested": "HEAD"`) {
		t.Errorf("a run with no --baseline reported one:\n%s", without)
	}
}

// TestParityEnvelopesAreWellFormedResults guards against the failure mode a
// byte comparison cannot see: two identical WRONG answers.
//
// If both sides degraded to an empty stdout, every case above would pass. This
// decodes one structured answer per group and asserts it is a v2 result
// envelope naming the command that produced it.
func TestParityEnvelopesAreWellFormedResults(t *testing.T) {
	t.Parallel()
	limitParityConcurrency(t)
	root := parityWorkspace(t)
	extensions := parityExtensions(t, root, parityExtensionRuntime(t))

	for _, args := range [][]string{
		{"features", "list", "--output=json"},
		{"specs", "list", "--output=json"},
		{"architecture", "snapshot", "--output=json"},
		{"contracts", "generate", "--project", "@acme/billing", "--output=json"},
	} {
		out, _, code := runParityExtension(t, root, extensions, args)
		if code != 0 {
			t.Errorf("%v exited %d:\n%s", args, code, out)
			continue
		}
		var envelope struct {
			ProtocolVersion int    `json:"protocolVersion"`
			Command         string `json:"command"`
			Status          string `json:"status"`
			Data            any    `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil {
			t.Errorf("%v did not write one JSON document: %v\n%s", args, err, out)
			continue
		}
		if want := args[0] + " " + args[1]; envelope.Command != want {
			t.Errorf("%v envelope command = %q, want %q", args, envelope.Command, want)
		}
		if envelope.Status != "success" || envelope.ProtocolVersion == 0 || envelope.Data == nil {
			t.Errorf("%v envelope = %+v, want a populated v2 success", args, envelope)
		}
	}
}
