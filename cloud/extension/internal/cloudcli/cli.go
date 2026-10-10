package cloudcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	identityapi "go.putnami.dev/cloud/clients/identity-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	identitycli "go.putnami.dev/cloud/extension/internal/identitycli"
	observabilitycli "go.putnami.dev/cloud/extension/internal/observabilitycli"
	runtimecli "go.putnami.dev/cloud/extension/internal/runtimecli"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

type eventState struct {
	resultEmitted bool
	// failure is the command's terminal error, recorded before any terminal
	// event is emitted so the FAILED result event can carry a structured cause.
	// Without it the invoking CLI has no `error` member to read off the event
	// and falls back to the subprocess's own wait error — which is how a whole
	// shell harness's verdict was reduced to `exit status 1`.
	failure error
	// stderrTail retains a bounded slice of ordinary stderr logs so the single
	// terminal diagnostic can carry useful subprocess evidence without turning
	// every progress line (for example `go: downloading`) into a gate error.
	stderrTail []string
	// emitFailureDiagnostic is installed by putnamiEventIO. RunMain calls it once
	// after RunCommand returns an error, keeping diagnostic severity tied to the
	// terminal verdict instead of to the file descriptor a process wrote to.
	emitFailureDiagnostic func(error)
}

const (
	cloudRuntimeIdentity = "@putnami/cloud"
	cloudCLIContract     = 4
	cacheOAuthClientID   = "cache"
	cacheAllowedScopes   = "cache.read cache.write"
	// cloudRuntimeProtocol is the runtime EVENT protocol this extension can
	// speak, taken from the protocol package rather than typed here so the
	// handshake cannot claim a version putnamiEventIO does not emit.
	// A contract-3 extension is required to read the invoker's advertisement
	// and answer at it, and the invoking CLI accepts exactly the version it
	// advertises: a hand-written version makes the CLI discard every event this
	// extension emits before any renderer sees it.
	cloudRuntimeProtocol = runtimeproto.MaxKnownProtocolVersion
	cloudRuntimeABI      = runtimeproto.RuntimeABIVersion

	runtimeStderrTailLines        = 16
	runtimeStderrTailLineRunes    = 256
	runtimeFailureMessageRunes    = 4096
	runtimeFailureDiagnosticRunes = 8192
)

// cloudRuntimeVersion is a variable so @putnami/go can stamp immutable release
// versions into packaged binaries with -X. Source-checkout runtimes keep the
// manifest version as their default identity.
var cloudRuntimeVersion = "0.1.0"

// runtimeInfoResponse is the identity document a contract-v3 runtime returns
// before the core schedules any Cloud task. Keep it independent from workspace
// context and runtime events: the resolver calls it before either exists.
type runtimeInfoResponse struct {
	Extension       string `json:"extension"`
	Version         string `json:"version"`
	Platform        string `json:"platform"`
	CLIContract     int    `json:"cliContract"`
	RuntimeProtocol int    `json:"runtimeProtocol"`
	RuntimeABI      int    `json:"runtimeABI"`
}

var setupAtomicWrite = clicore.WriteFileAtomic

func RunMain(argv []string, ioctx IO) int {
	hasContext := hasArg(argv, "--putnamiContext")
	env := envOrProcess(ioctx.Env)
	// PUTNAMI_INTERACTIVE=1 means the parent CLI ran us as a structured command
	// with the live job renderer bypassed. The context file is still passed for
	// workspace/params metadata, but we must write plain stdout/stderr — no
	// JSONL events — or progress chrome would leak into the user's terminal.
	interactive := env["PUTNAMI_INTERACTIVE"] == "1"
	putnamiMode := hasContext && !interactive
	state := &eventState{}
	runtimeIO := ioctx
	if putnamiMode {
		runtimeIO = putnamiEventIO(ioctx, state, negotiatedEventVersion(env))
	}

	command := "help"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		command = argv[0]
	}

	args := argv
	if command != "help" && len(argv) > 0 {
		args = argv[1:]
	}
	resultParams := resultParamsFor(command, args, env)

	confirm := firstNonNil(runtimeIO.Confirm, ioctx.Confirm)
	if confirm == nil {
		// Parent-dispatched commands forward stdin,
		// so the default prompt works under native dispatch too. defaultConfirm
		// still reports ok=false on non-interactive stdin, which keeps reveal
		// flows fail-fast (--yes required) in CI and JSONL mode.
		confirm = func(question string) (string, bool) {
			return defaultConfirm(question, env)
		}
	}
	prompt := firstNonNil(runtimeIO.Prompt, ioctx.Prompt)
	if prompt == nil {
		prompt = func(question string) (string, bool) {
			return defaultPrompt(question, env)
		}
	}

	commandIO := IO{
		Env:         env,
		Stdout:      firstNonNil(runtimeIO.Stdout, ioctx.Stdout, func(line string) { fmt.Println(line) }),
		Stderr:      firstNonNil(runtimeIO.Stderr, ioctx.Stderr, func(line string) { fmt.Fprintln(os.Stderr, line) }),
		JSON:        runtimeIO.JSON,
		Artifact:    runtimeIO.Artifact,
		Phase:       runtimeIO.Phase,
		Progress:    runtimeIO.Progress,
		TTY:         runtimeIO.TTY,
		Confirm:     confirm,
		Prompt:      prompt,
		OpenBrowser: firstNonNil(runtimeIO.OpenBrowser, ioctx.OpenBrowser),
		Context:     firstNonNil(runtimeIO.Context, ioctx.Context),
		Sleep:       firstNonNil(runtimeIO.Sleep, ioctx.Sleep),
		Client:      clientOrDefault(ioctx.Client),
		Now:         nowOrDefault(ioctx.Now),
	}
	err := RunCommand(command, commandIO, args)
	if err == nil {
		if putnamiMode && !state.resultEmitted {
			runtimeIO.Result("OK", nil)
		}
		return ExitSuccess
	}

	code := clicore.ExitCode(err)
	message := err.Error()
	if putnamiMode {
		// Record the cause before anything terminal is emitted: both terminal
		// paths below render a FAILED result event, and an event that says only
		// "FAILED" leaves the invoking CLI to describe the failure from the
		// process exit status alone.
		state.failure = err
		if state.emitFailureDiagnostic != nil {
			state.emitFailureDiagnostic(err)
		}
		if !state.resultEmitted {
			clicore.WriteErrorResult(err, resultParams, runtimeIO)
		}
		if !state.resultEmitted {
			runtimeIO.Result("FAILED", nil)
		}
	} else if shouldEmitStructuredFailure(resultParams) {
		// Use the same normalized IO passed to RunCommand. Standalone invocations
		// commonly arrive with a zero IO and rely on the default stdout/stderr
		// sinks above; passing the raw IO here made structured failures panic while
		// trying to call a nil Stdout function.
		clicore.WriteErrorResult(err, resultParams, commandIO)
		if !clicore.StructuredOutput(resultParams) {
			firstNonNil(ioctx.Stderr, func(line string) { fmt.Fprintln(os.Stderr, line) })(message)
		}
	} else {
		firstNonNil(ioctx.Stderr, func(line string) { fmt.Fprintln(os.Stderr, line) })(message)
	}
	return code
}

