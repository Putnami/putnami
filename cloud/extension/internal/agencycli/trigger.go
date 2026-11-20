package agencycli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Routine trigger bounds Config enforces (config/agent-trigger). The CLI
// checks them first, so nothing is sent for a trigger Config would refuse.
const (
	triggerFireHost      = "api.anthropic.com"
	triggerMinTokenBytes = 16
	triggerMaxTokenBytes = 1024
)

var (
	triggerRoutinePath = regexp.MustCompile(`^/v1/claude_code/routines/([A-Za-z0-9_-]{1,128})/fire$`)
	triggerSkills      = []string{"rebase", "build"}
)

// triggerPutnamiRoutine is the trigger without --skill: the person's Putnami
// routine, which Putnami starts for every routine task the person keeps no
// skill-specific trigger for.
const triggerPutnamiRoutine = "putnami"

// Trigger runs `putnami agency trigger register|show|remove`: the API trigger
// of the caller's own Putnami routine in the workspace, or with --skill of a
// routine for that skill alone. The token is read from input only and never
// printed.
func Trigger(params map[string]any, args []string, root string, env map[string]string, ioctx clicore.IO, input io.Reader) error {
	positionals := clicore.Positionals(args)
	if len(positionals) == 0 || positionals[0] == "help" {
		commands := []string{
			"agency trigger register --url <fire URL> --token-stdin",
			"agency trigger show",
			"agency trigger remove",
			"agency trigger register --skill rebase|build --url <fire URL> --token-stdin",
		}
		clicore.WriteResult(map[string]any{
			"commands": commands,
			"url":      "the Fire URL of your routine's API trigger, https://api.anthropic.com/v1/claude_code/routines/<routine>/fire",
			"token":    "register reads the routine's token from standard input only; no command prints it",
			"skill":    "without --skill, the command acts on your Putnami routine, which runs every routine task; --skill rebase or --skill build acts on a routine for that skill alone",
		}, params, ioctx, strings.Join(commands, "\n"))
		return nil
	}
	operation := positionals[0]
	if operation != "register" && operation != "show" && operation != "remove" || len(positionals) != 1 {
		return clicore.NewError("trigger takes register, show or remove", clicore.ExitUsage)
	}
	if err := refuseSecretFlags(params, []string{"token", "value", "key", "from-file"},
		"the routine's token is read from standard input only: pipe it to `putnami agency trigger register --token-stdin`"); err != nil {
		return err
	}
	skill := strings.TrimSpace(clicore.StringParam(params, "skill"))
	if skill == "" {
		skill = triggerPutnamiRoutine
	} else if !slices.Contains(triggerSkills, skill) {
		return clicore.NewError("--skill names the one task a routine runs: rebase or build. Leave it out for your Putnami routine, which runs every task", clicore.ExitUsage)
	}
	fireURL := clicore.StringParam(params, "url")
	var token []byte
	if operation == "register" {
		if _, ok := triggerRoutineOf(fireURL); !ok {
			return clicore.NewError("--url is the Fire URL of your routine's API trigger: https://api.anthropic.com/v1/claude_code/routines/<routine>/fire", clicore.ExitUsage)
		}
		if !clicore.BoolParam(params, false, "token-stdin") {
			return clicore.NewError("trigger register reads the routine's token from standard input: pipe it to `putnami agency trigger register --token-stdin`", clicore.ExitUsage)
		}
		read, ok := readSecret(input, triggerMinTokenBytes, triggerMaxTokenBytes)
		if !ok {
			return clicore.NewError("standard input is not one routine token: one line of 16 to 1024 printable characters with no space", clicore.ExitUsage)
		}
		token = read
		defer clear(token)
	}
	s, err := openSession(params, root, env, ioctx, "Config", triggerRefusal)
	if err != nil {
		return err
	}
	config, err := clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, s.workspace.ServiceBinding())
	if err != nil {
		return err
	}
	workspace := s.workspace.WorkspaceID
	switch operation {
	case "remove":
		_, _, err := call(s, func(ctx context.Context) (*configapiclient.DeleteV1WorkspacesConfigAgentTriggersResult, error) {
			return config.DeleteV1WorkspacesConfigAgentTriggers(ctx, configapiclient.DeleteV1WorkspacesConfigAgentTriggersInput{
				Path: configapiclient.DeleteV1WorkspacesConfigAgentTriggersPath{Workspace: workspace, Skill: skill},
			})
		})
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Your %s trigger is removed from this workspace: Putnami starts your Putnami routine for it, if you registered one.", skill)
		if skill == triggerPutnamiRoutine {
			text = "Your Putnami routine is removed from this workspace: Putnami starts it for no task."
		}
		clicore.WriteResult(map[string]any{"skill": skill, "state": "removed"}, params, ioctx, text)
		return nil
	case "show":
		trigger, answered, err := call(s, func(ctx context.Context) (*configapiclient.AgentTrigger, error) {
			return config.GetV1WorkspacesConfigAgentTriggers(ctx, configapiclient.GetV1WorkspacesConfigAgentTriggersInput{
				Path: configapiclient.GetV1WorkspacesConfigAgentTriggersPath{Workspace: workspace, Skill: skill},
			})
		}, http.StatusNotFound)
		if err != nil {
			return err
		}
		if answered == http.StatusNotFound || trigger == nil {
			text := fmt.Sprintf("You have no %s trigger in this workspace. Register one with `putnami agency trigger register --skill %s --url <fire URL> --token-stdin`.", skill, skill)
			if skill == triggerPutnamiRoutine {
				text = "You have no Putnami routine in this workspace. Connect it in putnami.app: click Start on an issue of the backlog, and follow its two steps. From a terminal instead, create it with the putnami-routine skill, then register its API trigger with `putnami agency trigger register --url <fire URL> --token-stdin`."
			}
			clicore.WriteResult(map[string]any{"skill": skill, "state": "not_registered"}, params, ioctx, text)
			return nil
		}
		result := triggerResult(trigger)
		clicore.WriteResult(result, params, ioctx, triggerText(result))
		return nil
	}
	binding, once := s.onceBinding()
	onceConfig, err := clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, binding)
	if err != nil {
		return err
	}
	value := string(token)
	trigger, err := callOnce(s, once, "token", func(ctx context.Context) (*configapiclient.AgentTrigger, error) {
		return onceConfig.UpdateV1WorkspacesConfigAgentTriggers(ctx, configapiclient.UpdateV1WorkspacesConfigAgentTriggersInput{
			Path: configapiclient.UpdateV1WorkspacesConfigAgentTriggersPath{Workspace: workspace, Skill: skill},
			Body: configapiclient.AgentTriggerRegisterRequest{Url: &fireURL, Token: &value},
		})
	})
	value = ""
	if err != nil {
		return err
	}
	if trigger == nil {
		return clicore.NewError("Config answered no trigger", clicore.ExitAPI)
	}
	result := triggerResult(trigger)
	clicore.WriteResult(result, params, ioctx, triggerText(result))
	return nil
}

