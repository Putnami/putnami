package agencycli

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	agencyapiclient "go.putnami.dev/cloud/clients/agency-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
)

// buildRepositoryPattern is an owner/name GitHub repository, the shape
// the agency gateway accepts for a build start.
var buildRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// The reasons the gateway gives when it starts nothing.
const (
	buildReasonNoTrigger   = "no_trigger"
	buildReasonDisabled    = "disabled"
	buildReasonUnavailable = "unavailable"
)

// originRemote is the URL of the origin remote of the checkout at dir. Tests
// replace it.
var originRemote = func(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Build runs `putnami agency build --issue <n> [--repository owner/name]`: it
// asks the agency gateway to start the caller's own build routine on one
// issue, and prints the routine's session link. When the gateway starts
// nothing, it prints the reason and its fix, and exits 1.
func Build(params map[string]any, root string, env map[string]string, ioctx clicore.IO) error {
	issue, err := strconv.ParseInt(strings.TrimSpace(clicore.StringParam(params, "issue")), 10, 64)
	if err != nil || issue <= 0 {
		return clicore.NewError("--issue must be the issue number, a positive integer", clicore.ExitFailure)
	}
	repository := strings.TrimSpace(clicore.StringParam(params, "repository"))
	if repository == "" {
		remote, err := originRemote(root)
		if err == nil && !strings.Contains(strings.ToLower(remote), "github.com") {
			err = fmt.Errorf("the origin remote %q is not on GitHub", remote)
		}
		if err == nil {
			_, repository, err = clicore.RepositoryKeyFromRemote(remote)
		}
		if err != nil {
			return clicore.NewError("the origin remote of this checkout names no owner/name repository: pass --repository owner/name", clicore.ExitFailure)
		}
	}
	if !buildRepositoryPattern.MatchString(repository) {
		return clicore.NewError("--repository must be owner/name", clicore.ExitFailure)
	}
	s, err := openSession(params, root, env, ioctx, "the agency gateway", buildStartRefusal)
	if err != nil {
		return err
	}
	api, err := clicore.NewServiceClient[agencyapiclient.AgencyClient](agencyapiclient.RegisterAgencyClient, s.workspace.ServiceBinding())
	if err != nil {
		return err
	}
	workspace := s.workspace.WorkspaceID
	answer, _, err := call(s, func(ctx context.Context) (*agencyapiclient.BuildStartResponse, error) {
		return api.CreateV1WorkspacesAgencyBuilds(ctx, agencyapiclient.CreateV1WorkspacesAgencyBuildsInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyBuildsPath{Workspace: workspace},
			Body: agencyapiclient.BuildStartRequest{Repository: &repository, Issue: &issue},
		})
	})
	if err != nil {
		return err
	}
	if answer == nil {
		return clicore.NewError("the agency gateway answered no build start", clicore.ExitFailure)
	}
	result := map[string]any{"repository": repository, "issue": issue, "started": answer.Started != nil && *answer.Started}
	if result["started"] == true {
		text := fmt.Sprintf("Your build routine started on issue #%d of %s.", issue, repository)
		if run := deref(answer.Run); run != "" {
			result["run"] = run
		}
		if link := deref(answer.SessionUrl); link != "" {
			result["sessionUrl"] = link
			text += " Follow it at " + link
		}
		clicore.WriteResult(result, params, ioctx, text)
		return nil
	}
	reason := deref(answer.Reason)
	result["reason"] = reason
	return protocolcli.WithResultData(clicore.NewError(buildReasonText(reason), clicore.ExitFailure), result)
}

// buildReasonText is the reason the gateway started nothing, and its fix.
func buildReasonText(reason string) string {
	switch reason {
	case buildReasonNoTrigger:
		return "Nothing started: you have no Putnami routine in this workspace. Connect it in putnami.app: click Start on the issue in the backlog, and follow its two steps. From a terminal instead, create it with the putnami-routine skill, then register its API trigger with `putnami agency trigger register --url <fire URL> --token-stdin`."
	case buildReasonDisabled:
		return "Nothing started: routine triggers are not enabled on this Putnami Cloud yet."
	case buildReasonUnavailable:
		return "Nothing started: your routine did not accept the request. Check its API trigger, then register it again if its token changed."
	}
	return "Nothing started: the agency gateway gave no reason."
}

// buildStartRefusal maps the gateway's refusal to its sentence. Every refusal
// exits 1: nothing started.
func buildStartRefusal(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return clicore.NewError("the agency gateway rejected the credential; run `putnami cloud login`", clicore.ExitFailure)
	case http.StatusForbidden:
		return clicore.NewError("you cannot start builds in this workspace: a member starts their own build, with a person's own session", clicore.ExitFailure)
	case http.StatusBadRequest:
		return clicore.NewError("the agency gateway refused the request: the repository must be owner/name and the issue a positive number", clicore.ExitFailure)
	}
	if status >= 500 {
		return clicore.NewError(fmt.Sprintf("the agency gateway answered the build start with HTTP %d; nothing is known to have started: check your routine before you run it again", status), clicore.ExitFailure)
	}
	return clicore.NewError(fmt.Sprintf("the agency gateway refused the build start (HTTP %d); nothing started", status), clicore.ExitFailure)
}