func (s *eventState) rememberStderr(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	line = boundRunes(line, runtimeStderrTailLineRunes)
	if len(s.stderrTail) < runtimeStderrTailLines {
		s.stderrTail = append(s.stderrTail, line)
		return
	}
	copy(s.stderrTail, s.stderrTail[1:])
	s.stderrTail[len(s.stderrTail)-1] = line
}

func (s *eventState) failureDiagnostic(err error) string {
	message := "command failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = boundRunes(strings.TrimSpace(err.Error()), runtimeFailureMessageRunes)
	}
	var evidence []string
	for _, line := range s.stderrTail {
		if !strings.Contains(message, line) {
			evidence = append(evidence, line)
		}
	}
	if len(evidence) > 0 {
		message += "\nstderr tail:\n" + strings.Join(evidence, "\n")
	}
	return boundRunes(message, runtimeFailureDiagnosticRunes)
}

func boundRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func RunCommand(command string, ioctx IO, args []string) error {
	if command == "__putnami" {
		return runtimeInfo(args, ioctx)
	}

	env := envOrProcess(ioctx.Env)
	context, err := loadContext(args, env)
	if err != nil {
		return err
	}
	params := mergeParams(context.Params, parseFlags(args))
	params["command"] = commandPath(command, args)
	if err := clicore.ValidateOutputMode(params); err != nil {
		return err
	}
	// Surface the native CLI's cwd-resolved project as the default app, so
	// `cd <workload> && putnami cloud <verb>` matches the ergonomics of every
	// other putnami command without an explicit --app. Skipped when --app is
	// set or when the resolved "project" is the workspace itself.
	adoptContextProject(params, context)
	workspaceRoot := context.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot = envGet(env, "PUTNAMI_WORKSPACE_ROOT")
	}
	if workspaceRoot == "" {
		// Walk up from cwd looking for the workspace's cloud-link. This is
		// what makes `cd <workload> && putnami cloud …` work the same way
		// the native `putnami` CLI commands do — without it the user has to
		// either run from the workspace root or set PUTNAMI_WORKSPACE_ROOT
		// every time.
		cwd, _ := os.Getwd()
		if root, ok := findCloudLinkRoot(cwd); ok {
			workspaceRoot = root
		} else {
			workspaceRoot = cwd
		}
	}

	ioctx.Env = env
	ioctx.Stdout = firstNonNil(ioctx.Stdout, func(string) {})
	ioctx.Stderr = firstNonNil(ioctx.Stderr, func(string) {})
	ioctx.Client = clientOrDefault(ioctx.Client)
	ioctx.Now = nowOrDefault(ioctx.Now)

	args, answered, err := rootHelpArgs(params, ioctx, command, args)
	if answered {
		return err
	}

	switch command {
	case "help", "":
		return help(params, ioctx)

	// The public root: one entry per thing the user knows.
	case "login":
		return login(params, workspaceRoot, env, ioctx)
	case "logout":
		return logout(params, env, ioctx)
	case "whoami":
		return whoami(params, workspaceRoot, env, ioctx)
	case "setup":
		return setup(params, workspaceRoot, env, ioctx)
	case "status":
		return cloudStatus(params, args, workspaceRoot, env, ioctx)
	case "token":
		return routeToken(params, args, workspaceRoot, env, ioctx)
	case "registries":
		return distributioncli.Registries(params, args, env, ioctx)
	case "packages":
		return routePackages(params, args, workspaceRoot, env, ioctx)
	case "channels":
		return routeChannels(params, args, workspaceRoot, env, ioctx)
	case "ci":
		return deliverycli.CI(params, args, workspaceRoot, env, ioctx)
	case "cache":
		return deliverycli.Cache(params, args, workspaceRoot, env, ioctx)
	case "source":
		return routeSource(params, args, workspaceRoot, env, ioctx)
	case "config":
		return routeConfig(params, args, workspaceRoot, env, ioctx)
	case "secrets":
		return configcli.Secrets(params, args, workspaceRoot, env, ioctx)
	case "db":
		return routeDB(params, args, workspaceRoot, env, ioctx)
	case "env":
		return routeEnv(params, args, workspaceRoot, env, ioctx)
	case "deploy":
		return runtimecli.Deploy(params, args, workspaceRoot, env, ioctx)
	case "logs":
		return observabilitycli.Logs(params, args, workspaceRoot, env, ioctx)
	case "traces":
		return observabilitycli.Traces(params, args, workspaceRoot, env, ioctx)
	case "metrics":
		return observabilitycli.Metrics(params, args, workspaceRoot, env, ioctx)

	// Hidden aliases: every retired root name still runs (hiddenAliases).
	case "tokens":
		return machineTokens(params, args, workspaceRoot, env, ioctx)
	case "distribution":
		return distributioncli.Distribution(params, args, workspaceRoot, env, ioctx)
	case "oci":
		return distributioncli.OCI(params, args, workspaceRoot, env, ioctx)
	case "publish-archives":
		return publishArchives(params, args, workspaceRoot, env, ioctx)
	case "track":
		return deliverycli.Track(params, args, workspaceRoot, env, ioctx)
	case "report":
		return deliverycli.Report(params, args, workspaceRoot, env, ioctx)
	case "validate-config":
		return validateConfig(params, args, workspaceRoot, ioctx)
	case "publish-config":
		return publishConfig(params, args, workspaceRoot, env, ioctx)
	case "publish-migration":
		return publishMigration(params, args, workspaceRoot, env, ioctx)

	// Platform maintenance lives in the operator CLI.
	case "sql-proxy", "publish-doc":
		return movedToOperator(command, operatorCommands[command])

	// Machine commands: hidden, callable, protocol names (machineCommands).
	case "install":
		return install(params, workspaceRoot, env, ioctx)
	case "release-set":
		return distributioncli.ReleaseSet(params, args, workspaceRoot, env, ioctx)
	case "registry-token":
		return distributioncli.RegistryToken(params, workspaceRoot, env, ioctx)
	case "publish-provider":
		return distributioncli.PublishProvider(params, env, ioctx)
	case "cache-provider":
		return deliverycli.CacheProvider(params, args, workspaceRoot, env, ioctx)
	case "credential-provider":
		return credentialProvider(params, workspaceRoot, env, ioctx)
	case "session-reporter":
		return deliverycli.SessionReporter(params, args, workspaceRoot, env, ioctx)
	case "log-reporter":
		return deliverycli.LogReporter(params, args, workspaceRoot, env, ioctx)
	case "image-layers":
		return deliverycli.ImageLayers(params, args, workspaceRoot, env, ioctx)
	case "shell-test":
		return deliverycli.ShellTest(params, args, workspaceRoot, env, ioctx)
	case "validate-config-project":
		return validateConfigProject(params, args, workspaceRoot, ioctx)
	case "package-config-member":
		return packageConfigMember(params, args, workspaceRoot, ioctx)
	case "package-site-content":
		return distributioncli.PackageSiteContent(params, args, workspaceRoot, ioctx)
	case "validate-site-content":
		return distributioncli.ValidateSiteContentSources(params, args, workspaceRoot, ioctx)
	case "publish-site-content":
		return publishSiteContent(params, args, workspaceRoot, env, ioctx)
	case "publish-deployment":
		return publishDeployment(params, args, workspaceRoot, env, ioctx)
	default:
		return newError("unknown @putnami/cloud command: "+command, ExitUsage)
	}
}

