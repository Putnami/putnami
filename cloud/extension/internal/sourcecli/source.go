// Package sourcecli holds the source-domain command implementations for the
// @putnami/cloud CLI extension: connect, status and disconnect, the
// self-service, workspace-scoped journey. The break-glass bind and unbind by
// numeric ids live in the operator CLI.
package sourcecli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.putnami.dev/client"
	sourceapiclient "go.putnami.dev/cloud/clients/source-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

const githubSourcePath = "/source/github"

var sourceOriginRemote = func(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Source dispatches the `putnami cloud source <sub>` family.
func Source(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	sub := clicore.FirstPositional(args)
	switch sub {
	case "", "help":
		return sourceHelp(params, ioctx)
	case "connect":
		return sourceConnect(params, workspaceRoot, env, ioctx)
	case "status":
		return sourceStatus(params, workspaceRoot, env, ioctx)
	case "disconnect":
		return sourceDisconnect(params, workspaceRoot, env, ioctx)
	default:
		return clicore.NewError("unknown source subcommand: "+sub, clicore.ExitUsage)
	}
}

func sourceHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud source connect [--repo owner/name] [--replace]", "description": "connect a GitHub repository with browser-assisted authorization"},
		{"command": "cloud source status [--strict]", "description": "check the GitHub repository connection, its health and permissions, and count the open pull requests; exits 1 when the installation is broken, and under --strict when anything is not ok"},
		{"command": "cloud source disconnect [--yes]", "description": "disconnect the linked workspace's GitHub repository"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud source commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-70s %s", c["command"], c["description"]))
	}
	return nil
}

