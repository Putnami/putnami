package action

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

const feature = "tooling/observe-action"

// tokenFormat and tokenArgs append the four tokens a process sees to the
// call a fake logs.
const (
	tokenFormat = ` | GITHUB_TOKEN=%s PUTNAMI_CLOUD_TOKEN=%s INPUT_TOKEN=%s RUN_TOKEN=%s\n'`
	tokenArgs   = ` "${GITHUB_TOKEN-unset}" "${PUTNAMI_CLOUD_TOKEN-unset}" "${PUTNAMI_OBSERVE_TOKEN-unset}" "${PUTNAMI_OBSERVE_GITHUB_TOKEN-unset}"`
)

// noToken is the end of a logged call that saw no token.
const noToken = "| GITHUB_TOKEN=unset PUTNAMI_CLOUD_TOKEN=unset INPUT_TOKEN=unset RUN_TOKEN=unset"

// fakeInstaller is what the fake curl serves for install.sh: it records its
// arguments and installs a putnami that records each call, each with the
// tokens it sees.
const fakeInstaller = `printf 'installer %s` + tokenFormat + ` "$*"` + tokenArgs + ` >>"$LOG"
mkdir -p "$HOME/.putnami/bin"
cat >"$HOME/.putnami/bin/putnami" <<'PUTNAMI'
#!/bin/sh
printf 'putnami %s` + tokenFormat + ` "$*"` + tokenArgs + ` >>"$LOG"
PUTNAMI
chmod +x "$HOME/.putnami/bin/putnami"
`

type run struct {
	out      string
	err      error
	log      []string
	output   string
	evidence string
}

// runAction runs observe.sh the way the composite step does, with the inputs
// as PUTNAMI_OBSERVE_* variables and a fake curl that serves fakeInstaller.
func runAction(t *testing.T, inputs map[string]string) run {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the action runs on Linux and macOS runners")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	temp := filepath.Join(dir, "runner-temp")
	for _, d := range []string{home, bin, temp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls.log")
	installer := filepath.Join(dir, "install.sh")
	output := filepath.Join(dir, "github-output")
	write(t, installer, fakeInstaller, 0o644)
	write(t, filepath.Join(bin, "curl"), "#!/bin/sh\nprintf 'curl %s"+tokenFormat+` "$*"`+tokenArgs+` >>"$LOG"`+"\ncat \"$FAKE_INSTALLER\"\n", 0o755)

	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + home,
		"LOG=" + log,
		"FAKE_INSTALLER=" + installer,
		"RUNNER_OS=Linux",
		"RUNNER_TEMP=" + temp,
		"GITHUB_OUTPUT=" + output,
		"PUTNAMI_OBSERVE_GITHUB_TOKEN=run-token",
		"GITHUB_TOKEN=inherited-token",
	}
	for name, value := range inputs {
		env = append(env, name+"="+value)
	}
	cmd := exec.Command("bash", "observe.sh")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	r := run{out: string(out), err: err, evidence: filepath.Join(temp, "observe-evidence.json")}
	if data, readErr := os.ReadFile(log); readErr == nil {
		r.log = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	if data, readErr := os.ReadFile(output); readErr == nil {
		r.output = string(data)
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
		t.Fatalf("observe.sh failed: %v\n%s", r.err, r.out)
	}
	for _, line := range r.log {
		if strings.HasPrefix(line, "putnami observe ") {
			return line
		}
	}
	t.Fatalf("no putnami observe call in %q", r.log)
	return ""
}

func TestTheActionInstallsTheCLIOfTheChannel(t *testing.T) {
	spectest.Proves(t, feature, "action-installs-the-cli", "the-action-installs-the-cli-of-the-channel")
	spectest.Proves(t, feature, "action-installs-the-cli", "the-action-installs-the-extensions-in-the-user-scope")
	spectest.Proves(t, feature, "action-tokens-reach-observe-only", "the-install-sees-no-token")

	for _, tc := range []struct{ input, channel string }{
		{"", "latest"},
		{"stable", "latest"},
		{"latest", "latest"},
		{"canary", "canary"},
		{"tooling-v0.4.0", "tooling-v0.4.0"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			r := runAction(t, map[string]string{
				"PUTNAMI_OBSERVE_CHANNEL": tc.input,
				"PUTNAMI_OBSERVE_TOKEN":   "cloud-token",
			})
			observeCall(t, r)
			expectCalls(t, r,
				"curl -fsSL https://putnami.dev/install.sh",
				"installer --version "+tc.channel,
				"putnami extensions install --user @putnami/observe@"+tc.channel,
				"putnami extensions install --user @putnami/cloud@"+tc.channel,
				"putnami observe ",
			)
		})
	}
}