func emitArchivePublishedMember(ioctx IO, published *distributioncli.ArchivePublishResult) error {
	if published == nil || ioctx.Artifact == nil {
		return nil
	}
	member := &extensionproto.PublishedMember{
		Ecosystem:      string(cloudArchiveEcosystem),
		Coordinate:     published.Package,
		Version:        published.Version,
		ArtifactDigest: published.ArtifactDigest,
		Platforms:      published.Platforms,
	}
	if diagnostics := extensionproto.ValidatePublishedMember(member); len(diagnostics) != 0 {
		return fmt.Errorf("refuse invalid archive publication result: %s", diagnostics[0].String())
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return fmt.Errorf("encode archive publication result: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return fmt.Errorf("encode archive publication event data: %w", err)
	}
	return ioctx.Artifact(
		"archive-"+strings.NewReplacer("/", "-", "@", "").Replace(published.Package),
		published.Package,
		extensionproto.PublishedMemberEventKind,
		"",
		data,
	)
}

// runtimeInfo implements the reserved runtime handshake. It deliberately
// bypasses context loading and event emission because the core probes an
// extension before it has selected a workspace, task, or event protocol.
func runtimeInfo(args []string, ioctx IO) error {
	if len(args) != 1 || args[0] != "runtime-info" {
		return newError("unknown @putnami/cloud internal runtime command; expected \"runtime-info\"", ExitUsage)
	}

	data, err := json.Marshal(runtimeInfoResponse{
		Extension:       cloudRuntimeIdentity,
		Version:         cloudRuntimeVersion,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     cloudCLIContract,
		RuntimeProtocol: cloudRuntimeProtocol,
		RuntimeABI:      cloudRuntimeABI,
	})
	if err != nil {
		return fmt.Errorf("encode @putnami/cloud runtime descriptor: %w", err)
	}
	firstNonNil(ioctx.Stdout, func(string) {})(string(data))
	return nil
}

// help lists the public root: exactly the entries of publicEntries. Hidden
// aliases, machine commands and operator commands never appear.
func help(params map[string]any, ioctx IO) error {
	commands := make([]map[string]string, 0, len(publicEntries))
	for _, entry := range publicEntries {
		commands = append(commands, map[string]string{
			"command":     "cloud " + entry.Name,
			"usage":       "cloud " + entry.Usage,
			"description": entry.Description,
		})
	}
	if clicore.StructuredOutput(params) {
		writeResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	width := 0
	for _, entry := range publicEntries {
		width = max(width, len(entry.Name))
	}
	ioctx.Stdout("Usage: putnami cloud <command> [flags]")
	ioctx.Stdout("")
	ioctx.Stdout("Commands:")
	for _, entry := range publicEntries {
		ioctx.Stdout(fmt.Sprintf("  %-*s  %s", width, entry.Name, entry.Description))
	}
	ioctx.Stdout("")
	ioctx.Stdout("Run `putnami cloud <command> help` for the verbs of one command.")
	return nil
}

func resultParamsFor(command string, args []string, env map[string]string) map[string]any {
	context, err := loadContext(args, env)
	if err != nil {
		context = putnamiContext{Params: map[string]any{}}
	}
	params := mergeParams(context.Params, parseFlags(args))
	params["command"] = commandPath(command, args)
	return params
}

func commandPath(command string, _ []string) string {
	if command == "" {
		command = "help"
	}
	return "cloud " + command
}

func shouldEmitStructuredFailure(params map[string]any) bool {
	mode, err := clicore.ResolveOutputMode(params)
	if err == nil {
		return mode.IsStructured()
	}
	raw := strings.TrimSpace(clicore.StringParam(params, "output"))
	return truthy(param(params, "json")) || raw == string(protocolcli.OutputJSON) || raw == string(protocolcli.OutputJSONL)
}

func login(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	res, err := identitycli.Login(params, env, ioctx)
	if err != nil {
		return err
	}
	// After authenticating, provision the local per-registry credential recipes.
	// This is cross-domain orchestration owned by the login *command*, layered on
	// top of the identity mechanism — identitycli never reaches into registries.
	state, err := distributioncli.WriteRegistryTokenRecipes(params, env)
	if err != nil {
		return err
	}
	// login is an explicit, user-initiated action, so — unlike install/setup —
	// it also migrates the session off any legacy static native credential
	// (~/.npmrc, ~/.netrc, Docker config) left by an older CLI.
	if err := distributioncli.ScrubLegacyNativeRegistryCredentials(params, env, state); err != nil {
		return err
	}
	result := map[string]any{
		"authenticated": true,
		"user":          res.User,
		"token_type":    res.Auth.TokenType,
		"expires_at":    res.Auth.ExpiresAt,
		"auth_url":      res.Auth.Issuer,
		"registries":    distributioncli.RegistriesSummary(state),
	}
	followup := workspaceSetupFollowup(workspaceRoot)
	if !truthy(param(params, "json")) && ioctx.TTY != nil {
		ioctx.TTY("\n  " + res.Message + "\n")
		if followup != "" {
			ioctx.TTY("  " + followup + "\n")
		}
	}
	writeResult(result, params, ioctx, res.Message)
	if followup != "" && !truthy(param(params, "json")) {
		ioctx.Stdout(followup)
	}
	return nil
}

func logout(params map[string]any, env map[string]string, ioctx IO) error {
	endpoints := authEndpoints(params, env, ioctx.Client, "")
	clientID := stringParam(params, "client-id", "clientId")
	if clientID == "" {
		clientID = "putnami-cli"
	}
	auth, err := readAuth(env, false)
	if err != nil {
		return err
	}
	// Order matters: revoke registry-scoped pkt_* keys BEFORE the OAuth
	// refresh token, because revoking the refresh token also invalidates
	// the access-token we'd need to call /apikeys. A missing access
	// token (already-expired session) means we can only delete the local
	// files — server-side keys stay until the user runs logout from a
	// session that can still authenticate.
	accessToken := ""
	if auth != nil {
		accessToken = auth.AccessToken
	}
	if rerr := distributioncli.TeardownRegistries(params, env, ioctx, accessToken); rerr != nil {
		ioctx.Stderr("warning: failed to tear down registry credentials: " + rerr.Error())
	}
	if auth != nil && auth.RefreshToken != "" {
		_, err := clicore.PostRevocationEndpoint(ioctx.Client, endpoints.RevocationURL, map[string]any{
			"token":           auth.RefreshToken,
			"token_type_hint": "refresh_token",
			"client_id":       clientID,
		}, []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound})
		if err != nil {
			ioctx.Stderr("warning: remote token revocation failed: " + err.Error())
		}
	}
	if err := removeAuth(env); err != nil {
		return err
	}
	writeResult(map[string]any{"authenticated": false}, params, ioctx, identitycli.SignedOutMessage)
	return nil
}

// whoami is `putnami cloud whoami`: the session on this machine and the
// workspace this repository is linked to, as one status.
func whoami(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	return identitycli.WhoAmI(params, workspaceRoot, env, ioctx)
}

func setup(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	// --auto is the idempotent "ensure" mode run during workspace install:
	// it configures Cloud only when this repo
	// already knows its workspace and the user is signed in, it never creates a
	// workspace, and it never fails the surrounding install. Plain `cloud setup`
	// keeps the interactive auto-create-and-link behavior and surfaces errors.
	auto := boolParam(params, false, "auto")

	linkedWorkspace, err := setupLinkedWorkspace(params, workspaceRoot)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	workspaceID, err := setupWorkspaceID(params, linkedWorkspace)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	if auto {
		if workspaceID == "" {
			return autoSetupSkip(params, ioctx, "no linked Putnami Cloud workspace; run `putnami cloud setup` to create and link one")
		}
		if !hasStoredAuth(env) {
			return autoSetupSkip(params, ioctx, "not signed in to Putnami Cloud; run `putnami cloud login`, then `putnami cloud setup`")
		}
	}

	// --slug names the workspace that setup creates (setup_slug.go). With no
	// workspace to link, setup creates one: the slug is checked here, before
	// the session and any request.
	slug, err := setupSlugParam(params)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	if slug != "" && workspaceID == "" {
		if err := validateNewWorkspaceSlug(slug); err != nil {
			return autoFail(auto, params, ioctx, err)
		}
	}

	cacheEnabled := boolParam(params, true, "cache")
	var cacheCfg *deliverycli.CacheConfig
	if cacheEnabled {
		cacheCfg, err = deliverycli.BuildCacheConfig(params, env)
		if err != nil {
			return autoFail(auto, params, ioctx, err)
		}
	}

	auth, err := activeAuth(params, env, ioctx)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	controlURL := controlPlaneBaseURL(params, env, valueString(linkedWorkspace, "control_plane_url"))
	if workspaceID != "" && !clicore.IsWorkspaceID(workspaceID) {
		// --workspace <slug|name>: resolve it once, here, so the link records
		// the id every other command and scope_ref check keys on.
		workspaceID, err = clicore.LookupWorkspaceID(params, env, ioctx, linkedWorkspace, workspaceID)
		if err != nil {
			return autoFail(auto, params, ioctx, err)
		}
	}

	// Linking reads, and creation writes, the workspace through identity-api's
	// generated client (identity_calls.go): identity-api serves /v1/workspaces.
	var workspace *identityapi.Workspace2
	if workspaceID != "" {
		// Link existing workspace: verify it exists and capture its metadata.
		workspace, err = readWorkspace(ioctx.Client, controlURL, auth.AccessToken, workspaceID)
		if err != nil {
			return autoFail(auto, params, ioctx, err)
		}
		if err := checkLinkedWorkspaceSlug(slug, workspaceID, workspace); err != nil {
			return autoFail(auto, params, ioctx, err)
		}
	} else {
		// Auto-create: derive a workspace name from putnami.json and POST it.
		// Unreachable under --auto (guarded above), so an install hook never
		// silently spins up a workspace. organization_id is intentionally
		// omitted so the control-plane resolves the caller's personal scope
		// (see the self-bootstrap path in iam handlers). --organization
		// overrides the default for users who want the workspace under a
		// non-personal org.
		name := stringParam(params, "workspace-name", "workspaceName")
		if name == "" {
			name = deriveWorkspaceName(workspaceRoot)
		}
		if name == "" {
			return newError("cloud setup requires --workspace <id> or a workspace name (set putnami.json's \"name\" or pass --workspace-name)", ExitUsage)
		}
		request := identityapi.CreateWorkspaceRequest{Name: &name}
		if org := stringParam(params, "organization", "organizationId"); org != "" {
			request.OrganizationId = &org
		}
		if repo := stringParam(params, "repository"); repo != "" {
			request.Repository = &repo
		}
		// Without --slug the request carries no slug, and identity-api derives
		// one from the workspace id.
		if slug != "" {
			request.Slug = &slug
		}
		workspace, err = createWorkspace(ioctx.Client, controlURL, auth.AccessToken, request)
		if err != nil {
			return err
		}
		workspaceID = clicore.Deref(workspace.Id)
		if workspaceID == "" {
			return newError("workspace creation returned no id", ExitAPI)
		}
	}

	linkData := map[string]any{
		"version":           1,
		"control_plane_url": controlURL,
		"workspace_id":      workspaceID,
		"workspace_name":    firstString(stringParam(params, "workspace-name", "workspaceName"), clicore.Deref(workspace.Name)),
		"environment":       firstString(stringParam(params, "environment"), "prod"),
		"repository":        firstString(stringParam(params, "repository"), clicore.Deref(workspace.Repository)),
		"linked_at":         ioctx.Now().UTC().Format(time.RFC3339Nano),
	}
	// Intelligence is the @putnami/intelligence extension's concern: its own
	// `putnami intelligence setup` enables the agent wiring and reports hosted
	// availability. Cloud setup links the workspace and nothing else of it.
	commitSetup, err := prepareSetupLink(workspaceRoot, linkData)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	if err := commitSetup(); err != nil {
		return autoFail(auto, params, ioctx, err)
	}

	result := map[string]any{"configured": true, "workspace": linkData, "path": linkPath(workspaceRoot)}
	message := fmt.Sprintf("Configured Putnami Cloud workspace %s (%s).", workspaceID, linkData["environment"])

	// Setup is the one-stop "configure this repository for Cloud" entrypoint:
	// it provisions every workspace-local feature, the remote build cache
	// included. The cache config carries only a token *source* (never a raw
	// bearer), so it is safe to commit. --no-cache opts out and leaves any
	// existing .putnami/cache.json untouched.
	if cacheEnabled {
		if err := deliverycli.WriteCacheConfig(workspaceRoot, cacheCfg); err != nil {
			return autoFail(auto, params, ioctx, err)
		}
		cache := deliverycli.CacheSummary(cacheCfg, deliverycli.CachePath(workspaceRoot))
		result["cache"] = cache
		message += fmt.Sprintf(" Build cache enabled at %s (mode %s).", cache["url"], cache["mode"])
	} else {
		result["cache"] = map[string]any{"enabled": false, "skipped": true}
	}
	state, err := distributioncli.WriteRegistryTokenRecipes(params, env)
	if err != nil {
		return autoFail(auto, params, ioctx, err)
	}
	result["registries"] = distributioncli.RegistriesSummary(state)

	writeResult(result, params, ioctx, message)
	return nil
}

func install(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	link, ok, err := readManifestLink(workspaceRoot)
	if err != nil {
		return err
	}
	if !ok {
		// Installation replays declared local state only. Even an existing
		// session or an injected --workspace must never create or select a link.
		return autoSetupSkip(params, ioctx, "no linked Putnami Cloud workspace; continue locally. If hosted capabilities help this task, offer explicit workspace setup once; it may be deferred")
	}

	linkData := normalizedManifestLink(link)
	if err := writeLinkCache(workspaceRoot, linkData); err != nil {
		return err
	}
	cacheWritten := false
	if boolParam(params, true, "cache") {
		cacheCfg, err := deliverycli.ReadCacheConfig(workspaceRoot)
		if err != nil {
			return err
		}
		if cacheCfg == nil || truthy(param(params, "force")) {
			cacheCfg, err = deliverycli.BuildCacheConfig(params, env)
			if err != nil {
				return err
			}
			if err := deliverycli.WriteCacheConfig(workspaceRoot, cacheCfg); err != nil {
				return err
			}
			cacheWritten = true
		}
	}

	state, err := distributioncli.WriteRegistryTokenRecipes(params, env)
	if err != nil {
		return err
	}
	result := map[string]any{
		"configured":       true,
		"workspace":        linkData,
		"path":             linkPath(workspaceRoot),
		"cache_configured": cacheWritten,
		"registries":       distributioncli.RegistriesSummary(state),
	}
	writeResult(result, params, ioctx, "Putnami Cloud local setup is ready.")
	return nil
}

func normalizedManifestLink(link map[string]any) map[string]any {
	return map[string]any{
		"version":           1,
		"control_plane_url": firstString(valueString(link, "control_plane_url"), DefaultControlPlaneURL),
		"workspace_id":      valueString(link, "workspace_id"),
		"workspace_name":    valueString(link, "workspace_name"),
		"environment":       firstString(valueString(link, "environment"), "prod"),
		"repository":        valueString(link, "repository"),
		"linked_at":         valueString(link, "linked_at"),
	}
}

// autoFail downgrades a setup error to a non-fatal skip under --auto so
// automation never breaks setup on a transient failure (offline, expired
// session, control-plane hiccup). Without --auto the error is returned
// unchanged.
func autoFail(auto bool, params map[string]any, ioctx IO, err error) error {
	if err == nil || !auto {
		return err
	}
	return autoSetupSkip(params, ioctx, err.Error())
}

// autoSetupSkip reports a no-op `cloud setup --auto` run and returns nil. The
// reason is surfaced as the human message and the reason field of the JSON
// envelope.
func autoSetupSkip(params map[string]any, ioctx IO, reason string) error {
	writeResult(map[string]any{
		"configured": false,
		"skipped":    true,
		"reason":     reason,
	}, params, ioctx, "Putnami Cloud setup skipped: "+reason)
	return nil
}

// hasStoredAuth reports whether a Cloud credential exists locally, without a
// network round-trip, so `cloud setup --auto` can skip cleanly on an
// unauthenticated checkout instead of erroring.
func hasStoredAuth(env map[string]string) bool {
	auth, err := readAuth(env, false)
	return err == nil && auth != nil && auth.AccessToken != ""
}

func setupLinkedWorkspace(params map[string]any, workspaceRoot string) (map[string]any, error) {
	if link, ok, err := workspaceLinkParam(params, "workspace", "workspaceId"); ok || err != nil {
		return link, err
	}
	link, ok, err := readManifestLink(workspaceRoot)
	if err != nil || !ok {
		return nil, err
	}
	return link, nil
}

func setupWorkspaceID(params map[string]any, link map[string]any) (string, error) {
	if id, ok, err := workspaceIDParam(params, "workspace", "workspaceId"); ok || err != nil {
		return id, err
	}
	if link == nil {
		return "", nil
	}
	id := strings.TrimSpace(valueString(link, "workspace_id"))
	if id == "" {
		return "", newError("Putnami Cloud workspace config missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	return id, nil
}

func workspaceLinkParam(params map[string]any, names ...string) (map[string]any, bool, error) {
	value := param(params, names...)
	if value == nil {
		return nil, false, nil
	}
	link, ok := value.(map[string]any)
	if !ok {
		return nil, true, nil
	}
	if strings.TrimSpace(valueString(link, "workspace_id")) == "" {
		return nil, true, newError("Putnami Cloud workspace config missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	return link, true, nil
}

func workspaceIDParam(params map[string]any, names ...string) (string, bool, error) {
	value := param(params, names...)
	if value == nil {
		return "", false, nil
	}
	if link, ok := value.(map[string]any); ok {
		id := strings.TrimSpace(valueString(link, "workspace_id"))
		if id == "" {
			return "", true, newError("Putnami Cloud workspace config missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
		}
		return id, true, nil
	}
	return strings.TrimSpace(stringValue(value)), true, nil
}

func workspaceToken(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	purpose := strings.ToLower(strings.TrimSpace(stringParam(params, "for")))
	if purpose == "" && truthy(param(params, "for-registry", "forRegistry")) {
		purpose = "registry"
	}
	if param(params, "for-cache", "forCache") != nil {
		return newError("unknown cloud token flag --for-cache; use `--for cache`", ExitUsage)
	}
	// The unscoped operator bearer (--global) left the public CLI for
	// `putnami operator token --global`.
	if truthy(param(params, "global")) {
		return movedToOperator("token --global", "putnami operator token --global")
	}
	// Registry token recipes use --for npm|go|oci|put ("registry" stays accepted
	// as the historical alias for put). They live under `cloud token` so
	// token-like surfaces have one API, while every TokenSource consumer still
	// receives a bare bearer on stdout. The kind is the WHOLE target: the
	// registry derives what this principal may read or write per namespace.
	if _, ok := distributioncli.RegistryFromTokenPurpose(purpose); ok {
		return distributioncli.RegistryToken(params, workspaceRoot, env, ioctx)
	}
	if purpose != "" && purpose != "cache" {
		return newError(fmt.Sprintf("unknown cloud token purpose %q; expected cache, npm, go, oci, or put", purpose), ExitUsage)
	}
	link, err := readCloudLink(workspaceRoot)
	if err != nil {
		return err
	}
	workspaceID := stringValue(link["workspace_id"])
	if purpose == "cache" {
		auth, err := clicore.MintWorkspaceScopedAuth(params, env, ioctx, workspaceID, cacheOAuthClientID, cacheAllowedScopes)
		if err != nil {
			return err
		}
		// --for cache is the token *source* the build cache config records. The
		// contract (.putnami/cache.json `token.command`) is "trimmed stdout is the
		// bearer", so emit the bare least-privilege access token on its own line and
		// nothing else — no status message or JSON envelope in any output mode.
		ioctx.Stdout(auth.AccessToken)
		return nil
	}
	auth, err := workspaceAuth(params, env, ioctx, workspaceID)
	if err != nil {
		return err
	}
	out := map[string]any{
		"workspace_id":      workspaceID,
		"control_plane_url": stringValue(link["control_plane_url"]),
		"access_token":      auth.AccessToken,
		"token_type":        auth.TokenType,
		"expires_at":        auth.ExpiresAt,
	}
	writeResult(out, params, ioctx, "Minted cloud token for workspace "+workspaceID+".")
	return nil
}

// deriveWorkspaceName reads the workspace's display name from
// putnami.workspace.json (monorepo root) or, failing that, putnami.json
// (single-project root). Returns "" when neither file is present or
// parseable — the caller decides whether to fall back or error out.
func deriveWorkspaceName(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	for _, file := range []string{"putnami.workspace.json", "putnami.json"} {
		data, err := os.ReadFile(filepath.Join(workspaceRoot, file))
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		if manifest.Name != "" {
			return manifest.Name
		}
	}
	return ""
}

func workspaceSetupFollowup(workspaceRoot string) string {
	if workspaceRoot == "" || manifestLinkPath(workspaceRoot) == "" || hasWorkspaceLink(workspaceRoot) {
		return ""
	}
	return "Next: run `putnami cloud setup` to configure this repository for Putnami Cloud."
}

func hasWorkspaceLink(workspaceRoot string) bool {
	if link, ok, err := readManifestLink(workspaceRoot); err == nil && ok {
		return stringValue(link["workspace_id"]) != ""
	}
	data, err := os.ReadFile(linkPath(workspaceRoot))
	if err != nil {
		return false
	}
	var link map[string]any
	if err := json.Unmarshal(data, &link); err != nil {
		return false
	}
	return stringValue(link["workspace_id"]) != "" && stringValue(link["control_plane_url"]) != ""
}

type stagedSetupFile struct {
	path         string
	data         []byte
	mode         os.FileMode
	original     []byte
	originalMode os.FileMode
	existed      bool
}

// prepareSetupLink validates and renders the manifest and compatibility link
// cache without mutating the repository. The returned commit writes both
// atomically per file and restores the manifest if the second write fails.
func prepareSetupLink(workspaceRoot string, linkData map[string]any) (func() error, error) {
	manifest := manifestLinkPath(workspaceRoot)
	if manifest == "" {
		return nil, errors.New("cloud setup requires a root putnami.workspace.json or putnami.json")
	}
	manifestData, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	manifestOut, err := updateManifestLinkJSON(manifestData, linkData)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifest, err)
	}
	linkDataJSON, err := json.MarshalIndent(linkData, "", "  ")
	if err != nil {
		return nil, err
	}
	files := []stagedSetupFile{
		{path: manifest, data: manifestOut, mode: 0o644},
		{path: linkPath(workspaceRoot), data: append(linkDataJSON, '\n'), mode: 0o644},
	}
	for index := range files {
		current, readErr := os.ReadFile(files[index].path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("stage %s: %w", files[index].path, readErr)
		}
		info, statErr := os.Stat(files[index].path)
		if statErr != nil {
			return nil, fmt.Errorf("stage %s: %w", files[index].path, statErr)
		}
		files[index].original = current
		files[index].originalMode = info.Mode().Perm()
		files[index].existed = true
	}
	return func() error {
		changed := make([]int, 0, len(files))
		rollback := func() {
			for index := len(changed) - 1; index >= 0; index-- {
				file := files[changed[index]]
				if file.existed {
					_ = clicore.WriteFileAtomic(file.path, file.original, file.originalMode)
				} else {
					_ = os.Remove(file.path)
				}
			}
		}
		for index, file := range files {
			if file.existed && bytes.Equal(file.original, file.data) {
				continue
			}
			if err := setupAtomicWrite(file.path, file.data, file.mode); err != nil {
				rollback()
				return fmt.Errorf("write %s: %w", file.path, err)
			}
			changed = append(changed, index)
		}
		return nil
	}, nil
}

func writeLinkCache(workspaceRoot string, linkData map[string]any) error {
	file := linkPath(workspaceRoot)
	data, err := json.MarshalIndent(linkData, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically (temp file + fsync + rename): a killed `install`/`setup`
	// must never leave a partial cloud-link.json for the cache provider's token
	// command to choke on. WriteFileAtomic does the MkdirAll. Non-secret file,
	// 0644.
	return clicore.WriteFileAtomic(file, append(data, '\n'), 0o644)
}

func manifestLinkPath(workspaceRoot string) string {
	return clicore.ManifestPath(workspaceRoot)
}

// updateManifestLinkJSON writes the workspace link to the manifest's
// options.@putnami/cloud.workspace and leaves every other byte as written.
func updateManifestLinkJSON(data []byte, linkData map[string]any) ([]byte, error) {
	workspaceValue, err := marshalManifestWorkspaceLink(linkData)
	if err != nil {
		return nil, err
	}
	return clicore.UpsertJSONField(data, []string{"options", "@putnami/cloud"}, "workspace", workspaceValue)
}

// marshalManifestWorkspaceLink renders the workspace link committed to
// putnami.workspace.json. It is intentionally minimal: only the data needed to
// connect this repository to its Cloud workspace — the workspace id and, when
// it differs from the public default, the control-plane URL. Everything else
// (environment, repository, display name, link timestamp, schema version) is
// resolved from the control plane or defaulted at use time, so it stays out of
// the source-controlled manifest and is only mirrored to the local cache.
func marshalManifestWorkspaceLink(linkData map[string]any) ([]byte, error) {
	type manifestWorkspaceLink struct {
		WorkspaceID     string `json:"workspace_id"`
		ControlPlaneURL string `json:"control_plane_url,omitempty"`
	}
	controlPlaneURL := stringValue(linkData["control_plane_url"])
	if controlPlaneURL == DefaultControlPlaneURL {
		controlPlaneURL = ""
	}
	return json.MarshalIndent(manifestWorkspaceLink{
		WorkspaceID:     stringValue(linkData["workspace_id"]),
		ControlPlaneURL: controlPlaneURL,
	}, "", "  ")
}

// findCloudLinkRoot walks up from start until it finds a directory containing
// .putnami/cloud-link.json (i.e. a linked workspace root). Returns the
// directory + true on hit; "" + false when the filesystem root is reached
// without finding one. Lets `cd <subdir> && putnami cloud …` work the same
// way `cd <subdir> && putnami build` does — anchored on the workspace, not
// on the cwd literally.
func findCloudLinkRoot(start string) (string, bool) {
	if start == "" {
		return "", false
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, LinkFileRelative)); err == nil {
			return dir, true
		}
		if _, ok, err := readManifestLink(dir); err == nil && ok {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// adoptContextProject surfaces the native CLI's resolved active project
// (context.project, resolved from the invocation cwd exactly as `putnami
// build` resolves it) as the default app. This is the primary resolution
// path under native dispatch — the cloud commands aren't project-scoped in
// the extension manifest, so the native CLI doesn't forward a bare
// positional, but it always passes the cwd-resolved project in the context.
//
// Skipped when --app / --appName is already set, or when the resolved
// project IS the workspace itself (invocation from the workspace root, not
// inside a workload). The native CLI conflates context.workspace.name with
// context.project.name when run from a workload, so we can't compare those
// two; instead we read the workspace's real name from putnami.workspace.json
// at the workspace root and skip when the resolved project matches it.
// Mutates params in place.
func adoptContextProject(params map[string]any, ctx putnamiContext) {
	if _, set := params["app"]; set {
		return
	}
	if _, set := params["appName"]; set {
		return
	}
	proj := ctx.Project.Name
	if proj == "" {
		return
	}
	if proj == workspaceName(ctx.WorkspaceRoot) {
		return
	}
	// A project-scoped task can be selected with an unqualified tail such as
	// `putnami publish core`. The native scheduler expands that selector to
	// every matching project, but its task context still carries the original
	// shorthand. Do not turn an ambiguous shorthand into --app: the task runs
	// from {projectRoot}, so ResolveApp can recover that task's canonical
	// putnami.json name from its cwd instead.
	if !strings.Contains(proj, "/") {
		canonical, err := clicore.CanonicalProjectName(ctx.WorkspaceRoot, proj)
		if err != nil {
			return
		}
		proj = canonical
	}
	params["app"] = proj
	params["_contextApp"] = proj
}

// workspaceName reads the `name` field from putnami.workspace.json at the
// workspace root. Empty when the file is absent or unparseable — callers
// treat an empty result as "couldn't confirm it's the workspace," which is
// safe because the only consumer (adoptContextProject) just won't skip.
func workspaceName(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.workspace.json"))
	if err != nil {
		return ""
	}
	var ws map[string]any
	if err := json.Unmarshal(data, &ws); err != nil {
		return ""
	}
	return stringValue(ws["name"])
}

type putnamiContext struct {
	WorkspaceRoot string         `json:"workspaceRoot"`
	Params        map[string]any `json:"params"`
	// Project is the active project the native putnami CLI resolved from the
	// invocation cwd (the same resolution `putnami build` / `putnami test`
	// use). Name is the workload's putnami.json `name` when invoked from
	// inside a workload, or the workspace's own name when invoked from the
	// workspace root.
	Project struct {
		Name string `json:"name"`
	} `json:"project"`
}

func loadContext(argv []string, env map[string]string) (putnamiContext, error) {
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--putnamiContext" && i+1 < len(argv) {
			data, err := os.ReadFile(argv[i+1])
			if err != nil {
				return putnamiContext{}, err
			}
			var ctx putnamiContext
			if err := json.Unmarshal(data, &ctx); err != nil {
				return putnamiContext{}, err
			}
			if ctx.Params == nil {
				ctx.Params = map[string]any{}
			}
			return ctx, nil
		}
	}
	return putnamiContext{WorkspaceRoot: envGet(env, "PUTNAMI_WORKSPACE_ROOT"), Params: map[string]any{}}, nil
}

// negotiatedEventVersion resolves the runtime event protocol version every line
// of this process's session stream is stamped with.
//
// The invoking CLI advertises the version it accepts in PUTNAMI_RUNTIME_EVENTS
// and — since it began requiring extension contract 3 — accepts EXACTLY that
// version: a lower-versioned line means the stream disagrees with the manifest,
// so it is dropped rather than interpreted. The resolution is total (absent,
// blank, or unparsable all mean v1, the version every consumer has always
// understood), which is why a directly invoked binary and an older CLI still
// get a stream they can read.
func negotiatedEventVersion(env map[string]string) int {
	return runtimeproto.NegotiatedVersionFromEnv(func(key string) string { return env[key] })
}

// resultEventError renders the `error` member the invoking CLI reads off a
// FAILED result event to describe the task. It prefers the structured result
// envelope's own error and falls back to the terminal error the command
// returned, because the two terminal paths in RunMain reach here with only one
// or the other in hand.
func resultEventError(resultErr *protocolcli.ResultError, failure error) map[string]any {
	if resultErr != nil {
		member := map[string]any{"message": resultErr.Message}
		if resultErr.Code != "" {
			member["code"] = resultErr.Code
		}
		return member
	}
	if failure != nil {
		return map[string]any{"message": failure.Error()}
	}
	return nil
}

func putnamiEventIO(base IO, state *eventState, eventVersion int) IO {
	stdout := firstNonNil(base.Stdout, func(line string) { fmt.Println(line) })
	emitter := runtimeproto.NewEmitterForVersion(eventLineWriter{stdout: stdout}, eventVersion)
	tty := base.TTY
	if tty == nil {
		tty = writeTTY
	}
	state.emitFailureDiagnostic = func(err error) {
		emitEvent(stdout, map[string]any{
			"v":        eventVersion,
			"type":     "diagnostic",
			"time":     eventTime(),
			"severity": "error",
			"message":  state.failureDiagnostic(err),
		})
	}
	return IO{
		Env:         base.Env,
		Client:      base.Client,
		Now:         base.Now,
		Confirm:     base.Confirm,
		Prompt:      base.Prompt,
		OpenBrowser: base.OpenBrowser,
		Context:     base.Context,
		Sleep:       base.Sleep,
		Artifact: func(id, name, kind, path string, data map[string]any) error {
			return emitter.ArtifactData(id, name, kind, path, data)
		},
		Stdout: func(line string) {
			if line == "" {
				return
			}
			emitEvent(stdout, map[string]any{
				"v":       eventVersion,
				"type":    "log",
				"time":    eventTime(),
				"level":   "info",
				"message": line,
			})
		},
		Stderr: func(line string) {
			if line == "" {
				return
			}
			state.rememberStderr(line)
			emitEvent(stdout, map[string]any{
				"v":       eventVersion,
				"type":    "log",
				"time":    eventTime(),
				"level":   "error",
				"message": line,
			})
		},
		JSON: func(data any) {
			status := "OK"
			level := "info"
			message := "Job OK"
			payloadData := data
			var resultErr *protocolcli.ResultError
			if result, ok := data.(protocolcli.ResultV2); ok {
				// v2 splits the non-success verdict into "failure" and
				// "aborted"; the job event stream has one not-OK status, so
				// anything that is not a success reports FAILED.
				if result.Status != protocolcli.StatusSuccess {
					status = "FAILED"
					level = "error"
					message = "Job FAILED"
				}
				resultErr = result.Error
				payloadData = result.Data
				if payloadData == nil && result.Error != nil {
					payloadData = map[string]any{"error": result.Error, "exitCode": result.ExitCode}
				}
			}
			payload := map[string]any{"status": status, "data": payloadData}
			if status == "FAILED" {
				if cause := resultEventError(resultErr, state.failure); cause != nil {
					payload["error"] = cause
				}
			}
			emitEvent(stdout, map[string]any{
				"v":       eventVersion,
				"type":    "result",
				"time":    eventTime(),
				"level":   level,
				"message": message,
				"data":    payload,
			})
			state.resultEmitted = true
		},
		Phase: func(name string) {
			emitEvent(stdout, map[string]any{
				"v":      eventVersion,
				"type":   "phase",
				"time":   eventTime(),
				"name":   name,
				"action": "start",
			})
		},
		Progress: func(current, total float64, message string) {
			emitEvent(stdout, map[string]any{
				"v":       eventVersion,
				"type":    "progress",
				"time":    eventTime(),
				"current": current,
				"total":   total,
				"message": message,
			})
		},
		TTY: tty,
		Result: func(status string, data any) {
			level := "info"
			if status == "FAILED" {
				level = "error"
			}
			payload := map[string]any{"status": status}
			if data != nil {
				payload["data"] = data
			}
			if status == "FAILED" {
				if cause := resultEventError(nil, state.failure); cause != nil {
					payload["error"] = cause
				}
			}
			emitEvent(stdout, map[string]any{
				"v":       eventVersion,
				"type":    "result",
				"time":    eventTime(),
				"level":   level,
				"message": "Job " + status,
				"data":    payload,
			})
			state.resultEmitted = true
		},
	}
}

// eventLineWriter adapts the runtime protocol's JSONL writer to the extension
// process's line sink. Runtime emitters write one complete JSON document per
// call; rejecting an embedded newline keeps that framing exact.
type eventLineWriter struct {
	stdout func(string)
}

func (w eventLineWriter) Write(data []byte) (int, error) {
	original := len(data)
	data = bytes.TrimSuffix(data, []byte{'\n'})
	if len(data) == 0 || bytes.ContainsRune(data, '\n') {
		return 0, errors.New("runtime emitter wrote an invalid JSONL frame")
	}
	w.stdout(string(data))
	return original, nil
}

func writeTTY(text string) {
	if runtime.GOOS == "windows" {
		return
	}
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(text)
}

func emitEvent(stdout func(string), event map[string]any) {
	data, _ := json.Marshal(event)
	stdout(string(data))
}

func eventTime() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func firstNonNil[T any](values ...T) T {
	for _, value := range values {
		reflected := reflect.ValueOf(value)
		if !reflected.IsValid() {
			continue
		}
		switch reflected.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if reflected.IsNil() {
				continue
			}
		}
		return value
	}
	var zero T
	return zero
}

// ttyDevice is the controlling-terminal device defaultConfirm reads the
// answer from when stdin is interactive. Overridable in tests so they can
// exercise the os.Stdin fallback without blocking on the real terminal.
var ttyDevice = "/dev/tty"

func defaultConfirm(question string, env map[string]string) (string, bool) {
	return defaultPrompt(question, env)
}

func defaultPrompt(question string, env map[string]string) (string, bool) {
	if !stdinIsInteractive(env) {
		return "", false
	}
	if runtime.GOOS != "windows" && ttyDevice != "" {
		if tty, err := os.OpenFile(ttyDevice, os.O_RDWR, 0); err == nil {
			defer tty.Close()
			if _, err := tty.WriteString(question); err != nil {
				return "", false
			}
			return readConfirmAnswer(tty)
		}
	}
	if _, err := fmt.Fprint(os.Stderr, question); err != nil {
		return "", false
	}
	return readConfirmAnswer(os.Stdin)
}

func readConfirmAnswer(input io.Reader) (string, bool) {
	reader := bufio.NewReader(input)
	var answer strings.Builder
	for answer.Len() < 1024 {
		ch, err := reader.ReadByte()
		if err != nil {
			if answer.Len() == 0 {
				return "", false
			}
			break
		}
		if ch == '\n' || ch == '\r' {
			break
		}
		answer.WriteByte(ch)
	}
	return strings.TrimSpace(answer.String()), true
}

func stdinIsInteractive(env map[string]string) bool {
	if env["PUTNAMI_INTERACTIVE"] == "1" {
		return true
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func hasArg(argv []string, value string) bool {
	for _, arg := range argv {
		if arg == value {
			return true
		}
	}
	return false
}

func envOrProcess(env map[string]string) map[string]string {
	if env != nil {
		return env
	}
	out := map[string]string{}
	for _, item := range os.Environ() {
		if idx := strings.IndexByte(item, '='); idx >= 0 {
			out[item[:idx]] = item[idx+1:]
		}
	}
	return out
}

func clientOrDefault(client *http.Client) *http.Client {
	// withUserAgent also resolves a nil client to http.DefaultClient, and
	// hardens the shared client so every request through it carries the unified
	// CLI User-Agent even if a future request builder forgets setUserAgent.
	return withUserAgent(client)
}

func nowOrDefault(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}