func sourceConnect(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	repoKey := strings.TrimSpace(clicore.StringParam(params, "repo"))
	if clicore.StructuredOutput(params) && repoKey == "" {
		return clicore.NewError("cloud source connect requires --repo owner/name in structured output mode", clicore.ExitUsage)
	}
	openEnabled := clicore.BoolParam(params, true, "open")
	if clicore.StructuredOutput(params) && !openEnabled {
		return clicore.NewError("cloud source connect cannot use --no-open with structured output because the authorization URL cannot be shown mid-command; omit --no-open or run without structured output", clicore.ExitUsage)
	}
	runCtx, stop := sourceCommandContext(ioctx)
	defer stop()
	workspace, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	started, err := sourceCall(runCtx, workspace, workspace.WorkspaceURL(githubSourcePath+"/sessions"),
		func(callCtx context.Context, api *sourceapiclient.SourceClient) (*sourceapiclient.SourceGitHubOnboardingStart, error) {
			result, err := api.CreateV1WorkspacesSourceGithubSessions(callCtx, sourceapiclient.CreateV1WorkspacesSourceGithubSessionsInput{
				Path: sourceapiclient.CreateV1WorkspacesSourceGithubSessionsPath{Workspace: workspace.WorkspaceID},
			})
			if err != nil {
				return nil, err
			}
			return result.Body, nil
		})
	if err != nil {
		return err
	}
	sessionID := clicore.Deref(started.SessionId)
	authorizeURL := clicore.Deref(started.AuthorizeUrl)
	if sessionID == "" || authorizeURL == "" {
		return clicore.NewError("GitHub onboarding returned an incomplete session", clicore.ExitAPI)
	}
	if !clicore.StructuredOutput(params) {
		ioctx.Stdout("Authorize Putnami Cloud to discover your GitHub repositories:")
		ioctx.Stdout("  " + authorizeURL)
	}
	openedURL := ""
	if openEnabled {
		open := ioctx.OpenBrowser
		if open == nil {
			open = clicore.OpenBrowser
		}
		if err := open(authorizeURL); err != nil {
			if clicore.StructuredOutput(params) {
				_ = sourceCancelSession(workspace, sessionID)
				return clicore.NewError("could not open GitHub authorization automatically: "+err.Error()+"; rerun without structured output to see the authorization URL", clicore.ExitUsage)
			}
			ioctx.Stderr("warning: could not open the browser automatically: " + err.Error())
		} else {
			openedURL = authorizeURL
			if !clicore.StructuredOutput(params) {
				ioctx.Stdout("Browser opened automatically.")
			}
		}
	}

	timeout, err := sourcePollTimeout(params, *started, ioctx.Now())
	if err != nil {
		_ = sourceCancelSession(workspace, sessionID)
		return err
	}
	pollCtx, cancelPoll := context.WithTimeout(runCtx, timeout)
	defer cancelPoll()
	interval, err := sourcePollInterval(params, clicore.Deref(started.PollIntervalSeconds))
	if err != nil {
		_ = sourceCancelSession(workspace, sessionID)
		return err
	}
	if !clicore.StructuredOutput(params) {
		ioctx.Stdout("Waiting for GitHub authorization...")
	}
	session, err := waitForSourceCandidates(pollCtx, workspace, sessionID, interval, openEnabled, clicore.StructuredOutput(params), &openedURL, ioctx)
	if err != nil {
		_ = sourceCancelSession(workspace, sessionID)
		if runCtx.Err() != nil && pollCtx.Err() != context.DeadlineExceeded {
			return clicore.NewError("GitHub onboarding interrupted; the session was canceled", clicore.ExitSignal)
		}
		if pollCtx.Err() == context.DeadlineExceeded {
			return clicore.NewError("GitHub onboarding timed out; the session was canceled", clicore.ExitAPI)
		}
		return err
	}
	candidate, err := selectSourceCandidate(params, workspaceRoot, clicore.Deref(session.Candidates), ioctx)
	if err != nil {
		_ = sourceCancelSession(workspace, sessionID)
		return err
	}
	replace := clicore.BoolParam(params, false, "replace")
	installationID, repoID := clicore.Deref(candidate.InstallationId), clicore.Deref(candidate.RepoId)
	binding, err := sourceCall(pollCtx, workspace,
		workspace.WorkspaceURL(githubSourcePath+"/sessions/"+clicore.URLPathEscape(sessionID)+"/complete"),
		func(callCtx context.Context, api *sourceapiclient.SourceClient) (*sourceapiclient.Binding, error) {
			return api.CreateV1WorkspacesSourceGithubSessionsComplete(callCtx, sourceapiclient.CreateV1WorkspacesSourceGithubSessionsCompleteInput{
				Path: sourceapiclient.CreateV1WorkspacesSourceGithubSessionsCompletePath{Workspace: workspace.WorkspaceID, Session: sessionID},
				Body: sourceapiclient.CompleteSourceGitHubOnboardingRequest{
					InstallationId: &installationID, RepoId: &repoID, Replace: &replace,
				},
			})
		})
	if err != nil {
		_ = sourceCancelSession(workspace, sessionID)
		return sourceCompletionError(err)
	}
	clicore.WriteResult(sourceBindingFrom(*binding), params, ioctx, fmt.Sprintf("Connected GitHub repository %s/%s to workspace %s.", clicore.Deref(candidate.Owner), clicore.Deref(candidate.Repo), workspace.WorkspaceID))
	return nil
}

func sourceDisconnect(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if !clicore.BoolParam(params, false, "yes") {
		if clicore.StructuredOutput(params) {
			return clicore.NewError("cloud source disconnect requires --yes in structured output mode", clicore.ExitUsage)
		}
		if ioctx.Confirm == nil {
			return clicore.NewError("cloud source disconnect requires interactive confirmation or --yes", clicore.ExitUsage)
		}
		answer, ok := ioctx.Confirm("Disconnect the GitHub repository from this workspace? [y/N]: ")
		if !ok {
			return clicore.NewError("cloud source disconnect requires interactive confirmation or --yes", clicore.ExitUsage)
		}
		if !affirmative(answer, false) {
			ioctx.Stdout("Disconnect canceled.")
			return nil
		}
	}
	runCtx, stop := sourceCommandContext(ioctx)
	defer stop()
	workspace, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	if _, err := sourceCall(runCtx, workspace, workspace.WorkspaceURL(githubSourcePath),
		func(callCtx context.Context, api *sourceapiclient.SourceClient) (*struct{}, error) {
			_, err := api.DeleteV1WorkspacesSourceGithub(callCtx, sourceapiclient.DeleteV1WorkspacesSourceGithubInput{
				Path: sourceapiclient.DeleteV1WorkspacesSourceGithubPath{Workspace: workspace.WorkspaceID},
			})
			return nil, err
		}); err != nil {
		return err
	}
	clicore.WriteResult(map[string]any{"connected": false, "workspace_id": workspace.WorkspaceID}, params, ioctx, "GitHub repository disconnected.")
	return nil
}

