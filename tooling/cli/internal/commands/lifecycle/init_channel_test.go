package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// candidateChannel is the immutable channel of a release candidate, as a
// tagged publish names it.
const candidateChannel = "tooling-v0.4.0"

// noInstallRecord makes the running CLI name record no channel, whatever the
// test binary is called, and clears the variable a host may export.
func noInstallRecord(t *testing.T) {
	t.Helper()
	installedAs(t, "lifecycle.test")
}

// installedAs makes name the file name the running CLI resolves to, and clears
// the variable a host may export.
func installedAs(t *testing.T, name string) {
	t.Helper()
	original := runningCLIName
	runningCLIName = func() string { return name }
	t.Cleanup(func() { runningCLIName = original })
	t.Setenv(initChannelEnv, "")
}

func TestParseInitFlagsReadsTheChannel(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want initFlags
	}{
		{name: "a separate value", args: []string{"--channel", "canary"},
			want: initFlags{extension: "ts", channel: "canary", channelSet: true}},
		{name: "an attached value", args: []string{"--channel=canary", "--project", "app"},
			want: initFlags{extension: "ts", project: "app", channel: "canary", channelSet: true}},
		{name: "an attached empty value", args: []string{"--channel="},
			want: initFlags{extension: "ts", channelSet: true}},
		{name: "no value", args: []string{"--project", "app", "--channel"},
			want: initFlags{extension: "ts", project: "app", channelSet: true}},
		{name: "no flag", args: []string{"--project", "app"},
			want: initFlags{extension: "ts", project: "app"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseInitFlags(tt.args); got != tt.want {
				t.Fatalf("parseInitFlags(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// The flag wins over the variable, the variable over the install record, and
// the install record over latest. stable reads latest, as in `putnami upgrade`.
func TestResolveInitChannelPrecedence(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "the-flag-wins-over-the-variable-and-the-install-record")
	tests := []struct {
		name       string
		flags      initFlags
		variable   string
		executable string
		want       initChannel
	}{
		{name: "nothing chooses", executable: "putnami",
			want: initChannel{name: "latest"}},
		{name: "the install record", executable: "putnami-go-canary",
			want: initChannel{name: "canary", origin: initChannelFromInstall}},
		{name: "the variable wins over the install record", variable: candidateChannel, executable: "putnami-go-canary",
			want: initChannel{name: candidateChannel, origin: initChannelFromEnv}},
		{name: "the flag wins over the variable and the install record",
			flags: initFlags{channel: "preview", channelSet: true}, variable: candidateChannel, executable: "putnami-go-canary",
			want: initChannel{name: "preview", origin: initChannelFromFlag}},
		{name: "the flag chooses latest over a recorded channel",
			flags: initFlags{channel: "latest", channelSet: true}, executable: "putnami-go-canary",
			want: initChannel{name: "latest", origin: initChannelFromFlag}},
		{name: "stable by flag reads latest",
			flags: initFlags{channel: "stable", channelSet: true}, variable: candidateChannel,
			want: initChannel{name: "latest", origin: initChannelFromFlag}},
		{name: "stable by variable reads latest", variable: "stable", executable: "putnami-go-canary",
			want: initChannel{name: "latest", origin: initChannelFromEnv}},
		{name: "an empty variable chooses nothing", variable: "", executable: "putnami-go-canary",
			want: initChannel{name: "canary", origin: initChannelFromInstall}},
		{name: "surrounding blanks are not part of the name", variable: "  canary\n", executable: "putnami",
			want: initChannel{name: "canary", origin: initChannelFromEnv}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == initChannelEnv {
					return tt.variable
				}
				return ""
			}
			got, err := resolveInitChannel(tt.flags, getenv, tt.executable)
			if err != nil || got != tt.want {
				t.Fatalf("resolveInitChannel() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}

	if got := (initChannel{name: "latest", origin: initChannelFromFlag}).selected(); got != "" {
		t.Fatalf("latest selects %q, want no channel: a run on latest sends the requests a run without a channel sends", got)
	}
	if got := (initChannel{name: "canary"}).selected(); got != "canary" {
		t.Fatalf("canary selects %q, want canary", got)
	}
}

// A flag or a variable that names no channel, a name that is not a safe
// channel token, and an exact version are usage errors that name the source.
func TestResolveInitChannelRefusesUnsafeNamesAndVersions(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "an-unsafe-name-or-a-version-is-refused-before-anything-is-written")
	refused := []struct {
		value string
		want  string
	}{
		{"", "needs a channel name"},
		{"   ", "needs a channel name"},
		{"../canary", "invalid channel"},
		{"a/b", "invalid channel"},
		{"a b", "invalid channel"},
		{"-canary", "invalid channel"},
		{".canary", "invalid channel"},
		{"canary@1", "invalid channel"},
		{"canary?os=linux", "invalid channel"},
		{"canary&channel=latest", "invalid channel"},
		{"canary%2f", "invalid channel"},
		{"canäry", "invalid channel"},
		{strings.Repeat("c", 129), "invalid channel"},
		{"1.2.3", "not on an exact version"},
		{"v1.2.3", "not on an exact version"},
		{"0.1.0-feb66161", "not on an exact version"},
		{"v0.4.0-rc.1+build.7", "not on an exact version"},
	}
	for _, tt := range refused {
		t.Run("flag "+tt.value, func(t *testing.T) {
			got, err := resolveInitChannel(initFlags{channel: tt.value, channelSet: true}, func(string) string { return "canary" }, "putnami-go-canary")
			if err == nil || !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), initChannelFlag) {
				t.Fatalf("resolveInitChannel(--channel %q) = %+v, %v; want a usage error that says %q and names %s", tt.value, got, err, tt.want, initChannelFlag)
			}
		})
		if tt.value == "" {
			// An empty variable is an unset one, not a refused one.
			continue
		}
		t.Run("variable "+tt.value, func(t *testing.T) {
			getenv := func(string) string { return tt.value }
			got, err := resolveInitChannel(initFlags{}, getenv, "putnami-go-canary")
			if err == nil || !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), initChannelEnv) {
				t.Fatalf("resolveInitChannel(%s=%q) = %+v, %v; want a usage error that says %q and names %s", initChannelEnv, tt.value, got, err, tt.want, initChannelEnv)
			}
		})
	}

	accepted := []string{"canary", "latest", candidateChannel, "tooling-v0.4.0-rc.1", "typescript-v1.2.3", "Preview_2", "a", strings.Repeat("c", 128)}
	for _, value := range accepted {
		got, err := resolveInitChannel(initFlags{channel: value, channelSet: true}, func(string) string { return "" }, "putnami")
		if err != nil || got.name != value {
			t.Errorf("resolveInitChannel(--channel %q) = %+v, %v; want the channel accepted as given", value, got, err)
		}
	}
}

// The install record is the tag of the name the installers give the CLI,
// putnami-<variant>-<tag>, when that tag is a channel.
func TestInstallRecordChannel(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "the-install-record-is-the-channel-in-the-installed-name")
	tests := []struct {
		executable string
		goos       string
		want       string
	}{
		// What install.sh leaves for a channel: putnami -> putnami-<variant>-<channel>.
		{"putnami-go-canary", "linux", "canary"},
		{"putnami-ts-canary", "darwin", "canary"},
		{"putnami-go-" + candidateChannel, "linux", candidateChannel},
		{"putnami-go-latest", "linux", "latest"},
		{"putnami-go-stable", "linux", "latest"},
		// A version install and `putnami upgrade` name the binary after a version.
		{"putnami-go-1.2.3", "linux", ""},
		{"putnami-go-v1.2.3", "linux", ""},
		{"putnami-go-0.1.0-feb66161", "linux", ""},
		{"putnami-go-v0.4.0-rc.1", "darwin", ""},
		// `putnami upgrade --from-source` and a local build.
		{"putnami-go-source-0123abcd", "linux", ""},
		{"putnami-go-dev", "darwin", ""},
		// Names the installers do not write.
		{"putnami", "linux", ""},
		{"putnami-go-", "linux", ""},
		{"putnami-rust-canary", "linux", ""},
		{"putnami-go-can ary", "linux", ""},
		{"xputnami-go-canary", "linux", ""},
		{"lifecycle.test", "linux", ""},
		{"", "linux", ""},
		// On Windows the active putnami.exe is a copy: its name records nothing.
		{"putnami.exe", "windows", ""},
		// A versioned Windows binary run by its own name still records its channel.
		{"putnami-go-canary.exe", "windows", "canary"},
		{"putnami-go-canary.EXE", "windows", "canary"},
		{"putnami-go-1.2.3.exe", "windows", ""},
	}
	for _, tt := range tests {
		if got := installRecordChannel(tt.executable, tt.goos); got != tt.want {
			t.Errorf("installRecordChannel(%q, %s) = %q, want %q", tt.executable, tt.goos, got, tt.want)
		}
	}
}

// The running CLI name is the name its links resolve to: the installer points
// `putnami` at putnami-<variant>-<tag>, and the tag is read from the target.
func TestRunningCLINameResolvesLinks(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := runningCLIName(), filepath.Base(resolved); got != want {
		t.Fatalf("runningCLIName() = %q, want %q", got, want)
	}
}

// initStageCalls records what init hands each of its stages.
type initStageCalls struct {
	extensions [][]string
	templates  [][]string
	// installs and creates are the channel each dependency install and each
	// project create ran with.
	installs []string
	creates  []string
	// params are the job options each dependency install sends its installers.
	params []map[string]any
}

// recordInitStages replaces every stage of init with one that records what it
// received and succeeds.
func recordInitStages(t *testing.T) *initStageCalls {
	t.Helper()
	stubWorkspaceInitStages(t, "", 0)
	calls := &initStageCalls{}
	installExtension = func(_ context.Context, _ string, _ *wsproto.Config, names []string, _ string) error {
		calls.extensions = append(calls.extensions, slices.Clone(names))
		return nil
	}
	installInitTemplate = func(_ context.Context, _ string, _ *wsproto.Config, names []string, _ string) error {
		calls.templates = append(calls.templates, slices.Clone(names))
		return nil
	}
	installInitDependencies = func(_ context.Context, _ string, _ *wsproto.Config, _, _ string, env LifecycleEnv) error {
		calls.installs = append(calls.installs, env.channel)
		calls.params = append(calls.params, env.installParams())
		return nil
	}
	createInitProject = func(_ context.Context, _ string, _ *wsproto.Config, _ []string, _ bool, env LifecycleEnv) error {
		calls.creates = append(calls.creates, env.channel)
		return nil
	}
	return calls
}

// A chosen channel is the resolution target of every stage of the run: the
// language extension, the agent-content extension, the template, the project
// create and both dependency installs. The workspace config keeps bare names.
func TestWorkspaceInitResolvesEveryStageOnTheChannel(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "every-stage-of-init-receives-the-channel")
	choices := []struct {
		name     string
		args     []string
		variable string
		origin   string
	}{
		{name: "by flag", args: []string{"--channel", candidateChannel}, origin: initChannelFromFlag},
		{name: "by attached flag", args: []string{"--channel=" + candidateChannel}, origin: initChannelFromFlag},
		{name: "by variable", variable: candidateChannel, origin: initChannelFromEnv},
	}
	for _, choice := range choices {
		t.Run(choice.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := recordInitStages(t)
			// AdoptAgentWorkflows reads the lock, which the recording install
			// does not write: the adoption reports nothing to adopt.
			starterOptsInto(t, "go", "@putnami/contributor")
			noInstallRecord(t)
			t.Setenv(initChannelEnv, choice.variable)

			args := append([]string{"--workspace", "channel-ws", "--project", "app", "--extension", "go"}, choice.args...)
			output, _ := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", args, LifecycleEnv{})
			})

			wantExtensions := [][]string{{"@putnami/go@" + candidateChannel}, {"@putnami/contributor@" + candidateChannel}}
			if !reflect.DeepEqual(calls.extensions, wantExtensions) {
				t.Errorf("extension installs = %v, want %v", calls.extensions, wantExtensions)
			}
			if want := [][]string{{"go-server@" + candidateChannel}}; !reflect.DeepEqual(calls.templates, want) {
				t.Errorf("template installs = %v, want %v", calls.templates, want)
			}
			if want := []string{candidateChannel, candidateChannel}; !slices.Equal(calls.installs, want) {
				t.Errorf("dependency installs ran on %q, want %q", calls.installs, want)
			}
			for _, params := range calls.params {
				if want := map[string]any{initChannelJobOption: candidateChannel}; !reflect.DeepEqual(params, want) {
					t.Errorf("installer job options = %v, want %v", params, want)
				}
			}
			if want := []string{candidateChannel}; !slices.Equal(calls.creates, want) {
				t.Errorf("project create ran on %q, want %q", calls.creates, want)
			}
			if want := "Channel: " + candidateChannel + " (" + choice.origin + ")"; !strings.Contains(output, want) {
				t.Errorf("init output does not state its channel %q:\n%s", want, output)
			}

			data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), candidateChannel) {
				t.Errorf("the workspace config names the channel:\n%s", data)
			}
			config := readWorkspaceConfigAt(dir)
			if got := strings.Join(config.Extensions.Names(), ","); got != "@putnami/contributor,@putnami/go" {
				t.Errorf("workspace extensions = %s, want the two bare names", got)
			}
			for name, constraint := range config.Extensions.List {
				if constraint != "" {
					t.Errorf("extension %s is declared with the constraint %q, want none", name, constraint)
				}
			}
		})
	}
}

