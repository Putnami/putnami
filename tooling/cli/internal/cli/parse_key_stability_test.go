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
// The three expectations below were computed from the earlier implementation
// and pasted in as literals. They are not regenerated from the new parser: a
// pin recomputed from the code it guards rubber-stamps its own drift. A row
// that moves is a repo-wide cold-cache event plus run-marker mismatches, with
// no error anywhere — the failure mode this repo has shipped twice.
//
// Deliberate cache-format movements since: an earlier change bumped to v5 for
// task-contract digests, a later change bumped to v6 for the
// implementation digest of a workspace-local direct extension, and a further
// change bumps to v7 for the workspace-probe project metadata
// digest, and a further change bumps to v8 because a key that carries an
// extension implementation digest no longer carries the extension version.
// Each is an intentional repo-wide cold-cache event. The wantCacheKey
// column is remapped to v8; wantParams and wantMarker remain the ORIGINAL
// pre-rewrite pins, proving the parser seam itself has not drifted — which is the
// property this corpus exists for, and the reason the key column is the only
// one a format bump is ever allowed to touch. The key column now pins v8
// against accidental movement. A later change removed the store's
// name-based volatile-parameter filter in favor of task-contract projection;
// only rows that passed those formerly filtered names directly to this store
// fixture moved.

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
// passthrough separator, and extension flag passthrough.
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
