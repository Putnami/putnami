package cli

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The parser rewrite must not move a
// single job cache key or run marker for a currently-valid invocation.
//
// buildCommandParams turns the tokens ParseArgs did not consume into the
// untyped param map that reaches TWO hashes with DIFFERENT sensitivity:
//
//   - internal/store's hashParams (via CacheKey.ComputeHashUsing, exercised
//     here through the production path) sorts every parameter its caller has
//     projected and hashes "key \0 json.Marshal(value) \0" per key. The jobs
//     package limits that map to task and owning-command inputs before reaching
//     the store.
//   - workspace_state.LastBuildParamsHash json.Marshals the WHOLE map — it does
//     not filter volatile params — and keeps 12 hex chars. It feeds the
//     successful-run markers that drive last-build SHA and remote skip-hit
//     decisions.
//
// Because both hash through encoding/json, the Go TYPE of each value is part of
// the hash: params["retry"] = "3" and params["retry"] = 3 are different keys.
// Preserving the key SET is therefore not enough; the corpus pins the rendered
// type of every value as well.
//
// The expectations below are literals computed outside the code they guard,
// never regenerated from it: a pin recomputed from the code it guards
// rubber-stamps its own drift. A row that moves is a repo-wide cold-cache event
// plus run-marker mismatches, with no error anywhere.
//
// wantParams and wantMarker pin the parser seam, and nothing moves them.
// wantCacheKey pins the current cache-key format against accidental movement,
// and it is the only column a format change may touch: a format change is an
// intentional repo-wide cold-cache event, and it remaps that column to values
// computed independently of the new code.

// keyStabilityCase is one currently-valid invocation and the exact hashes its
// leftover tokens must keep producing.
type keyStabilityCase struct {
	name            string
	args            []string
	userAliases     map[string]string
	extensionGroups map[string]bool

	// wantParams renders the params map as sorted "name=GoType(value)" entries,
	// so a value that changes type fails with a readable diff rather than an
	// opaque hash mismatch.
	wantParams string
	// wantCacheKey is the full job cache key computed through the production
	// CacheKey hasher over a fixed non-params skeleton, so any change to the
	// params contribution moves it.
	wantCacheKey string
	// wantMarker is workspace_state.LastBuildParamsHash of the same map.
	wantMarker string
}