func waitForSourceCandidates(ctx context.Context, workspace *clicore.WorkspaceContext, sessionID string, interval time.Duration, openEnabled, structured bool, openedURL *string, ioctx clicore.IO) (*sourceapiclient.SourceGitHubOnboardingSession, error) {
	url := workspace.WorkspaceURL(githubSourcePath + "/sessions/" + clicore.URLPathEscape(sessionID))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		session, err := sourceCall(ctx, workspace, url,
			func(callCtx context.Context, api *sourceapiclient.SourceClient) (*sourceapiclient.SourceGitHubOnboardingSession, error) {
				return api.GetV1WorkspacesSourceGithubSessions(callCtx, sourceapiclient.GetV1WorkspacesSourceGithubSessionsInput{
					Path: sourceapiclient.GetV1WorkspacesSourceGithubSessionsPath{Workspace: workspace.WorkspaceID, Session: sessionID},
				})
			})
		if err != nil {
			return nil, err
		}
		status := clicore.Deref(session.Status)
		switch status {
		case "candidates_ready":
			return session, nil
		case "pending_auth":
		case "awaiting_install":
			installURL := clicore.Deref(session.InstallUrl)
			if installURL != "" && installURL != *openedURL {
				*openedURL = installURL
				if !structured {
					ioctx.Stdout("Install or configure the Putnami GitHub App:")
					ioctx.Stdout("  " + installURL)
				}
				if openEnabled {
					open := ioctx.OpenBrowser
					if open == nil {
						open = clicore.OpenBrowser
					}
					if err := open(installURL); err != nil {
						if structured {
							return nil, clicore.NewError("could not open the GitHub App install page automatically: "+err.Error()+"; rerun without structured output to see the install URL", clicore.ExitUsage)
						}
						ioctx.Stderr("warning: could not open the GitHub App install page automatically: " + err.Error())
					}
				}
			}
		case "cancelled": //nolint:misspell // Canonical onboarding status returned by the API.
			return nil, clicore.NewError("GitHub onboarding was canceled", clicore.ExitAPI)
		case "expired":
			return nil, clicore.NewError("GitHub onboarding session expired", clicore.ExitAPI)
		case "failed":
			detail := clicore.Deref(session.FailureCode)
			if detail == "" {
				detail = "unknown failure"
			}
			return nil, clicore.NewError("GitHub onboarding failed: "+detail, clicore.ExitAPI)
		case "completed":
			return nil, clicore.NewError("GitHub onboarding session was already completed", clicore.ExitAPI)
		default:
			return nil, clicore.NewError("GitHub onboarding returned unknown status "+strconv.Quote(status), clicore.ExitAPI)
		}
		if err := waitSourcePoll(ctx, interval, ioctx.Sleep); err != nil {
			return nil, err
		}
	}
}

