package gitlab

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/upload"
	"go.putnami.dev/tooling/observe/internal/yaml"
)

const feature = "tooling/observe-gitlab-component"

// tokenNames are the credentials a GitLab job may hold, in the order a fake
// logs them after each call.
var tokenNames = []string{"PUTNAMI_ID_TOKEN", "PUTNAMI_CLOUD_TOKEN", "PUTNAMI_GITLAB_TOKEN", "CI_JOB_TOKEN", "CI_REPOSITORY_URL", "CI_REGISTRY_PASSWORD", "CI_DEPENDENCY_PROXY_PASSWORD", "CI_DEPLOY_PASSWORD", "PUTNAMI_ORGANIZATION"}

// logCall is the shell line a fake runs to log its arguments and the tokens
// it sees, one line per call.
func logCall(name string) string {
	var format, args strings.Builder
	for _, token := range tokenNames {
		format.WriteString(" " + token + "=%s")
		args.WriteString(` "${` + token + `-unset}"`)
	}
	return `printf '` + name + ` %s |` + format.String() + `\n' "$*"` + args.String() + ` >>"$LOG"`
}

// fakeInstaller is what the fake curl serves for install.sh: it records its
// arguments and installs a putnami that records each call with the tokens it
// sees.
func fakeInstaller() string {
	return logCall("installer") + `
mkdir -p "$HOME/.putnami/bin"
cat >"$HOME/.putnami/bin/putnami" <<'PUTNAMI'
#!/bin/sh
` + logCall("putnami") + `
PUTNAMI
chmod +x "$HOME/.putnami/bin/putnami"
`
}

// template is the component file read as GitLab reads it: the spec document
// and the job document.
type template struct {
	inputs yaml.Map
	job    yaml.Map
}

func readTemplate(t *testing.T) template {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("templates", "observe.yml"))
	if err != nil {
		t.Fatal(err)
	}
	header, body, ok := strings.Cut(string(data), "\n---\n")
	if !ok {
		t.Fatal("templates/observe.yml has no --- between its spec and its jobs")
	}
	spec, err := yaml.Parse([]byte(header))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	jobs, err := yaml.Parse([]byte(body))
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	inputs, _ := yaml.Get(spec, "spec", "inputs").(yaml.Map)
	job, _ := yaml.Get(jobs, "observe").(yaml.Map)
	if inputs == nil || job == nil {
		t.Fatal("templates/observe.yml declares no spec.inputs or no observe job")
	}
	if m, _ := jobs.(yaml.Map); len(m) != 1 {
		t.Fatalf("templates/observe.yml declares %d jobs, want observe alone", len(m))
	}
	return template{inputs: inputs, job: job}
}

// interpolate replaces each $[[ inputs.<name> ]] in value with the input's
// value, or its default, as GitLab does when it includes the component.
func (tpl template) interpolate(t *testing.T, value string, inputs map[string]string) string {
	t.Helper()
	for name := range tpl.inputs {
		v, set := inputs[name]
		if !set {
			v = yaml.String(tpl.inputs, name, "default")
		}
		value = strings.ReplaceAll(value, "$[[ inputs."+name+" ]]", v)
	}
	if strings.Contains(value, "$[[") {
		t.Fatalf("%q names an input the spec does not declare", value)
	}
	return value
}

type run struct {
	out string
	err error
	log []string
	dir string
}