// Without a choice, and with a choice of latest, every stage receives what it
// receives from an init that knows no channel: bare names and no job option.
func TestWorkspaceInitWithoutAChoiceSendsWhatItSentBefore(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "no-choice-is-unchanged", "no-choice-hands-every-stage-bare-names-and-no-option")
	choices := []struct {
		name       string
		args       []string
		variable   string
		executable string
	}{
		{name: "no choice and no install record", executable: "putnami"},
		{name: "a version install", executable: "putnami-go-1.2.3"},
		{name: "a CLI installed from latest", executable: "putnami-go-latest"},
		{name: "latest by flag", args: []string{"--channel", "latest"}, executable: "putnami-go-canary"},
		{name: "stable by variable", variable: "stable", executable: "putnami-go-canary"},
	}
	for _, choice := range choices {
		t.Run(choice.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := recordInitStages(t)
			starterOptsInto(t, "go", "@putnami/contributor")
			installedAs(t, choice.executable)
			t.Setenv(initChannelEnv, choice.variable)

			args := append([]string{"--workspace", "channel-ws", "--project", "app", "--extension", "go"}, choice.args...)
			output, _ := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", args, LifecycleEnv{})
			})

			if want := [][]string{{"@putnami/go"}, {"@putnami/contributor"}}; !reflect.DeepEqual(calls.extensions, want) {
				t.Errorf("extension installs = %v, want %v", calls.extensions, want)
			}
			if want := [][]string{{"go-server"}}; !reflect.DeepEqual(calls.templates, want) {
				t.Errorf("template installs = %v, want %v", calls.templates, want)
			}
			if want := []string{"", ""}; !slices.Equal(calls.installs, want) {
				t.Errorf("dependency installs ran on %q, want no channel", calls.installs)
			}
			for _, params := range calls.params {
				if params != nil {
					t.Errorf("installer job options = %v, want none", params)
				}
			}
			if want := []string{""}; !slices.Equal(calls.creates, want) {
				t.Errorf("project create ran on %q, want no channel", calls.creates)
			}
			if strings.Contains(output, "Channel:") {
				t.Errorf("init on latest announces a channel:\n%s", output)
			}
		})
	}
}