// keyStabilityCorpus covers every argument SHAPE the parser can hand to
// buildCommandParams: bare root tasks, comma commands, builtin and user
// aliases, the "." selector, --projects, global flags that must never reach
// params, --flag=value, --no-flag, --flag value, bare --flag, the "--"
// passthrough separator, extension flag passthrough, and a value that begins
// with a hyphen.
var keyStabilityCorpus = []keyStabilityCase{
	{
		name:         "bare root task",
		args:         []string{"build"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "root task with a positional project selector",
		args:         []string{"build", "@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "root task with the dot selector",
		args:         []string{"build", "."},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "explicit --projects never reaches params",
		args:         []string{"build", "--projects", "@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "comma commands with a builtin alias",
		args:         []string{"l,t,b", "--impacted"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "user alias resolving through a builtin alias",
		args:         []string{"ci", "--all"},
		userAliases:  map[string]string{"ci": "b"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "global execution flags are consumed, never params",
		args:         []string{"build", "--max-parallel", "4", "--retry", "2", "--output", "jsonl", "--no-cache"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "job flag with a separate value is a string",
		args:         []string{"build", "--target", "linux/amd64"},
		wantParams:   "target=string(linux/amd64)",
		wantCacheKey: "af5985fe1ebef667543fc8a27a55fb037eeca3f5ffd4cbe47290ed0eec06f3fe",
		wantMarker:   "dee4bcd7033c",
	},
	{
		name:         "job flag with an inline value is a string",
		args:         []string{"build", "--target=linux/amd64"},
		wantParams:   "target=string(linux/amd64)",
		wantCacheKey: "af5985fe1ebef667543fc8a27a55fb037eeca3f5ffd4cbe47290ed0eec06f3fe",
		wantMarker:   "dee4bcd7033c",
	},
	{
		name:         "bare job flag is bool true",
		args:         []string{"build", "--minify"},
		wantParams:   "minify=bool(true)",
		wantCacheKey: "6362f4b68216a7f5958d1f130c44c261b452c1cd1d65e8ac76855f1cca6d06c3",
		wantMarker:   "190e6b9bab6a",
	},
	{
		name:         "job flag followed by another flag is bool true",
		args:         []string{"build", "--minify", "--sourcemap"},
		wantParams:   "minify=bool(true) sourcemap=bool(true)",
		wantCacheKey: "00d395cabab9b2beffce5e56dbb8580538ac1f14f7ed4f3a51f2c2dfb89fccc0",
		wantMarker:   "6f0eb87789a5",
	},
	{
		name:         "--no-flag is bool false",
		args:         []string{"build", "--no-minify"},
		wantParams:   "minify=bool(false)",
		wantCacheKey: "31506be2cbf60d50530c01420d8c98585f1be6ae0ebc84776fecd57e998065ef",
		wantMarker:   "87c1fa2fec66",
	},
	{
		name:         "single-dash no- form is stripped to the same name",
		args:         []string{"build", "-no-minify"},
		wantParams:   "minify=bool(false)",
		wantCacheKey: "31506be2cbf60d50530c01420d8c98585f1be6ae0ebc84776fecd57e998065ef",
		wantMarker:   "87c1fa2fec66",
	},
	{
		name:         "numeric job flag value stays a string",
		args:         []string{"build", "--workers", "8"},
		wantParams:   "workers=string(8)",
		wantCacheKey: "dbed8c507584763ae336347e90d6ed382d2c9d102f5de8931813c2541c80e4a2",
		wantMarker:   "888d52d60695",
	},
	{
		name:         "mixed job flag shapes",
		args:         []string{"build", "--target", "linux/amd64", "--minify", "--mode=release", "--no-sourcemap"},
		wantParams:   "minify=bool(true) mode=string(release) sourcemap=bool(false) target=string(linux/amd64)",
		wantCacheKey: "e464f32e7260b3e2b31517332b6ff3c6ec0ddc8c2b4b3b0758a7256c6b2269a9",
		wantMarker:   "cc794bb26d54",
	},
	{
		name:         "positional selector plus job flags",
		args:         []string{"build", "@putnami/cli", "--target", "wasm"},
		wantParams:   "target=string(wasm)",
		wantCacheKey: "d662e89611ee37636fe75df98a2354a7c0fbf843bd89d9506d3e3934a35c30c8",
		wantMarker:   "670b817f4333",
	},
	{
		name:         "comma commands carrying a job flag",
		args:         []string{"lint,test", ".", "--coverage"},
		wantParams:   "coverage=bool(true)",
		wantCacheKey: "ae2ee5f4b7f60e4e746f05908c06490eb280dfc5c1701662b607bab79f994a9d",
		wantMarker:   "4e0f808f956c",
	},
	{
		name:         "double dash passthrough",
		args:         []string{"run", "--", "--verbose", "arg"},
		wantParams:   "=string(arg)",
		wantCacheKey: "f9770592b9539ad73db382ee7a9b25dfbb594c0687483396c6423e8909dcac92",
		wantMarker:   "48a9f69878fc",
	},
	{
		name:         "double dash after a job flag",
		args:         []string{"test", "--filter", "unit", "--", "-race"},
		wantParams:   "=bool(true) filter=string(unit) race=bool(true)",
		wantCacheKey: "de18194a2d60b3d8abd1757237d2dc87eb8239cc697636ae4204546928ae6378",
		wantMarker:   "2c2723740f0d",
	},
	{
		name:            "extension command group flag passthrough",
		args:            []string{"cloud", "deploy", "--env", "prod", "--force"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=string(prod) force=bool(true)",
		wantCacheKey:    "fcab8cb9407c758d06a4c74de5a1e5716e21c84ebb16193c6ae4334fdfadfe65",
		wantMarker:      "52d6c5cfbe5d",
	},
	{
		name:            "extension command group with an inline value",
		args:            []string{"cloud", "deploy", "--env=prod"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=string(prod)",
		wantCacheKey:    "ee6f6d76e13e6035a3735a77bb9e3c947b284db55670c1d0ec425b1669c4b516",
		wantMarker:      "fdf65bc0fcac",
	},
	{
		name:         "operational-looking params retain their typed identity",
		args:         []string{"build", "--verbose-report", "--dryRun"},
		wantParams:   "dryRun=bool(true) verbose-report=bool(true)",
		wantCacheKey: "d6f56c9e045c0b1f91dca2104ee1e72f7077ff99524ac9be37e2fdb681578baa",
		wantMarker:   "87cc12d18645",
	},
	{
		name:         "inline global value form never reaches params",
		args:         []string{"build", "--projects=@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "0d16b1b62a97e3fd62d4c6779bdf7f39d64f5b7d04a88ba1dd83e67d4139b918",
		wantMarker:   "",
	},
	{
		name:         "operational-looking param reaches both raw hashers",
		args:         []string{"build", "--dryRun"},
		wantParams:   "dryRun=bool(true)",
		wantCacheKey: "04f09a16634aae4c3dfd78535051e19d9e722d1771be7e2233c344f99a004f4d",
		wantMarker:   "e59f1698ce59",
	},
	{
		name:         "a multi-word value that begins with a hyphen binds whole",
		args:         []string{"run", "--args", "--check --dry-run"},
		wantParams:   "args=string(--check --dry-run)",
		wantCacheKey: "f092ffae6d668762e8d755c762eb5250042c43f2ff1df74638f8cd12ac631ed4",
		wantMarker:   "eae433231770",
	},
	{
		name:         "the inline form of a multi-word hyphen value binds the same",
		args:         []string{"run", "--args=--check --dry-run"},
		wantParams:   "args=string(--check --dry-run)",
		wantCacheKey: "f092ffae6d668762e8d755c762eb5250042c43f2ff1df74638f8cd12ac631ed4",
		wantMarker:   "eae433231770",
	},
	{
		name:         "an inline one-word hyphen value binds whole",
		args:         []string{"run", "--args=--gate"},
		wantParams:   "args=string(--gate)",
		wantCacheKey: "ee9e995a11b9178c83e6b28969d54c986d7754105f538d3b11957c8f72450eaf",
		wantMarker:   "5642f875a0fc",
	},
	{
		name:         "a separate one-word hyphen value is a flag of its own",
		args:         []string{"run", "--args", "--gate"},
		wantParams:   "args=bool(true) gate=bool(true)",
		wantCacheKey: "a7070568c1196a545eab0531d140bc30f951a02bb6e007c967a3d094c5142cf5",
		wantMarker:   "a327c9b0f7eb",
	},
	{
		name:         "an inline negative number binds whole",
		args:         []string{"run", "--port=-1"},
		wantParams:   "port=string(-1)",
		wantCacheKey: "77dff1dd0b27afd0191e6ce5757bcbcccff903bd5d21e77605400c0fc860c3c2",
		wantMarker:   "721abc4ea711",
	},
	{
		name:         "an inline multi-word value holding = binds whole",
		args:         []string{"run", "--args=--port=3000 --watch"},
		wantParams:   "args=string(--port=3000 --watch)",
		wantCacheKey: "868fe2f3ed6c5f8555096f7eea11b188223a96d3cdc6550d7c6a82a259dde356",
		wantMarker:   "1232a47ad056",
	},
	{
		name:         "an inline one-word value holding = binds whole",
		args:         []string{"run", "--args=--port=3000"},
		wantParams:   "args=string(--port=3000)",
		wantCacheKey: "a7b4c150eb697f56e2475a5c2e939e6f5264ec27546ca17d9392ff17b6549722",
		wantMarker:   "1643d9d07175",
	},
	{
		name:         "a separate value with = after its first word binds whole",
		args:         []string{"run", "--args", "--watch --port=3000"},
		wantParams:   "args=string(--watch --port=3000)",
		wantCacheKey: "ade602de2fbe91a2f884586cc6b8d7f7c13e588f5c981cb42b27bd42bc5b7c15",
		wantMarker:   "33c023eb3d22",
	},
	{
		name:         "a separate value with = in its first word is a flag with an inline value",
		args:         []string{"run", "--args", "--port=3000 --watch"},
		wantParams:   "args=bool(true) port=string(3000 --watch)",
		wantCacheKey: "20c13a7ba35682260c81085a134782e5f8fc32237473f4742f613fa1450c49bd",
		wantMarker:   "c0ca325b1cc5",
	},
	{
		name:         "a bare switch before a flag stays a switch",
		args:         []string{"test", "--concurrent", "--update-snapshots"},
		wantParams:   "concurrent=bool(true) update-snapshots=bool(true)",
		wantCacheKey: "acddf2c5f47b818cd3bbe1633de883550f7644ed416c0e038aa34773b3fdef60",
		wantMarker:   "51a7cf25a00f",
	},
	{
		name:         "a bare switch before a negation stays a switch",
		args:         []string{"test", "--parallel", "--no-enforce-coverage"},
		wantParams:   "enforce-coverage=bool(false) parallel=bool(true)",
		wantCacheKey: "bc9634a686a1d59b25faa13118413f23aee965579c7c5b1b4219f68bba5050f6",
		wantMarker:   "99774122259e",
	},
	{
		name:         "a bare string-typed switch before a flag stays a switch",
		args:         []string{"build", "--sourcemap", "--minify"},
		wantParams:   "minify=bool(true) sourcemap=bool(true)",
		wantCacheKey: "00d395cabab9b2beffce5e56dbb8580538ac1f14f7ed4f3a51f2c2dfb89fccc0",
		wantMarker:   "6f0eb87789a5",
	},
	{
		name:         "a switch before an inline multi-word value stays a switch",
		args:         []string{"test", "--coverage", "--filter=a b"},
		wantParams:   "coverage=bool(true) filter=string(a b)",
		wantCacheKey: "b818181ad4e1fac64826543d2b8ad22f4fac358f150d7ddce41c4f8030da2a56",
		wantMarker:   "2cba3a43d79e",
	},
	{
		name:         "a switch before an inline multi-word hyphen value stays a switch",
		args:         []string{"test", "--coverage", "--args=--check --dry-run"},
		wantParams:   "args=string(--check --dry-run) coverage=bool(true)",
		wantCacheKey: "1b65080da353c466fb35be14be579ebb9da8c667ec8a0ae7a2794117cd1e819b",
		wantMarker:   "fd1413125a90",
	},
	{
		name:         "past the separator an inline hyphen word splits",
		args:         []string{"test", "--", "--args=--gate"},
		wantParams:   "=bool(true) args=bool(true) gate=bool(true)",
		wantCacheKey: "72d78d0c5cf39bf70a07085b7dd8af72d0bb40dd7916527e16e213a6408fddfa",
		wantMarker:   "66a1e0f43e51",
	},
	{
		name:         "past the separator a multi-word hyphen token is a value",
		args:         []string{"test", "--", "--args", "--check --dry-run"},
		wantParams:   "=bool(true) args=string(--check --dry-run)",
		wantCacheKey: "1c8dd58ab71ee9a8896014f4c1ee9bf8a3e92d831ad790b76275c62c2c70847c",
		wantMarker:   "9dc20478bd52",
	},
	{
		name:            "an extension command group splits an inline hyphen word",
		args:            []string{"cloud", "deploy", "--env=-x"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=bool(true) x=bool(true)",
		wantCacheKey:    "da1f5d9fecb810f0bfc37e90809f06d96aff037600df528a1b968eba7f49dcec",
		wantMarker:      "9b569eaddf37",
	},
	{
		name:            "an extension command group binds a multi-word hyphen value whole",
		args:            []string{"cloud", "deploy", "--args", "--a --b"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "args=string(--a --b)",
		wantCacheKey:    "b24eb523aabeafc88f1e415b546348aacad05f6cf2e60f22fa987da742b65d5e",
		wantMarker:      "7d30d7f1a8b1",
	},
}

// corpusHashes is the single derivation point: it parses an invocation exactly
// as the CLI does, builds the command params from the leftover tokens, and
// returns the three pinned observations.
//
// rendered lists the params as sorted "name=GoType(value)" entries, because
// encoding/json — and therefore both hashes — treats "true" and true as
// different values, so a type change must fail with a readable diff rather than
// an opaque hash mismatch.
//
// cacheKey runs through the production CacheKey hasher (→ hashParams) over a
// fixed non-params skeleton rather than a test-local copy of the hashing rules.
// No file patterns are set, so no I/O happens.
func corpusHashes(t *testing.T, tc keyStabilityCase) (rendered, cacheKey, marker string) {
	t.Helper()

	parsed := ParseArgs(tc.args, tc.userAliases, tc.extensionGroups)
	if parsed.Err != nil {
		t.Fatalf("ParseArgs(%v) rejected a currently-valid invocation: %v", tc.args, parsed.Err)
	}
	params := buildCommandParams(parsed.RawJobArgs)

	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf("%s=%T(%v)", name, params[name], params[name]))
	}

	key := store.CacheKey{
		Extension: "@putnami/corpus",
		Task:      "build",
		Project:   "@putnami/cli",
		Params:    params,
	}
	hash, err := key.ComputeHashUsing(store.NewCacheManager(nil))
	if err != nil {
		t.Fatalf("compute cache key: %v", err)
	}
	return strings.Join(entries, " "), hash, workspace_state.LastBuildParamsHash(params)
}

func TestParseKeyStability_ParamsAndHashesAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range keyStabilityCorpus {
		t.Run(tc.name, func(t *testing.T) {
			rendered, cacheKey, marker := corpusHashes(t, tc)
			if rendered != tc.wantParams {
				t.Errorf("params = %q, want %q", rendered, tc.wantParams)
			}
			if cacheKey != tc.wantCacheKey {
				t.Errorf("cache key = %q, want %q", cacheKey, tc.wantCacheKey)
			}
			if marker != tc.wantMarker {
				t.Errorf("run-marker params hash = %q, want %q", marker, tc.wantMarker)
			}
		})
	}
}