func TestTheCloudExtensionInstallsOnlyWithAToken(t *testing.T) {
	spectest.Proves(t, feature, "action-installs-the-cli", "the-cloud-extension-installs-only-with-a-token")

	r := runAction(t, map[string]string{"PUTNAMI_OBSERVE_PUBLISH": "true"})
	observeCall(t, r)
	expectCalls(t, r,
		"curl -fsSL https://putnami.dev/install.sh",
		"installer --version latest",
		"putnami extensions install --user @putnami/observe@latest",
		"putnami observe ",
	)
}

// expectCalls checks that the logged calls are exactly the given prefixes,
// in order, and that every call before putnami observe saw no token.
func expectCalls(t *testing.T, r run, prefixes ...string) {
	t.Helper()
	if len(r.log) != len(prefixes) {
		t.Fatalf("calls = %q, want %d calls", r.log, len(prefixes))
	}
	for i, prefix := range prefixes {
		if !strings.HasPrefix(r.log[i], prefix) {
			t.Fatalf("call %d = %q, want prefix %q; calls %q", i, r.log[i], prefix, r.log)
		}
		if prefix != "putnami observe " && !strings.HasSuffix(r.log[i], noToken) {
			t.Fatalf("call %d = %q saw a token", i, r.log[i])
		}
	}
}

func TestAVersionOrABadChannelFailsBeforeTheInstall(t *testing.T) {
	spectest.Proves(t, feature, "action-channel-is-a-channel", "a-version-or-a-bad-channel-fails-before-the-install")

	for _, channel := range []string{"1.4.2", "v0.4.0", "0.4.0-rc.1", "Canary", "-x", "a b", strings.Repeat("a", 65)} {
		t.Run(channel, func(t *testing.T) {
			r := runAction(t, map[string]string{"PUTNAMI_OBSERVE_CHANNEL": channel})
			if r.err == nil {
				t.Fatalf("observe.sh passed with channel %q:\n%s", channel, r.out)
			}
			if !strings.Contains(r.out, "name a release channel") {
				t.Fatalf("message does not ask for a channel:\n%s", r.out)
			}
			if len(r.log) != 0 {
				t.Fatalf("channel %q made calls: %q", channel, r.log)
			}
		})
	}
}

func TestATokenInTheJobEnvironmentIsNotRead(t *testing.T) {
	spectest.Proves(t, feature, "action-tokens-reach-observe-only", "a-token-in-the-job-environment-is-not-read")

	r := runAction(t, map[string]string{"PUTNAMI_CLOUD_TOKEN": "job-cloud-token"})
	call := observeCall(t, r)
	if !strings.Contains(r.out, "PUTNAMI_CLOUD_TOKEN in the job environment is not read; pass it as the token input.") {
		t.Fatalf("no warning about the job token:\n%s", r.out)
	}
	if strings.Contains(call, "--upload") || !strings.HasSuffix(call, noToken) {
		t.Fatalf("call = %q, want no upload and no token", call)
	}
	expectCalls(t, r,
		"curl -fsSL https://putnami.dev/install.sh",
		"installer --version latest",
		"putnami extensions install --user @putnami/observe@latest",
		"putnami observe ",
	)

	quiet := runAction(t, map[string]string{
		"PUTNAMI_CLOUD_TOKEN":   "job-cloud-token",
		"PUTNAMI_OBSERVE_TOKEN": "cloud-token",
	})
	if call := observeCall(t, quiet); !strings.Contains(call, "PUTNAMI_CLOUD_TOKEN=cloud-token ") {
		t.Fatalf("call = %q, want the input token", call)
	}
	if strings.Contains(quiet.out, "is not read") {
		t.Fatalf("a set token input still warns:\n%s", quiet.out)
	}
}