// A CLI installed from a channel initializes on that channel when nothing else
// chooses one, and the flag and the variable override the record.
func TestWorkspaceInitDefaultsToTheChannelTheCLIWasInstalledFrom(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "init-defaults-to-the-install-record")
	tests := []struct {
		name     string
		args     []string
		variable string
		want     string
		origin   string
	}{
		{name: "the record alone", want: "canary", origin: initChannelFromInstall},
		{name: "the variable overrides it", variable: candidateChannel, want: candidateChannel, origin: initChannelFromEnv},
		{name: "the flag overrides both", args: []string{"--channel", "preview"}, variable: candidateChannel,
			want: "preview", origin: initChannelFromFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := recordInitStages(t)
			installedAs(t, "putnami-go-canary")
			t.Setenv(initChannelEnv, tt.variable)

			args := append([]string{"--workspace", "channel-ws", "--extension", "go"}, tt.args...)
			output, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", args, LifecycleEnv{})
			})
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, output)
			}
			if want := [][]string{{"@putnami/go@" + tt.want}}; !reflect.DeepEqual(calls.extensions, want) {
				t.Errorf("extension installs = %v, want %v", calls.extensions, want)
			}
			if want := "Channel: " + tt.want + " (" + tt.origin + ")"; !strings.Contains(output, want) {
				t.Errorf("init output does not state %q:\n%s", want, output)
			}
		})
	}
}

