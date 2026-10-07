package ci

import (
	"strings"
	"testing"
)

func TestLoadReadsEveryProvider(t *testing.T) {
	files := []string{
		".github/workflows/ci.yml", ".github/workflows/release.yaml", ".github/workflows/notes.md", ".gitlab-ci.yml",
		".circleci/config.yml", "azure-pipelines.yml", "Jenkinsfile", "bitbucket-pipelines.yml",
		".buildkite/pipeline.yml", ".travis.yml", "putnami.ci.json", "src/main.go",
	}
	contents := map[string][]byte{
		".github/workflows/ci.yml":       []byte("on: [pull_request]\njobs: {t: {steps: [{run: go test ./...}]}}"),
		".github/workflows/release.yaml": []byte("on: push\njobs: {t: {steps: [{run: go test ./...}]}}"),
		".gitlab-ci.yml":                 []byte("test:\n  script: make test\n"),
		"putnami.ci.json":                []byte(`{"commands":["lint","test"],"flags":["--enforce-coverage"],"runner":{"selection":"impacted"}}`),
	}
	configs := Load(files, contents)
	if len(configs) != 10 {
		t.Fatalf("loaded %d configurations, want 10: %+v", len(configs), configs)
	}
	for _, tc := range []struct {
		fragment string
		want     []string
	}{
		{"go test", []string{".github/workflows/ci.yml"}},
		{"make test", []string{".gitlab-ci.yml"}},
		{"putnami test --enforce-coverage --impacted", []string{"putnami.ci.json"}},
		{"cargo test", nil},
	} {
		var got []string
		for _, config := range Runs(configs, tc.fragment) {
			got = append(got, config.Path)
		}
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("Runs(%q) = %v, want %v", tc.fragment, got, tc.want)
		}
	}
	if putnamiText([]byte("not json")) != "" {
		t.Fatal("an invalid putnami.ci.json reads as commands")
	}
}

func TestLoadFollowsLocalGitHubActions(t *testing.T) {
	files := []string{
		".github/workflows/main.yml", ".github/workflows/release.yml", ".github/workflows/_build.yml",
		".github/setup/action.yml", ".github/cache/action.yaml", ".github/publish/action.yml", ".github/unused/action.yml",
		"tools/action.yml",
	}
	contents := map[string][]byte{
		".github/workflows/main.yml": []byte("on:\n  pull_request:\njobs:\n  test:\n    steps:\n      - uses: ./.github/setup\n      - run: pnpm test\n" +
			"  build:\n    uses: ./.github/workflows/_build.yml\n"),
		".github/workflows/release.yml": []byte("on: push\njobs:\n  release:\n    steps:\n      - uses: './.github/publish' # publish\n"),
		".github/workflows/_build.yml":  []byte("on: workflow_call\njobs:\n  build:\n    steps:\n      - run: pnpm build\n"),
		".github/setup/action.yml": []byte("runs:\n  using: composite\n  steps:\n    - uses: actions/setup-node@v6\n      with:\n        node-version-file: '.nvmrc'\n" +
			"    - uses: ./.github/cache\n    - uses: ./.github/setup\n    - run: pnpm install\n"),
		".github/cache/action.yaml":  []byte("runs:\n  using: composite\n  steps:\n    - run: pnpm store path\n"),
		".github/publish/action.yml": []byte("runs:\n  using: composite\n  steps:\n    - run: pnpm publish\n"),
		".github/unused/action.yml":  []byte("runs:\n  using: composite\n  steps:\n    - run: pnpm install --frozen-lockfile\n"),
		"tools/action.yml":           []byte("runs:\n  using: composite\n"),
	}
	configs := Load(files, contents)
	byPath := map[string]Config{}
	for _, config := range configs {
		byPath[config.Path] = config
	}
	for file, onPR := range map[string]bool{
		".github/setup/action.yml": true, ".github/cache/action.yaml": true, ".github/workflows/_build.yml": true,
		".github/publish/action.yml": false, ".github/workflows/release.yml": false,
	} {
		config, ok := byPath[file]
		if !ok || config.Provider != "GitHub Actions" || config.OnPullRequest != onPR {
			t.Errorf("%s = %+v (loaded %v), want GitHub Actions on pull requests %v", file, config, ok, onPR)
		}
	}
	if _, ok := byPath[".github/unused/action.yml"]; ok {
		t.Error("an action no workflow calls must not load")
	}
	if len(configs) != 6 {
		t.Fatalf("loaded %d configurations, want 6: %+v", len(configs), configs)
	}
	if runs := Runs(configs, "node-version-file"); len(runs) != 1 || runs[0].Path != ".github/setup/action.yml" {
		t.Fatalf("Runs(node-version-file) = %+v", runs)
	}
	if runs := Runs(configs, "pnpm build"); len(runs) != 1 || runs[0].Path != ".github/workflows/_build.yml" {
		t.Fatalf("a reusable workflow called on pull requests runs on them: %+v", runs)
	}
	if got := Unless(Runs(configs, "pnpm"), "pnpm install"); len(got) != 3 {
		t.Fatalf("Unless = %+v", got)
	}
	for file, want := range map[string]bool{".github/setup/action.yml": true, "a/action.yaml": true, ".github/workflows/ci.yml": false} {
		if ActionCandidate(file) != want {
			t.Errorf("ActionCandidate(%q) = %v, want %v", file, !want, want)
		}
	}
}