func waitSourcePoll(ctx context.Context, interval time.Duration, sleep func(context.Context, time.Duration) error) error {
	if interval <= 0 {
		return ctx.Err()
	}
	if sleep != nil {
		return sleep(ctx, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func selectSourceCandidate(params map[string]any, workspaceRoot string, candidates []sourceapiclient.SourceGitHubOnboardingCandidate, ioctx clicore.IO) (sourceapiclient.SourceGitHubOnboardingCandidate, error) {
	if len(candidates) == 0 {
		return sourceapiclient.SourceGitHubOnboardingCandidate{}, clicore.NewError("GitHub authorization returned no repository candidates; configure the App for a repository and retry", clicore.ExitUsage)
	}
	requested := strings.TrimSpace(clicore.StringParam(params, "repo"))
	if requested != "" {
		for _, candidate := range candidates {
			if strings.EqualFold(requested, candidateKey(candidate)) {
				return candidate, nil
			}
		}
		return sourceapiclient.SourceGitHubOnboardingCandidate{}, clicore.NewError(fmt.Sprintf("repository %q is not an authorized candidate; choose one of: %s", requested, candidateKeys(candidates)), clicore.ExitUsage)
	}
	if remote, err := sourceOriginRemote(workspaceRoot); err == nil {
		if provider, key, parseErr := clicore.RepositoryKeyFromRemote(remote); parseErr == nil && provider == "github" {
			for _, candidate := range candidates {
				if strings.EqualFold(key, candidateKey(candidate)) {
					if ioctx.Confirm != nil {
						answer, ok := ioctx.Confirm("Connect local origin " + candidateKey(candidate) + "? [Y/n]: ")
						if ok && affirmative(answer, true) {
							return candidate, nil
						}
					}
					break
				}
			}
		}
	}
	for i, candidate := range candidates {
		ioctx.Stdout(fmt.Sprintf("  %d. %s", i+1, candidateKey(candidate)))
	}
	if ioctx.Prompt == nil {
		return sourceapiclient.SourceGitHubOnboardingCandidate{}, clicore.NewError("choose a repository with --repo owner/name in non-interactive mode", clicore.ExitUsage)
	}
	answer, ok := ioctx.Prompt("Select a repository [1-" + strconv.Itoa(len(candidates)) + "]: ")
	if !ok {
		return sourceapiclient.SourceGitHubOnboardingCandidate{}, clicore.NewError("choose a repository with --repo owner/name in non-interactive mode", clicore.ExitUsage)
	}
	selected, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || selected < 1 || selected > len(candidates) {
		return sourceapiclient.SourceGitHubOnboardingCandidate{}, clicore.NewError("repository selection must be a number from 1 to "+strconv.Itoa(len(candidates)), clicore.ExitUsage)
	}
	return candidates[selected-1], nil
}

// candidateKey is a candidate's owner/name repository key.
func candidateKey(candidate sourceapiclient.SourceGitHubOnboardingCandidate) string {
	return clicore.Deref(candidate.Owner) + "/" + clicore.Deref(candidate.Repo)
}

func candidateKeys(candidates []sourceapiclient.SourceGitHubOnboardingCandidate) string {
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, candidateKey(candidate))
	}
	return strings.Join(keys, ", ")
}

// sourceCompletionError turns the two conflicts source-api declares on
// onboarding completion into guidance. The generated client selects their
// typed errors from the provider's stable codes.
func sourceCompletionError(err error) error {
	var refusal sourceRefusalError
	if !errors.As(err, &refusal) {
		return err
	}
	var workspaceBound *sourceapiclient.CreateV1WorkspacesSourceGithubSessionsCompleteSourceGithubWorkspaceAlreadyBoundError
	var boundElsewhere *sourceapiclient.CreateV1WorkspacesSourceGithubSessionsCompleteSourceGithubRepoBoundElsewhereError
	switch {
	case errors.As(refusal.cause, &workspaceBound):
		return clicore.NewError(workspaceBound.Remote.Code()+": this workspace already has a GitHub repository; rerun with --replace to replace it", clicore.ExitAPI)
	case errors.As(refusal.cause, &boundElsewhere):
		return clicore.NewError(boundElsewhere.Remote.Code()+": this GitHub repository is connected to another workspace; disconnect it there or choose another repository", clicore.ExitAPI)
	default:
		return err
	}
}

func sourcePollInterval(params map[string]any, serverSeconds int64) (time.Duration, error) {
	if override := clicore.NumberParam(params, "poll-interval-ms", "pollIntervalMs"); override != nil {
		if *override < 0 {
			return 0, clicore.NewError("--poll-interval-ms must be zero or greater", clicore.ExitUsage)
		}
		return time.Duration(*override * float64(time.Millisecond)), nil
	}
	if serverSeconds <= 0 {
		serverSeconds = 2
	}
	return time.Duration(serverSeconds) * time.Second, nil
}

func sourcePollTimeout(params map[string]any, started sourceapiclient.SourceGitHubOnboardingStart, now time.Time) (time.Duration, error) {
	if override := clicore.NumberParam(params, "poll-timeout-ms", "pollTimeoutMs"); override != nil {
		if *override < 0 {
			return 0, clicore.NewError("--poll-timeout-ms must be zero or greater", clicore.ExitUsage)
		}
		return time.Duration(*override * float64(time.Millisecond)), nil
	}
	timeout := clicore.Deref(started.ExpiresAt).Sub(now)
	if timeout <= 0 {
		return 0, nil
	}
	return timeout, nil
}

// sourceClient resolves source-api's generated Go client bound to the
// workspace's control plane, forwarding its bearer per call.
func sourceClient(workspace *clicore.WorkspaceContext) (*sourceapiclient.SourceClient, error) {
	return clicore.NewServiceClient[sourceapiclient.SourceClient](sourceapiclient.RegisterSourceClient, workspace.ServiceBinding())
}

// sourceCall issues one generated source-api call through
// clicore.CallWithSession (the single 401 re-mint, when the context carries
// one) and returns the generated response, or its zero value when the call
// answered no body. A refusal reads "request failed for <url>: <status line>"
// (ExitAuth on 401): the generated client withholds the provider's free-text
// message; its typed error rides a sourceRefusalError
// for sourceCompletionError.
func sourceCall[R any](ctx context.Context, workspace *clicore.WorkspaceContext, url string,
	call func(context.Context, *sourceapiclient.SourceClient) (*R, error),
) (*R, error) {
	api, err := sourceClient(workspace)
	if err != nil {
		return nil, err
	}
	resp, err := clicore.CallWithSession(ctx, workspace, func(callCtx context.Context) (*R, error) {
		return call(callCtx, api)
	})
	if err != nil {
		return nil, sourceCallError(url, err)
	}
	if resp == nil {
		resp = new(R)
	}
	return resp, nil
}

// sourceRefusalError is a provider refusal. cause keeps the generated
// client's error, typed by the provider-declared stable code; err is the CLI
// error it renders as.
type sourceRefusalError struct {
	err    error
	cause  error
	status int
}

func (e sourceRefusalError) Error() string { return e.err.Error() }
func (e sourceRefusalError) Unwrap() error { return e.err }

func sourceCallError(url string, err error) error {
	var remote *client.RemoteError
	if errors.As(err, &remote) {
		code := clicore.ExitAPI
		if remote.StatusCode == http.StatusUnauthorized {
			code = clicore.ExitAuth
		}
		return sourceRefusalError{
			err:    clicore.NewError("request failed for "+url+": "+clicore.StatusLine(remote.StatusCode), code),
			cause:  err,
			status: remote.StatusCode,
		}
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return clicore.NewError("invalid JSON response from "+url, clicore.ExitAPI)
	}
	return clicore.NewError(fmt.Sprintf("request failed for %s: %s", url, err.Error()), clicore.ExitAPI)
}

func sourceCancelSession(workspace *clicore.WorkspaceContext, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sourceCall(ctx, workspace, workspace.WorkspaceURL(githubSourcePath+"/sessions/"+clicore.URLPathEscape(sessionID)),
		func(callCtx context.Context, api *sourceapiclient.SourceClient) (*struct{}, error) {
			_, err := api.DeleteV1WorkspacesSourceGithubSessions(callCtx, sourceapiclient.DeleteV1WorkspacesSourceGithubSessionsInput{
				Path: sourceapiclient.DeleteV1WorkspacesSourceGithubSessionsPath{Workspace: workspace.WorkspaceID, Session: sessionID},
			})
			return nil, err
		})
	if sourceRefusalStatus(err) == http.StatusNotFound {
		return nil
	}
	return err
}

func sourceCommandContext(ioctx clicore.IO) (context.Context, context.CancelFunc) {
	base := ioctx.Context
	if base == nil {
		base = context.Background()
	}
	return signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
}

func affirmative(answer string, defaultValue bool) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	case "":
		return defaultValue
	default:
		return false
	}
}

// sourceRefusalStatus is the HTTP status of a provider refusal sourceCall
// rendered, or 0.
func sourceRefusalStatus(err error) int {
	var refusal sourceRefusalError
	if !errors.As(err, &refusal) {
		return 0
	}
	return refusal.status
}