// A channel init cannot resolve on is refused before the workspace config, the
// repository or any other file exists, and before any stage runs.
func TestWorkspaceInitRefusesAnInvalidChannelBeforeWritingAnything(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "an-unsafe-name-or-a-version-is-refused-before-anything-is-written")
	tests := []struct {
		name     string
		args     []string
		variable string
	}{
		{name: "a flag with no value", args: []string{"--channel"}},
		{name: "an empty flag", args: []string{"--channel="}},
		{name: "a path by flag", args: []string{"--channel", "../canary"}},
		{name: "a version by flag", args: []string{"--channel", "1.2.3"}},
		{name: "a query by variable", variable: "canary&channel=latest"},
		{name: "a version by variable", variable: "v0.4.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			calls := recordInitStages(t)
			noInstallRecord(t)
			t.Setenv(initChannelEnv, tt.variable)

			args := append([]string{"--workspace", "channel-ws", "--project", "app", "--extension", "go"}, tt.args...)
			output, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", args, LifecycleEnv{})
			})
			if err == nil || !errors.Is(err, cmderr.ErrUsage) {
				t.Fatalf("WorkspaceInit = %v, want a usage error\n%s", err, output)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Errorf("a refused init left %v behind, want nothing written", names)
			}
			if len(calls.extensions)+len(calls.templates)+len(calls.installs)+len(calls.creates) != 0 {
				t.Errorf("a refused init ran stages: %+v", calls)
			}
		})
	}
}

