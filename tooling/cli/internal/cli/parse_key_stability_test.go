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
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "root task with a positional project selector",
		args:         []string{"build", "@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "root task with the dot selector",
		args:         []string{"build", "."},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "explicit --projects never reaches params",
		args:         []string{"build", "--projects", "@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "comma commands with a builtin alias",
		args:         []string{"l,t,b", "--impacted"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "user alias resolving through a builtin alias",
		args:         []string{"ci", "--all"},
		userAliases:  map[string]string{"ci": "b"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "global execution flags are consumed, never params",
		args:         []string{"build", "--max-parallel", "4", "--retry", "2", "--output", "jsonl", "--no-cache"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "job flag with a separate value is a string",
		args:         []string{"build", "--target", "linux/amd64"},
		wantParams:   "target=string(linux/amd64)",
		wantCacheKey: "af3d2a624d77fa282d83851c95ed6fa51e7c90442b01466776e31f82aeb4afb6",
		wantMarker:   "dee4bcd7033c",
	},
	{
		name:         "job flag with an inline value is a string",
		args:         []string{"build", "--target=linux/amd64"},
		wantParams:   "target=string(linux/amd64)",
		wantCacheKey: "af3d2a624d77fa282d83851c95ed6fa51e7c90442b01466776e31f82aeb4afb6",
		wantMarker:   "dee4bcd7033c",
	},
	{
		name:         "bare job flag is bool true",
		args:         []string{"build", "--minify"},
		wantParams:   "minify=bool(true)",
		wantCacheKey: "13b9d1d9b901f5c3f6150301ba312dfc0a69602fed72fc94dfe5ce44f3f46e6f",
		wantMarker:   "190e6b9bab6a",
	},
	{
		name:         "job flag followed by another flag is bool true",
		args:         []string{"build", "--minify", "--sourcemap"},
		wantParams:   "minify=bool(true) sourcemap=bool(true)",
		wantCacheKey: "311ac78e9d35c8b0f56012da3324fc64c6e7ee206141f2435d99998e07d8985c",
		wantMarker:   "6f0eb87789a5",
	},
	{
		name:         "--no-flag is bool false",
		args:         []string{"build", "--no-minify"},
		wantParams:   "minify=bool(false)",
		wantCacheKey: "8a93243accc2028ec406a25027dd8d4915fadbe13f663df4b8746325b6f83113",
		wantMarker:   "87c1fa2fec66",
	},
	{
		name:         "single-dash no- form is stripped to the same name",
		args:         []string{"build", "-no-minify"},
		wantParams:   "minify=bool(false)",
		wantCacheKey: "8a93243accc2028ec406a25027dd8d4915fadbe13f663df4b8746325b6f83113",
		wantMarker:   "87c1fa2fec66",
	},
	{
		name:         "numeric job flag value stays a string",
		args:         []string{"build", "--workers", "8"},
		wantParams:   "workers=string(8)",
		wantCacheKey: "eb35abbe4fc41c9b91940e622605cb0f2df033bc0f88e753bfa12bf574bb1143",
		wantMarker:   "888d52d60695",
	},
	{
		name:         "mixed job flag shapes",
		args:         []string{"build", "--target", "linux/amd64", "--minify", "--mode=release", "--no-sourcemap"},
		wantParams:   "minify=bool(true) mode=string(release) sourcemap=bool(false) target=string(linux/amd64)",
		wantCacheKey: "1a10c1ecf3cec2c5ce0ee38b9d13faeaf793480eb21f9523dcd02ffb5ff55e63",
		wantMarker:   "cc794bb26d54",
	},
	{
		name:         "positional selector plus job flags",
		args:         []string{"build", "@putnami/cli", "--target", "wasm"},
		wantParams:   "target=string(wasm)",
		wantCacheKey: "4f570d235d231003b96d4d13711ec910913c0286bc8212a4abba8c6ddcc8fa49",
		wantMarker:   "670b817f4333",
	},
	{
		name:         "comma commands carrying a job flag",
		args:         []string{"lint,test", ".", "--coverage"},
		wantParams:   "coverage=bool(true)",
		wantCacheKey: "c52251f5945d7188e98ec2441bb0b02610604f723c10f8e941236ffbb72335ab",
		wantMarker:   "4e0f808f956c",
	},
	{
		name:         "double dash passthrough",
		args:         []string{"run", "--", "--verbose", "arg"},
		wantParams:   "=string(arg)",
		wantCacheKey: "cd5617f240ca368d668cb30fa5bf3eb63b712a582e7fb7b5e2217d74c58c4f98",
		wantMarker:   "48a9f69878fc",
	},
	{
		name:         "double dash after a job flag",
		args:         []string{"test", "--filter", "unit", "--", "-race"},
		wantParams:   "=bool(true) filter=string(unit) race=bool(true)",
		wantCacheKey: "bd8c3298b801dda82405a54c5b99bcae9d66649302a937d7098c3cd93c9395ca",
		wantMarker:   "2c2723740f0d",
	},
	{
		name:            "extension command group flag passthrough",
		args:            []string{"cloud", "deploy", "--env", "prod", "--force"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=string(prod) force=bool(true)",
		wantCacheKey:    "83c951d1a60cb3ac2aae672d107de78d7e618f3c96b54851852effa2474ec2b3",
		wantMarker:      "52d6c5cfbe5d",
	},
	{
		name:            "extension command group with an inline value",
		args:            []string{"cloud", "deploy", "--env=prod"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=string(prod)",
		wantCacheKey:    "fba0014269afc3fff5b474f9b1cc45d2729a123beb79f40780ab84fbae859970",
		wantMarker:      "fdf65bc0fcac",
	},
	{
		name:         "operational-looking params retain their typed identity",
		args:         []string{"build", "--verbose-report", "--dryRun"},
		wantParams:   "dryRun=bool(true) verbose-report=bool(true)",
		wantCacheKey: "e90247ce1e84e43724f2deef8e567650b277f8a0fd52f2340f351c31088f119e",
		wantMarker:   "87cc12d18645",
	},
	{
		name:         "inline global value form never reaches params",
		args:         []string{"build", "--projects=@putnami/cli"},
		wantParams:   "",
		wantCacheKey: "5b877407b6b908a1bf29d507c1f48e1d7eeeae31fd904c161d81f08093ad305c",
		wantMarker:   "",
	},
	{
		name:         "operational-looking param reaches both raw hashers",
		args:         []string{"build", "--dryRun"},
		wantParams:   "dryRun=bool(true)",
		wantCacheKey: "2f6e339148ad636eb2d97b1dcc7c05d9c87910cf3b30def51d31677822c8cd75",
		wantMarker:   "e59f1698ce59",
	},
	{
		name:         "a multi-word value that begins with a hyphen binds whole",
		args:         []string{"run", "--args", "--check --dry-run"},
		wantParams:   "args=string(--check --dry-run)",
		wantCacheKey: "e6f5abd7cc8aeb7804a34caa8398099b8afdcf5156f044bc47ff7fa63f5bf67e",
		wantMarker:   "eae433231770",
	},
	{
		name:         "the inline form of a multi-word hyphen value binds the same",
		args:         []string{"run", "--args=--check --dry-run"},
		wantParams:   "args=string(--check --dry-run)",
		wantCacheKey: "e6f5abd7cc8aeb7804a34caa8398099b8afdcf5156f044bc47ff7fa63f5bf67e",
		wantMarker:   "eae433231770",
	},
	{
		name:         "an inline one-word hyphen value binds whole",
		args:         []string{"run", "--args=--gate"},
		wantParams:   "args=string(--gate)",
		wantCacheKey: "077236196e9099be6dfb24b4516e6ac88515348591f8d7ac755c275cb89db08f",
		wantMarker:   "5642f875a0fc",
	},
	{
		name:         "a separate one-word hyphen value is a flag of its own",
		args:         []string{"run", "--args", "--gate"},
		wantParams:   "args=bool(true) gate=bool(true)",
		wantCacheKey: "4cc0a321fa850b51c60ccf3efd7bba1729f27647f0a6e7aa852a34f323c8c806",
		wantMarker:   "a327c9b0f7eb",
	},
	{
		name:         "an inline negative number binds whole",
		args:         []string{"run", "--port=-1"},
		wantParams:   "port=string(-1)",
		wantCacheKey: "4613ee9de99a32fef16f298169b3d02dc7bfe9808441fbfa1f08bea340f8f172",
		wantMarker:   "721abc4ea711",
	},
	{
		name:         "an inline multi-word value holding = binds whole",
		args:         []string{"run", "--args=--port=3000 --watch"},
		wantParams:   "args=string(--port=3000 --watch)",
		wantCacheKey: "ac055dad6d9a06fae939f5d05256f0a60fa25410127b8acd4d08f7106be76ab7",
		wantMarker:   "1232a47ad056",
	},
	{
		name:         "an inline one-word value holding = binds whole",
		args:         []string{"run", "--args=--port=3000"},
		wantParams:   "args=string(--port=3000)",
		wantCacheKey: "c13f36c740eeae11b2165e60b366a53cc994c958327ec94fba5db15c40c213cf",
		wantMarker:   "1643d9d07175",
	},
	{
		name:         "a separate value with = after its first word binds whole",
		args:         []string{"run", "--args", "--watch --port=3000"},
		wantParams:   "args=string(--watch --port=3000)",
		wantCacheKey: "c10d8c2bf29f3c8e1f8251626c7610f4eb766306dd105af4410cb459b80878af",
		wantMarker:   "33c023eb3d22",
	},
	{
		name:         "a separate value with = in its first word is a flag with an inline value",
		args:         []string{"run", "--args", "--port=3000 --watch"},
		wantParams:   "args=bool(true) port=string(3000 --watch)",
		wantCacheKey: "87c234f242663600d2a9ba3ab4d04d24795c6c5068d4f745cd3eef03f8f3992e",
		wantMarker:   "c0ca325b1cc5",
	},
	{
		name:         "a bare switch before a flag stays a switch",
		args:         []string{"test", "--concurrent", "--update-snapshots"},
		wantParams:   "concurrent=bool(true) update-snapshots=bool(true)",
		wantCacheKey: "38c9e23ad932f770cdce4fd671cf9e67de81dbd577cc08b503f0fbc3fc437e0b",
		wantMarker:   "51a7cf25a00f",
	},
	{
		name:         "a bare switch before a negation stays a switch",
		args:         []string{"test", "--parallel", "--no-enforce-coverage"},
		wantParams:   "enforce-coverage=bool(false) parallel=bool(true)",
		wantCacheKey: "f9012915d4bc03b4b6d6786be3c124c9d1496efbd8bdbf3fcb9a4ab5dea115df",
		wantMarker:   "99774122259e",
	},
	{
		name:         "a bare string-typed switch before a flag stays a switch",
		args:         []string{"build", "--sourcemap", "--minify"},
		wantParams:   "minify=bool(true) sourcemap=bool(true)",
		wantCacheKey: "311ac78e9d35c8b0f56012da3324fc64c6e7ee206141f2435d99998e07d8985c",
		wantMarker:   "6f0eb87789a5",
	},
	{
		name:         "a switch before an inline multi-word value stays a switch",
		args:         []string{"test", "--coverage", "--filter=a b"},
		wantParams:   "coverage=bool(true) filter=string(a b)",
		wantCacheKey: "d075c530fd6982a32f0db179db4676b2247b690e45be49ec6f3a55981b5813db",
		wantMarker:   "2cba3a43d79e",
	},
	{
		name:         "a switch before an inline multi-word hyphen value stays a switch",
		args:         []string{"test", "--coverage", "--args=--check --dry-run"},
		wantParams:   "args=string(--check --dry-run) coverage=bool(true)",
		wantCacheKey: "ce5320d0e43060e46de75cba5e13c73145a130900ea189d07151228a9bde9a71",
		wantMarker:   "fd1413125a90",
	},
	{
		name:         "past the separator an inline hyphen word splits",
		args:         []string{"test", "--", "--args=--gate"},
		wantParams:   "=bool(true) args=bool(true) gate=bool(true)",
		wantCacheKey: "d67303273bad01479ab08b99f166686645d1ba4170ed5a202838d3221c40c030",
		wantMarker:   "66a1e0f43e51",
	},
	{
		name:         "past the separator a multi-word hyphen token is a value",
		args:         []string{"test", "--", "--args", "--check --dry-run"},
		wantParams:   "=bool(true) args=string(--check --dry-run)",
		wantCacheKey: "98247031bf581ddbb30f768ca2d5ebe58b645f0b0ee29e122b67e3a27c26a6a0",
		wantMarker:   "9dc20478bd52",
	},
	{
		name:            "an extension command group splits an inline hyphen word",
		args:            []string{"cloud", "deploy", "--env=-x"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "env=bool(true) x=bool(true)",
		wantCacheKey:    "9ad3aca55f159fccc2c4e44ebc2301b01b796162898964d9c37785d0d5c0ad37",
		wantMarker:      "9b569eaddf37",
	},
	{
		name:            "an extension command group binds a multi-word hyphen value whole",
		args:            []string{"cloud", "deploy", "--args", "--a --b"},
		extensionGroups: map[string]bool{"cloud": true},
		wantParams:      "args=string(--a --b)",
		wantCacheKey:    "193d2ccca480919abd56b33ce890a663e1c4a5e4b43e07ac0666259fc0990546",
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