func TestAWindowsRunnerFailsBeforeTheInstall(t *testing.T) {
	spectest.Proves(t, feature, "action-installs-the-cli", "a-windows-runner-fails-before-the-install")

	r := runAction(t, map[string]string{"RUNNER_OS": "Windows"})
	if r.err == nil {
		t.Fatalf("observe.sh passed on Windows:\n%s", r.out)
	}
	if !strings.Contains(r.out, "Linux and macOS") {
		t.Fatalf("message does not name Linux and macOS:\n%s", r.out)
	}
	if len(r.log) != 0 {
		t.Fatalf("Windows run made calls: %q", r.log)
	}
}

func TestEachSetInputBecomesItsFlag(t *testing.T) {
	spectest.Proves(t, feature, "action-maps-inputs-to-flags", "each-set-input-becomes-its-flag")
	spectest.Proves(t, feature, "action-maps-inputs-to-flags", "the-evidence-path-is-an-output")

	r := runAction(t, map[string]string{
		"PUTNAMI_OBSERVE_JUNIT":     "reports/*.xml,more junit.xml",
		"PUTNAMI_OBSERVE_COVERAGE":  "reports/coverage.out",
		"PUTNAMI_OBSERVE_SBOM":      "head.cdx.json",
		"PUTNAMI_OBSERVE_BASE_SBOM": "base.cdx.json",
		"PUTNAMI_OBSERVE_BASE":      "origin/main",
	})
	call := observeCall(t, r)
	want := "putnami observe --evidence " + r.evidence +
		" --junit reports/*.xml,more junit.xml --coverage reports/coverage.out --sbom head.cdx.json --base-sbom base.cdx.json --base origin/main |"
	if !strings.HasPrefix(call, want) {
		t.Fatalf("call = %q, want prefix %q", call, want)
	}
	if r.output != "evidence="+r.evidence+"\n" {
		t.Fatalf("GITHUB_OUTPUT = %q", r.output)
	}
}

func TestAnEmptyInputPassesNoFlag(t *testing.T) {
	spectest.Proves(t, feature, "action-maps-inputs-to-flags", "an-empty-input-passes-no-flag")

	r := runAction(t, map[string]string{
		"PUTNAMI_OBSERVE_JUNIT":     "",
		"PUTNAMI_OBSERVE_COVERAGE":  "",
		"PUTNAMI_OBSERVE_SBOM":      "",
		"PUTNAMI_OBSERVE_BASE_SBOM": "",
		"PUTNAMI_OBSERVE_BASE":      "",
	})
	call := observeCall(t, r)
	if want := "putnami observe --evidence " + r.evidence + " |"; !strings.HasPrefix(call, want) {
		t.Fatalf("call = %q, want prefix %q", call, want)
	}
}

func TestATokenUploadsAndPublishes(t *testing.T) {
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "a-token-uploads-and-publishes")
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "the-github-token-reaches-observe-only-when-publishing")

	call := observeCall(t, runAction(t, map[string]string{"PUTNAMI_OBSERVE_TOKEN": "cloud-token"}))
	if !strings.Contains(call, " --upload --publish |") {
		t.Fatalf("call = %q, want --upload --publish", call)
	}
	if !strings.HasSuffix(call, "| GITHUB_TOKEN=run-token PUTNAMI_CLOUD_TOKEN=cloud-token INPUT_TOKEN=unset RUN_TOKEN=unset") {
		t.Fatalf("call = %q, want the run token, the cloud token and no input token", call)
	}
}

func TestNoTokenPrintsTheSummaryOnly(t *testing.T) {
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "no-token-prints-the-summary-only")
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "the-github-token-reaches-observe-only-when-publishing")

	call := observeCall(t, runAction(t, nil))
	if strings.Contains(call, "--upload") || strings.Contains(call, "--publish") {
		t.Fatalf("call = %q, want neither --upload nor --publish", call)
	}
	if !strings.HasSuffix(call, noToken) {
		t.Fatalf("call = %q, want no token", call)
	}
}