func TestSpellCommandsReadsPutnamiInvocations(t *testing.T) {
	for text, want := range map[string]string{
		"run `putnami lint,test,build --impacted`": "run `putnami lint putnami test putnami build --impacted`",
		"./putnamiw test --projects a":             "putnami test --projects a",
		"run: ./putnamiw lint,build":               "run: putnami lint putnami build",
		"with `./putnamiw` when present":           "with `./putnamiw` when present",
		"import @putnami/go and putnami.json":      "import @putnami/go and putnami.json",
	} {
		if got := SpellCommands(text); got != want {
			t.Errorf("SpellCommands(%q) = %q, want %q", text, got, want)
		}
	}
	configs := Load([]string{".github/workflows/ci.yml"}, map[string][]byte{
		".github/workflows/ci.yml": []byte("on: pull_request\njobs: {t: {steps: [{run: ./putnamiw lint,test,build}]}}"),
	})
	if len(Runs(configs, "putnami test")) != 1 {
		t.Fatal("a CI step running ./putnamiw lint,test,build must read as putnami test")
	}
}

func TestLoadFollowsGitLabIncludes(t *testing.T) {
	files := []string{
		".gitlab-ci.yml", ".gitlab/_.yml", ".gitlab/backend/_.yml", ".gitlab/backend/test/lint.yml",
		".gitlab/backend/test/unit.yml", ".gitlab/agents/default/config.yaml", "ci/deploy.yml", "ci/unused.yml",
		".gitlab/security.gitlab-ci.yml", "docs/ci.yml",
	}
	contents := map[string][]byte{
		".gitlab-ci.yml":                []byte("include:\n  - local: .gitlab/_.yml\n  - '/ci/deploy.yml' # deploy\n  - project: acme/templates\n    file: /base.yml\n  - template: Security/SAST.gitlab-ci.yml\nstages: [test]\n"),
		".gitlab/_.yml":                 []byte("include:\n- local: '.gitlab/backend/_.yml'\n- remote: https://example.org/x.yml\n"),
		".gitlab/backend/_.yml":         []byte("include: { local: '.gitlab/backend/test/*.yml' }\nvariables:\n  X: 1\n"),
		".gitlab/backend/test/lint.yml": []byte("lint:\n  script: pnpm run lint\n  rules:\n    - changes:\n      - .gitlab/security.gitlab-ci.yml\n"),
		".gitlab/backend/test/unit.yml": []byte("unit:\n  script: pnpm test\n"),
	}
	var got []string
	for _, config := range Load(files, contents) {
		got = append(got, config.Path)
	}
	want := []string{".gitlab-ci.yml", ".gitlab/_.yml", ".gitlab/backend/_.yml", ".gitlab/backend/test/lint.yml", ".gitlab/backend/test/unit.yml", "ci/deploy.yml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	if runs := Runs(Load(files, contents), "run lint"); len(runs) != 1 || runs[0].Path != ".gitlab/backend/test/lint.yml" || runs[0].Provider != "GitLab CI" {
		t.Fatalf("Runs(lint) = %+v", runs)
	}
	if got := gitlabIncludes("include: '.gitlab/one.yml'\n"); len(got) != 1 || got[0] != ".gitlab/one.yml" {
		t.Fatalf("scalar include = %v", got)
	}
	if got := gitlabIncludes("include: ['a.yml', \"/b.yml\"]\n"); strings.Join(got, ",") != "a.yml,b.yml" {
		t.Fatalf("flow include = %v", got)
	}
	for file, want := range map[string]bool{".gitlab/x.yml": true, "ci/x.yaml": true, "deploy/base.gitlab-ci.yml": true, ".gitlab-ci.yml": false, "docs/ci.yml": false, ".gitlab/README.md": false} {
		if IncludeCandidate(file) != want {
			t.Errorf("IncludeCandidate(%q) = %v, want %v", file, !want, want)
		}
	}
}