// runJob runs the observe job's script the way a GitLab runner does: in the
// project directory, with the job's variables interpolated from inputs, the
// job's tokens in env, and a fake curl that serves fakeInstaller.
func runJob(t *testing.T, inputs, env map[string]string) run {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the component runs on Linux runners")
	}
	tpl := readTemplate(t)
	script, ok := yaml.Strings(yaml.Get(tpl.job, "script"))
	if !ok || len(script) != 1 {
		t.Fatalf("script = %v, want one block", yaml.Get(tpl.job, "script"))
	}

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	project := filepath.Join(dir, "project")
	for _, d := range []string{home, bin, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls.log")
	installer := filepath.Join(dir, "install.sh")
	scriptFile := filepath.Join(dir, "script.sh")
	write(t, installer, fakeInstaller(), 0o644)
	write(t, scriptFile, script[0], 0o644)
	write(t, filepath.Join(bin, "curl"), "#!/bin/sh\n"+logCall("curl")+"\ncat \"$FAKE_INSTALLER\"\n", 0o755)

	vars := []string{
		"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + home,
		"LOG=" + log,
		"FAKE_INSTALLER=" + installer,
		"GITLAB_CI=true",
		"CI_SERVER_HOST=gitlab.com",
		"PUTNAMI_ID_TOKEN=id-token",
		"CI_JOB_TOKEN=job-token",
		"CI_REPOSITORY_URL=https://gitlab-ci-token:job-token@gitlab.com/acme/shop.git",
		"CI_REGISTRY_PASSWORD=registry-password",
		"CI_DEPENDENCY_PROXY_PASSWORD=proxy-password",
		"CI_DEPLOY_PASSWORD=deploy-password",
	}
	variables, _ := yaml.Get(tpl.job, "variables").(yaml.Map)
	for name, value := range variables {
		s, _ := value.(string)
		vars = append(vars, name+"="+tpl.interpolate(t, s, inputs))
	}
	for name, value := range env {
		vars = append(vars, name+"="+value)
	}
	cmd := exec.Command("bash", scriptFile)
	cmd.Dir = project
	cmd.Env = vars
	out, err := cmd.CombinedOutput()
	r := run{out: string(out), err: err, dir: project}
	if data, readErr := os.ReadFile(log); readErr == nil {
		r.log = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	return r
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// observeCall returns the logged putnami observe call.
func observeCall(t *testing.T, r run) string {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the script failed: %v\n%s", r.err, r.out)
	}
	for _, line := range r.log {
		if strings.HasPrefix(line, "putnami observe ") {
			return line
		}
	}
	t.Fatalf("no putnami observe call in %q", r.log)
	return ""
}

// command returns a logged call without the tokens it saw.
func command(line string) string {
	before, _, _ := strings.Cut(line, " | ")
	return before
}

// seen returns the value of a token a logged call saw, "unset" for none.
func seen(t *testing.T, line, name string) string {
	t.Helper()
	_, tokens, _ := strings.Cut(line, " | ")
	for _, field := range strings.Fields(tokens) {
		if value, ok := strings.CutPrefix(field, name+"="); ok {
			return value
		}
	}
	t.Fatalf("%s is not logged in %q", name, line)
	return ""
}

func expectCalls(t *testing.T, r run, want ...string) {
	t.Helper()
	var got []string
	for _, line := range r.log {
		got = append(got, command(line))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestTheComponentInstallsTheCLIOfTheChannel(t *testing.T) {
	spectest.Proves(t, feature, "component-installs-the-cli", "the-component-installs-the-cli-of-the-channel")

	for _, tc := range []struct{ input, channel string }{
		{"", "latest"},
		{"stable", "latest"},
		{"latest", "latest"},
		{"canary", "canary"},
		{"tooling-v0.4.0", "tooling-v0.4.0"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			inputs := map[string]string{"organization": "org-acme"}
			if tc.input != "" {
				inputs["channel"] = tc.input
			}
			r := runJob(t, inputs, nil)
			observeCall(t, r)
			expectCalls(t, r,
				"curl -fsSL https://putnami.dev/install.sh",
				"installer --version "+tc.channel,
				"putnami extensions install --user @putnami/observe@"+tc.channel,
				"putnami extensions install --user @putnami/cloud@"+tc.channel,
				"putnami observe --evidence observe-evidence.json --upload --publish",
			)
		})
	}
}

func TestTheCloudExtensionInstallsOnlyWhenUploading(t *testing.T) {
	spectest.Proves(t, feature, "component-installs-the-cli", "the-cloud-extension-installs-only-when-uploading")

	r := runJob(t, nil, nil)
	observeCall(t, r)
	expectCalls(t, r,
		"curl -fsSL https://putnami.dev/install.sh",
		"installer --version latest",
		"putnami extensions install --user @putnami/observe@latest",
		"putnami observe --evidence observe-evidence.json",
	)
}

func TestAVersionOrABadChannelFailsBeforeTheInstall(t *testing.T) {
	spectest.Proves(t, feature, "component-channel-is-a-channel", "a-version-or-a-bad-channel-fails-before-the-install")

	for _, channel := range []string{"0.4.0", "v1.2.3", "1.0.0-rc.1", "Stable", "-canary", "canary channel", strings.Repeat("a", 65)} {
		t.Run(channel, func(t *testing.T) {
			r := runJob(t, map[string]string{"channel": channel}, nil)
			if r.err == nil {
				t.Fatalf("channel %q passed:\n%s", channel, r.out)
			}
			if !strings.Contains(r.out, "name a release channel") {
				t.Fatalf("output does not name a channel:\n%s", r.out)
			}
			if len(r.log) != 0 {
				t.Fatalf("a bad channel made calls: %q", r.log)
			}
		})
	}
}

func TestTheInputsSetTheJob(t *testing.T) {
	spectest.Proves(t, feature, "component-job-settings", "the-inputs-set-the-job")

	tpl := readTemplate(t)
	for key, want := range map[string]string{"stage": "test", "image": "buildpack-deps:bookworm-scm"} {
		if got := yaml.String(tpl.job, key); got != "$[[ inputs."+key+" ]]" {
			t.Fatalf("%s = %q, want the input", key, got)
		}
		if got := yaml.String(tpl.inputs, key, "default"); got != want {
			t.Fatalf("the %s input defaults to %q, want %q", key, got, want)
		}
	}
	if got := yaml.String(tpl.job, "needs"); got != "$[[ inputs.needs ]]" {
		t.Fatalf("needs = %q, want the input", got)
	}
	if got := yaml.String(tpl.inputs, "needs", "type"); got != "array" {
		t.Fatalf("the needs input is of type %q, want array", got)
	}
	needs, _ := yaml.Get(tpl.inputs, "needs", "default").([]any)
	if len(needs) != 1 || yaml.String(needs[0], "job") != "test" || yaml.String(needs[0], "artifacts") != "true" || yaml.String(needs[0], "optional") != "true" {
		t.Fatalf("the needs input defaults to %v, want the job test, its artifacts, optional", yaml.Get(tpl.inputs, "needs", "default"))
	}
	if got := yaml.String(tpl.job, "when"); got != "always" {
		t.Fatalf("when = %q, want always, so a failed test job is observed", got)
	}
	for _, key := range []string{"before_script", "after_script", "cache"} {
		if list, ok := yaml.Get(tpl.job, key).([]any); !ok || len(list) != 0 {
			t.Fatalf("%s = %v, want [] to keep the pipeline's defaults out", key, yaml.Get(tpl.job, key))
		}
	}
	data, err := os.ReadFile(filepath.Join("templates", "observe.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, depth := range []string{"GIT_DEPTH", "GIT_STRATEGY"} {
		if strings.Contains(string(data), depth) {
			t.Fatalf("the component sets %s", depth)
		}
	}
}

func TestTheJobKeepsTheEvidenceDocument(t *testing.T) {
	spectest.Proves(t, feature, "component-job-settings", "the-job-keeps-the-evidence-document")

	tpl := readTemplate(t)
	if got := yaml.String(tpl.job, "artifacts", "when"); got != "always" {
		t.Fatalf("artifacts.when = %q, want always", got)
	}
	if paths, ok := yaml.Strings(yaml.Get(tpl.job, "artifacts", "paths")); !ok || !slices.Equal(paths, []string{"observe-evidence.json"}) {
		t.Fatalf("artifacts.paths = %v, want the evidence document", yaml.Get(tpl.job, "artifacts", "paths"))
	}
	call := observeCall(t, runJob(t, nil, nil))
	if !strings.HasPrefix(command(call), "putnami observe --evidence observe-evidence.json") {
		t.Fatalf("observe call = %q, want the evidence document in the project directory", call)
	}
}

func TestTheInputsReachTheScriptAsVariables(t *testing.T) {
	spectest.Proves(t, feature, "component-maps-inputs-to-flags", "the-inputs-reach-the-script-as-variables")

	tpl := readTemplate(t)
	script, _ := yaml.Strings(yaml.Get(tpl.job, "script"))
	if strings.Contains(strings.Join(script, "\n"), "$[[") {
		t.Fatal("the script interpolates an input into its text")
	}
	variables, _ := yaml.Get(tpl.job, "variables").(yaml.Map)
	var names []string
	for name := range tpl.inputs {
		names = append(names, name)
		if name == "stage" || name == "needs" || name == "image" {
			continue
		}
		variable := "PUTNAMI_OBSERVE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if got := yaml.String(variables, variable); got != "$[[ inputs."+name+" ]]" {
			t.Fatalf("variable %s = %q, want the input %s", variable, got, name)
		}
	}
	slices.Sort(names)
	if want := []string{"base", "base-sbom", "channel", "coverage", "image", "junit", "needs", "organization", "publish", "sbom", "stage"}; !slices.Equal(names, want) {
		t.Fatalf("inputs = %q, want %q", names, want)
	}
	if len(variables) != 8 {
		t.Fatalf("variables = %v, want one per script input", variables)
	}

	// A value that would run a command as script text stays one argument.
	value := `x"; touch pwned; echo "$(touch pwned)`
	r := runJob(t, map[string]string{"junit": value}, nil)
	call := observeCall(t, r)
	if !strings.Contains(call, "--junit "+value) {
		t.Fatalf("observe call = %q, want the value as the --junit argument", call)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "pwned")); err == nil {
		t.Fatal("an input value ran a command")
	}
}

func TestEachSetInputBecomesItsFlag(t *testing.T) {
	spectest.Proves(t, feature, "component-maps-inputs-to-flags", "each-set-input-becomes-its-flag")

	r := runJob(t, map[string]string{
		"junit":     "reports/junit with space.xml",
		"coverage":  "coverage/lcov.info",
		"sbom":      "head.cdx.json",
		"base-sbom": "base.cdx.json",
		"base":      "origin/main",
	}, nil)
	want := "putnami observe --evidence observe-evidence.json --junit reports/junit with space.xml --coverage coverage/lcov.info --sbom head.cdx.json --base-sbom base.cdx.json --base origin/main"
	if got := command(observeCall(t, r)); got != want {
		t.Fatalf("observe call = %q, want %q", got, want)
	}
}

func TestAnEmptyInputPassesNoFlag(t *testing.T) {
	spectest.Proves(t, feature, "component-maps-inputs-to-flags", "an-empty-input-passes-no-flag")

	r := runJob(t, map[string]string{"junit": "", "coverage": "", "sbom": "", "base-sbom": "", "base": ""}, nil)
	if got := command(observeCall(t, r)); got != "putnami observe --evidence observe-evidence.json" {
		t.Fatalf("observe call = %q", got)
	}
}

func TestTheIDTokensBlockNamesTheAuthServerOnly(t *testing.T) {
	spectest.Proves(t, feature, "component-uploads-with-the-id-token", "the-id-tokens-block-names-the-auth-server-only")

	tpl := readTemplate(t)
	tokens, _ := yaml.Get(tpl.job, "id_tokens").(yaml.Map)
	if len(tokens) != 1 {
		t.Fatalf("id_tokens = %v, want PUTNAMI_ID_TOKEN alone", tokens)
	}
	token, _ := tokens["PUTNAMI_ID_TOKEN"].(yaml.Map)
	if len(token) != 1 || yaml.String(token, "aud") != "https://auth.putnami.cloud" {
		t.Fatalf("PUTNAMI_ID_TOKEN = %v, want the one audience https://auth.putnami.cloud", tokens["PUTNAMI_ID_TOKEN"])
	}
	for _, key := range []string{"secrets", "services", "environment"} {
		if yaml.Get(tpl.job, key) != nil {
			t.Fatalf("the job declares %s", key)
		}
	}
}

func TestAnOrganizationUploadsWithTheIDToken(t *testing.T) {
	spectest.Proves(t, feature, "component-uploads-with-the-id-token", "an-organization-uploads-with-the-id-token")

	call := observeCall(t, runJob(t, map[string]string{"organization": "org-acme"}, nil))
	if got := command(call); got != "putnami observe --evidence observe-evidence.json --upload --publish" {
		t.Fatalf("observe call = %q", got)
	}
	if got := seen(t, call, "PUTNAMI_ORGANIZATION"); got != "org-acme" {
		t.Fatalf("PUTNAMI_ORGANIZATION = %q", got)
	}
	if got := seen(t, call, "PUTNAMI_ID_TOKEN"); got != "id-token" {
		t.Fatalf("PUTNAMI_ID_TOKEN = %q, want the job's ID token", got)
	}
	if got := seen(t, call, "PUTNAMI_CLOUD_TOKEN"); got != "unset" {
		t.Fatalf("PUTNAMI_CLOUD_TOKEN = %q", got)
	}
}

func TestACloudTokenVariableUploadsAndWins(t *testing.T) {
	spectest.Proves(t, feature, "component-uploads-with-a-cloud-token", "a-cloud-token-variable-uploads-and-wins")

	for _, inputs := range []map[string]string{nil, {"organization": "org-acme"}} {
		r := runJob(t, inputs, map[string]string{"PUTNAMI_CLOUD_TOKEN": "cloud-token", "PUTNAMI_ORGANIZATION": "inherited"})
		call := observeCall(t, r)
		if got := command(call); got != "putnami observe --evidence observe-evidence.json --upload --publish" {
			t.Fatalf("%v: observe call = %q", inputs, got)
		}
		if got := seen(t, call, "PUTNAMI_CLOUD_TOKEN"); got != "cloud-token" {
			t.Fatalf("%v: PUTNAMI_CLOUD_TOKEN = %q", inputs, got)
		}
		for _, name := range []string{"PUTNAMI_ID_TOKEN", "PUTNAMI_ORGANIZATION"} {
			if got := seen(t, call, name); got != "unset" {
				t.Fatalf("%v: %s = %q, want unset since the cloud token wins", inputs, name, got)
			}
		}
	}
}

func TestWithoutACredentialTheJobPrintsTheSummaryOnly(t *testing.T) {
	spectest.Proves(t, feature, "component-uploads-with-a-cloud-token", "without-a-credential-the-job-prints-the-summary-only")

	call := observeCall(t, runJob(t, nil, map[string]string{"PUTNAMI_GITLAB_TOKEN": "gitlab-token", "PUTNAMI_ORGANIZATION": "inherited"}))
	if got := command(call); got != "putnami observe --evidence observe-evidence.json" {
		t.Fatalf("observe call = %q", got)
	}
	for _, name := range []string{"PUTNAMI_ID_TOKEN", "PUTNAMI_GITLAB_TOKEN", "PUTNAMI_ORGANIZATION"} {
		if got := seen(t, call, name); got != "unset" {
			t.Fatalf("%s = %q, want unset", name, got)
		}
	}
	if got := seen(t, call, upload.TokenEnv); got != "unset" {
		t.Fatalf("%s = %q", upload.TokenEnv, got)
	}
}

func TestThePublishInputTurnsThePostOff(t *testing.T) {
	spectest.Proves(t, feature, "component-uploads-with-a-cloud-token", "the-publish-input-turns-the-post-off")

	tpl := readTemplate(t)
	if got := yaml.String(tpl.inputs, "publish", "type"); got != "boolean" {
		t.Fatalf("the publish input is of type %q, want boolean", got)
	}
	if got := yaml.String(tpl.inputs, "publish", "default"); got != "true" {
		t.Fatalf("the publish input defaults to %q, want true", got)
	}
	call := observeCall(t, runJob(t, map[string]string{"organization": "org-acme", "publish": "false"}, map[string]string{"PUTNAMI_GITLAB_TOKEN": "gitlab-token"}))
	if got := command(call); got != "putnami observe --evidence observe-evidence.json --upload" {
		t.Fatalf("observe call = %q", got)
	}
	if got := seen(t, call, "PUTNAMI_GITLAB_TOKEN"); got != "unset" {
		t.Fatalf("PUTNAMI_GITLAB_TOKEN = %q, want unset without --publish", got)
	}

	r := runJob(t, map[string]string{"publish": "maybe"}, nil)
	if r.err == nil || !strings.Contains(r.out, `publish is "maybe"`) || len(r.log) != 0 {
		t.Fatalf("an invalid publish value: err %v, calls %q\n%s", r.err, r.log, r.out)
	}
}

func TestTheInstallSeesNoToken(t *testing.T) {
	spectest.Proves(t, feature, "component-tokens-reach-observe-only", "the-install-sees-no-token")

	r := runJob(t, map[string]string{"organization": "org-acme"}, map[string]string{
		"PUTNAMI_CLOUD_TOKEN":  "cloud-token",
		"PUTNAMI_GITLAB_TOKEN": "gitlab-token",
	})
	observeCall(t, r)
	for _, line := range r.log {
		if strings.HasPrefix(line, "putnami observe ") {
			continue
		}
		for _, name := range tokenNames {
			if name == "PUTNAMI_ORGANIZATION" {
				continue
			}
			if got := seen(t, line, name); got != "unset" {
				t.Fatalf("%q sees %s=%s", command(line), name, got)
			}
		}
	}
}

func TestTheIDTokenReachesObserveOnlyForTheExchange(t *testing.T) {
	spectest.Proves(t, feature, "component-tokens-reach-observe-only", "the-id-token-reaches-observe-only-for-the-exchange")

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		env    map[string]string
		want   string
	}{
		{"exchange", map[string]string{"organization": "org-acme"}, nil, "id-token"},
		{"no organization", nil, nil, "unset"},
		{"cloud token", map[string]string{"organization": "org-acme"}, map[string]string{"PUTNAMI_CLOUD_TOKEN": "cloud-token"}, "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := observeCall(t, runJob(t, tc.inputs, tc.env))
			if got := seen(t, call, "PUTNAMI_ID_TOKEN"); got != tc.want {
				t.Fatalf("PUTNAMI_ID_TOKEN = %q, want %q", got, tc.want)
			}
			// Git may deepen the clone with the runner's credentials.
			if got := seen(t, call, "CI_JOB_TOKEN"); got != "job-token" {
				t.Fatalf("CI_JOB_TOKEN = %q, want the job token kept for Git", got)
			}
		})
	}
}

func TestTheGitLabTokenReachesObserveOnlyWhenPublishing(t *testing.T) {
	spectest.Proves(t, feature, "component-tokens-reach-observe-only", "the-gitlab-token-reaches-observe-only-when-publishing")

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{"upload and publish", map[string]string{"organization": "org-acme"}, "gitlab-token"},
		{"upload only", map[string]string{"organization": "org-acme", "publish": "false"}, "unset"},
		{"no upload", nil, "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := observeCall(t, runJob(t, tc.inputs, map[string]string{"PUTNAMI_GITLAB_TOKEN": "gitlab-token"}))
			if got := seen(t, call, "PUTNAMI_GITLAB_TOKEN"); got != tc.want {
				t.Fatalf("PUTNAMI_GITLAB_TOKEN = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheComponentMarksItsRunForTheNextLine(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("templates", "observe.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "export "+upload.ComponentEnv+"=true") {
		t.Fatalf("the script does not set %s for putnami observe", upload.ComponentEnv)
	}
}
