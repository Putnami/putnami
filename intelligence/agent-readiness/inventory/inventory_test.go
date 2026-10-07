package inventory

import (
	"encoding/json"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

func TestDetectReadsManifestsAcrossEcosystems(t *testing.T) {
	files := []gitrepo.File{
		{Path: ".claude/settings.json", Text: true, Lines: 3},
		{Path: ".cursorrules", Text: true, Lines: 1, Size: 12},
		{Path: ".github/copilot-instructions.md", Text: true, Lines: 2, Size: 40},
		{Path: ".gitlab-ci.yml", Text: true, Lines: 4},
		{Path: ".terraform-version", Text: true, Lines: 1},
		{Path: "Cargo.toml", Text: true, Lines: 5},
		{Path: "Dockerfile", Text: true, Lines: 3},
		{Path: "Gemfile", Text: true, Lines: 2},
		{Path: "infra/main.tf", Text: true, Lines: 10},
		{Path: "logo.png", Size: 900},
		{Path: "poetry.lock", Text: true, Lines: 50},
		{Path: "pyproject.toml", Text: true, Lines: 6},
		{Path: "requirements.txt", Text: true, Lines: 2},
		{Path: "src/app.py", Text: true, Lines: 20},
		{Path: "src/lib.rs", Text: true, Lines: 30},
		{Path: "web/package.json", Text: true, Lines: 1},
		{Path: "web/vendor/x.js", Text: true, Lines: 99},
	}
	contents := map[string][]byte{
		"Cargo.toml":         []byte("[dependencies]\naxum = { version = \"0.7.5\", features = [\"json\"] }\ntokio = \"1\"\n"),
		"Gemfile":            []byte("gem 'rails', '~> 7.1.3'\n"),
		"pyproject.toml":     []byte("[project]\ndependencies = [\"fastapi==0.110.0\", \"psycopg[binary]>=3\"]\n"),
		"requirements.txt":   []byte("# pinned\nDjango==5.0.1\n-r other.txt\npytest\n"),
		"web/package.json":   []byte(`{"packageManager":"yarn@4.1.0+sha256.abc","dependencies":{"next":"workspace:*","vue":"3.4.21"}}`),
		".terraform-version": []byte("1.9.5\n"),
	}
	authored := func(path string) bool { return path != "poetry.lock" && path != "web/vendor/x.js" }
	inv := Detect(Input{Files: files, Authored: authored, Contents: contents, Changed: map[string]int64{".cursorrules": 100}, Now: 100 + 3*secondsPerDay})
	got, _ := json.Marshal(inv)
	want := `{"languages":[{"name":"HCL","files":1,"lines":10},{"name":"Python","files":1,"lines":20},{"name":"Rust","files":1,"lines":30}],` +
		`"frameworks":[{"name":"Axum","version":"0.7.5"},{"name":"Django","version":"5.0.1"},{"name":"FastAPI","version":"0.110.0"},{"name":"Next.js"},{"name":"Rails","version":"7.1.3"},{"name":"Vue","version":"3.4.21"}],` +
		`"packageManagers":[{"name":"Bundler"},{"name":"Cargo"},{"name":"Poetry"},{"name":"Yarn","version":"4.1.0"},{"name":"pip"}],` +
		`"monorepoTools":[],"ciProviders":[{"name":"GitLab CI"}],"iac":[{"name":"Terraform","version":"1.9.5"}],` +
		`"containers":[{"name":"Docker"}],"databases":[{"name":"PostgreSQL"}],"testFrameworks":[{"name":"pytest"}],` +
		`"agentTools":[{"name":"Claude Code"},{"name":"Cursor"},{"name":"GitHub Copilot"}],` +
		`"instructionFiles":[{"path":".cursorrules","bytes":12,"ageDays":3},{"path":".github/copilot-instructions.md","bytes":40,"ageDays":0}],` +
		`"files":14,"lines":90,"repoAgeDays":0,"commitsTotal":0,"commits90d":0,"activeContributors90d":0}`
	if string(got) != want {
		t.Fatalf("inventory =\n%s\nwant\n%s", got, want)
	}
	if wanted := Wanted(files); len(wanted) != 6 {
		t.Fatalf("Wanted = %v", wanted)
	}
}

func TestCleanVersion(t *testing.T) {
	for in, want := range map[string]string{
		"^1.2.3": "1.2.3", "~> 7.1": "7.1", "v5.1.0": "5.1.0", ">=3": "3", "workspace:*": "", "latest": "", "file:../x": "",
	} {
		if got := cleanVersion(in); got != want {
			t.Errorf("cleanVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