// The workspace installers receive the channel as a job option, the way
// `putnami upgrade` hands its target to the deps-upgrade job, and so does the
// fetch a hosted run starts with. An install with no channel sends the
// requests it sends without one.
func TestDepsInstallHandsTheChannelToTheInstallers(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "the-installers-receive-the-channel-as-a-job-option")
	tests := []struct {
		name    string
		bearer  string
		channel string
		want    map[string]map[string]any
	}{
		{name: "a local install on a channel", channel: candidateChannel,
			want: map[string]map[string]any{"workspace-install": {"putnami-channel": candidateChannel}}},
		{name: "a hosted install on a channel", bearer: "run-bearer", channel: candidateChannel,
			want: map[string]map[string]any{
				extensionproto.WorkspaceFetchCommand: {"putnami-channel": candidateChannel},
				"workspace-install":                  {"putnami-channel": candidateChannel},
			}},
		{name: "a local install without a channel",
			want: map[string]map[string]any{"workspace-install": nil}},
		{name: "a hosted install without a channel", bearer: "run-bearer",
			want: map[string]map[string]any{extensionproto.WorkspaceFetchCommand: nil, "workspace-install": nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(runcredential.SetForTest(tt.bearer))
			t.Setenv(artifactsEnsuredEnv, "")
			t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
			got := map[string]map[string]any{}
			env := LifecycleEnv{Out: io.Discard, channel: tt.channel,
				RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
					got[req.Job] = req.Params
					return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
				}}
			if err := DepsInstall(context.Background(), t.TempDir(), &wsproto.Config{}, "", "", env); err != nil {
				t.Fatalf("DepsInstall: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("job options by job = %v, want %v", got, tt.want)
			}
			for job, params := range got {
				if tt.channel == "" && params != nil {
					t.Errorf("%s got the options %v, want a nil bag: the request an install sends without a channel", job, params)
				}
			}
		})
	}
}

// `putnami upgrade` with no selector follows latest whatever chose the channel
// of an init: the variable and the install record are read by init alone.
func TestUpgradeFollowsLatestWhateverChoseTheInitChannel(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "the-workspace-pins-no-channel", "upgrade-follows-latest-by-default")
	installedAs(t, "putnami-go-"+candidateChannel)
	t.Setenv(initChannelEnv, candidateChannel)

	selector, err := resolveUpgradeSelector(UpgradeFlags{})
	if err != nil {
		t.Fatalf("resolveUpgradeSelector: %v", err)
	}
	if selector.Mode != "channel" || selector.RegistryTarget != "latest" || selector.DepsTarget != "latest" {
		t.Fatalf("upgrade with no selector resolves %+v, want the latest channel on every registry", selector)
	}
}