func TestThePublishInputOverridesTheDefault(t *testing.T) {
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "the-publish-input-overrides-the-default")

	withToken := observeCall(t, runAction(t, map[string]string{
		"PUTNAMI_OBSERVE_TOKEN":   "cloud-token",
		"PUTNAMI_OBSERVE_PUBLISH": "false",
	}))
	if !strings.Contains(withToken, "--upload |") || strings.Contains(withToken, "--publish") {
		t.Fatalf("token with publish false: call = %q, want --upload without --publish", withToken)
	}
	if !strings.Contains(withToken, "GITHUB_TOKEN=unset") {
		t.Fatalf("token with publish false: call = %q, want no GITHUB_TOKEN", withToken)
	}

	withoutToken := observeCall(t, runAction(t, map[string]string{"PUTNAMI_OBSERVE_PUBLISH": "true"}))
	if !strings.Contains(withoutToken, " --publish |") || strings.Contains(withoutToken, "--upload") {
		t.Fatalf("no token with publish true: call = %q, want --publish without --upload", withoutToken)
	}
	if !strings.HasSuffix(withoutToken, noToken) {
		t.Fatalf("no token with publish true: call = %q, want no token, since --publish without --upload posts nothing", withoutToken)
	}
}

func TestAnInvalidPublishValueFailsTheStep(t *testing.T) {
	spectest.Proves(t, feature, "action-token-uploads-and-publishes", "an-invalid-publish-value-fails-the-step")

	r := runAction(t, map[string]string{"PUTNAMI_OBSERVE_PUBLISH": "yes"})
	if r.err == nil {
		t.Fatalf("observe.sh passed with publish yes:\n%s", r.out)
	}
	if !strings.Contains(r.out, `publish is "yes"`) {
		t.Fatalf("message does not name the value:\n%s", r.out)
	}
	if len(r.log) != 0 {
		t.Fatalf("invalid publish made calls: %q", r.log)
	}
}

func TestTheActionChecksOutNothing(t *testing.T) {
	spectest.Proves(t, feature, "action-maps-inputs-to-flags", "the-action-checks-out-nothing")

	data, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if using := yaml.String(doc, "runs", "using"); using != "composite" {
		t.Fatalf("runs.using = %q, want composite", using)
	}
	steps, ok := yaml.Get(doc, "runs", "steps").([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("runs.steps = %v, want the artifact download and the observe step", yaml.Get(doc, "runs", "steps"))
	}
	if uses := yaml.String(steps[0], "uses"); uses != "actions/download-artifact@v4" {
		t.Fatalf("the first step uses %q, want the artifact download", uses)
	}
	observeStep := steps[1]
	if uses := yaml.String(observeStep, "uses"); uses != "" {
		t.Fatalf("the step uses %q, want a run step", uses)
	}
	if run := yaml.String(observeStep, "run"); run != `bash "$GITHUB_ACTION_PATH/observe.sh"` {
		t.Fatalf("the step runs %q", run)
	}
	if strings.Contains(string(data), "fetch-depth") {
		t.Fatal("action.yml sets a fetch depth")
	}

	inputs, _ := yaml.Get(doc, "inputs").(yaml.Map)
	env, _ := yaml.Get(observeStep, "env").(yaml.Map)
	var names []string
	for name := range inputs {
		names = append(names, name)
		variable := "PUTNAMI_OBSERVE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if got := yaml.String(env, variable); got != "${{ inputs."+name+" }}" {
			t.Fatalf("env %s = %q, want the input %s", variable, got, name)
		}
	}
	slices.Sort(names)
	if want := []string{"base", "base-sbom", "channel", "coverage", "from-artifact", "junit", "publish", "sbom", "token"}; !slices.Equal(names, want) {
		t.Fatalf("inputs = %q, want %q", names, want)
	}
	if got := yaml.String(env, "PUTNAMI_OBSERVE_GITHUB_TOKEN"); got != "${{ github.token }}" {
		t.Fatalf("env PUTNAMI_OBSERVE_GITHUB_TOKEN = %q, want the run's token", got)
	}
	if got := yaml.String(doc, "outputs", "evidence", "value"); got != "${{ steps.observe.outputs.evidence }}" {
		t.Fatalf("outputs.evidence.value = %q", got)
	}
}