// triggerRoutineOf answers the routine id of a routine fire URL.
func triggerRoutineOf(raw string) (string, bool) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > 512 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != triggerFireHost || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", false
	}
	m := triggerRoutinePath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func triggerRefusal(status int) error {
	switch status {
	case http.StatusForbidden:
		return clicore.NewError("only a member of the workspace keeps a trigger there, with a person's own session: a workspace or machine token cannot", clicore.ExitAuth)
	case http.StatusBadRequest:
		return clicore.NewError("Config refused the trigger: the URL must be your routine's Fire URL on api.anthropic.com", clicore.ExitUsage)
	case http.StatusServiceUnavailable:
		return clicore.NewError("routine triggers are not enabled on this Putnami Cloud yet", clicore.ExitAPI)
	}
	return nil
}

func triggerResult(trigger *configapiclient.AgentTrigger) map[string]any {
	result := map[string]any{
		"skill":      deref(trigger.Skill),
		"routineId":  deref(trigger.RoutineId),
		"generation": derefInt(trigger.Generation),
		"state":      "registered",
	}
	if trigger.UpdatedAt != nil {
		result["updatedAt"] = trigger.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return result
}

func triggerText(result map[string]any) string {
	if result["skill"] == triggerPutnamiRoutine {
		return fmt.Sprintf("Your Putnami routine is routine %v (generation %v). Putnami starts it for every routine task of your own pull requests that you keep no skill-specific trigger for.",
			result["routineId"], result["generation"])
	}
	return fmt.Sprintf("Your %v trigger starts routine %v (generation %v). Putnami starts it for your own pull requests only.",
		result["skill"], result["routineId"], result["generation"])
}