// artifactDir is the directory the download step fills, holding the
// document a pull request run kept.
func artifactDir(t *testing.T) (dir, document string) {
	t.Helper()
	dir = t.TempDir()
	document = filepath.Join(dir, "observe-evidence.json")
	write(t, document, "{}\n", 0o644)
	return dir, document
}

func TestAFromArtifactStepDownloadsTheTriggeringRunArtifact(t *testing.T) {
	spectest.Proves(t, feature, "action-relays-a-fork-artifact", "a-from-artifact-step-downloads-the-triggering-run-artifact")

	data, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := yaml.Get(doc, "runs", "steps").([]any)
	if len(steps) != 2 {
		t.Fatalf("runs.steps = %v", steps)
	}
	download := steps[0]
	for _, want := range []struct {
		path  []string
		value string
	}{
		{[]string{"if"}, "${{ inputs.from-artifact != '' && github.event_name == 'workflow_run' }}"},
		{[]string{"uses"}, "actions/download-artifact@v4"},
		{[]string{"with", "name"}, "${{ inputs.from-artifact }}"},
		{[]string{"with", "run-id"}, "${{ github.event.workflow_run.id }}"},
		{[]string{"with", "github-token"}, "${{ github.token }}"},
		{[]string{"with", "path"}, "${{ runner.temp }}/observe-artifact"},
	} {
		if got := yaml.String(download, want.path...); got != want.value {
			t.Fatalf("download %v = %q, want %q", want.path, got, want.value)
		}
	}
	// A run that kept no artifact fails the download, not the job: the
	// observe step then warns.
	if got := yaml.Get(download, "continue-on-error"); got != true && got != "true" {
		t.Fatalf("download continue-on-error = %v, want true", got)
	}
	env, _ := yaml.Get(steps[1], "env").(yaml.Map)
	if got := yaml.String(env, "PUTNAMI_OBSERVE_ARTIFACT_DIR"); got != "${{ runner.temp }}/observe-artifact" {
		t.Fatalf("env PUTNAMI_OBSERVE_ARTIFACT_DIR = %q, want the download path", got)
	}
	if got := yaml.String(env, "PUTNAMI_OBSERVE_DOWNLOAD"); got != "${{ steps.artifact.outcome }}" {
		t.Fatalf("env PUTNAMI_OBSERVE_DOWNLOAD = %q, want the download outcome", got)
	}
	if got := yaml.String(env, "PUTNAMI_OBSERVE_EVENT"); got != "${{ github.event_name }}" {
		t.Fatalf("env PUTNAMI_OBSERVE_EVENT = %q", got)
	}
}

func TestAFromArtifactStepRelaysWithUploadAndPublish(t *testing.T) {
	spectest.Proves(t, feature, "action-relays-a-fork-artifact", "a-from-artifact-step-relays-with-upload-and-publish")

	dir, document := artifactDir(t)
	relay := map[string]string{
		"PUTNAMI_OBSERVE_FROM_ARTIFACT": "observe-evidence",
		"PUTNAMI_OBSERVE_EVENT":         "workflow_run",
		"PUTNAMI_OBSERVE_ARTIFACT_DIR":  dir,
	}
	r := runAction(t, relay)
	call := observeCall(t, r)
	if !strings.HasPrefix(call, "putnami observe --from-evidence "+document+" --upload --publish |") {
		t.Fatalf("call = %q, want --from-evidence with --upload --publish", call)
	}
	if !strings.HasSuffix(call, "| GITHUB_TOKEN=run-token PUTNAMI_CLOUD_TOKEN=unset INPUT_TOKEN=unset RUN_TOKEN=unset") {
		t.Fatalf("call = %q, want the run token only: the job's ID token is the upload credential", call)
	}
	expectCalls(t, r,
		"curl -fsSL https://putnami.dev/install.sh",
		"installer --version latest",
		"putnami extensions install --user @putnami/observe@latest",
		"putnami extensions install --user @putnami/cloud@latest",
		"putnami observe ",
	)
	if r.output != "evidence="+document+"\n" {
		t.Fatalf("GITHUB_OUTPUT = %q, want the relayed document", r.output)
	}

	relay["PUTNAMI_OBSERVE_PUBLISH"] = "false"
	relay["PUTNAMI_OBSERVE_TOKEN"] = "cloud-token"
	call = observeCall(t, runAction(t, relay))
	if !strings.Contains(call, " --upload |") || strings.Contains(call, "--publish") {
		t.Fatalf("publish false: call = %q, want --upload without --publish", call)
	}
	if !strings.HasSuffix(call, "| GITHUB_TOKEN=run-token PUTNAMI_CLOUD_TOKEN=cloud-token INPUT_TOKEN=unset RUN_TOKEN=unset") {
		t.Fatalf("publish false: call = %q, want the cloud token and the run token, which reads the pull request", call)
	}
}

func TestAFromArtifactStepWithoutADocumentWarnsAndPasses(t *testing.T) {
	spectest.Proves(t, feature, "action-relays-a-fork-artifact", "a-from-artifact-step-without-a-document-warns-and-passes")

	dir, document := artifactDir(t)
	empty := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(document, filepath.Join(linked, "observe-evidence.json")); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ event, dir, download, want string }{
		"a pull request job": {"pull_request", dir, "", "this job runs for pull_request"},
		"an empty artifact":  {"workflow_run", empty, "success", "holds no observe-evidence.json"},
		"a linked document":  {"workflow_run", linked, "success", "holds no observe-evidence.json"},
		"no download":        {"workflow_run", "", "", "holds no observe-evidence.json"},
		"a failed download":  {"workflow_run", dir, "failure", "failed, as the step above says"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runAction(t, map[string]string{
				"PUTNAMI_OBSERVE_FROM_ARTIFACT": "observe-evidence",
				"PUTNAMI_OBSERVE_EVENT":         tc.event,
				"PUTNAMI_OBSERVE_ARTIFACT_DIR":  tc.dir,
				"PUTNAMI_OBSERVE_DOWNLOAD":      tc.download,
			})
			if r.err != nil {
				t.Fatalf("the step failed: %v\n%s", r.err, r.out)
			}
			if !strings.Contains(r.out, "::warning title=Putnami Observe::") || !strings.Contains(r.out, tc.want) {
				t.Fatalf("output %q, want a warning with %q", r.out, tc.want)
			}
			if len(r.log) != 0 {
				t.Fatalf("calls %q, want none", r.log)
			}
		})
	}

	r := runAction(t, map[string]string{
		"PUTNAMI_OBSERVE_FROM_ARTIFACT": "observe-evidence",
		"PUTNAMI_OBSERVE_EVENT":         "workflow_run",
		"PUTNAMI_OBSERVE_ARTIFACT_DIR":  dir,
		"PUTNAMI_OBSERVE_JUNIT":         "reports/*.xml",
	})
	if r.err == nil || !strings.Contains(r.out, "takes no junit") || len(r.log) != 0 {
		t.Fatalf("a report input with from-artifact: err %v, calls %q, output %s", r.err, r.log, r.out)
	}
}

// TestTheActionMarksItsRunForTheNextLine proves putnami observe runs with the
// variable the summary's Next line reads to name the action's token input.
func TestTheActionMarksItsRunForTheNextLine(t *testing.T) {
	installer := filepath.Join(t.TempDir(), "install.sh")
	write(t, installer, `mkdir -p "$HOME/.putnami/bin"
cat >"$HOME/.putnami/bin/putnami" <<'PUTNAMI'
#!/bin/sh
printf 'putnami %s | action=%s\n' "$*" "${`+upload.ActionEnv+`-unset}" >>"$LOG"
PUTNAMI
chmod +x "$HOME/.putnami/bin/putnami"
`, 0o644)
	// The last FAKE_INSTALLER of the environment wins over runAction's own.
	r := runAction(t, map[string]string{"FAKE_INSTALLER": installer})
	call := observeCall(t, r)
	if !strings.HasSuffix(call, "| action=true") {
		t.Fatalf("putnami observe ran without %s=true: %q", upload.ActionEnv, call)
	}
	for _, line := range r.log {
		if strings.HasPrefix(line, "putnami extensions ") && !strings.HasSuffix(line, "| action=unset") {
			t.Fatalf("an install saw %s: %q", upload.ActionEnv, line)
		}
	}
}
