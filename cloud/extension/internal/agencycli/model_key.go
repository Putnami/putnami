package agencycli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"go.putnami.dev/client"
	configapiclient "go.putnami.dev/cloud/clients/config-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The per-run budget bounds Config enforces. The CLI checks them first, so
// nothing is sent for a budget Config would refuse.
const (
	modelKeyMinTokens       = 1_000
	modelKeyMaxTokens       = 50_000_000
	modelKeyMinModelMinutes = 1
	modelKeyMaxModelMinutes = 120
	modelKeyMinBytes        = 20
	modelKeyMaxBytes        = 512
	modelKeyProvider        = "anthropic"
)

// ModelKey runs `putnami agency model-key set|status|revoke`: the
// organization's Anthropic API key that Putnami jobs use, within a budget per
// run. The key is read from input only and never printed.
func ModelKey(params map[string]any, args []string, root string, env map[string]string, ioctx clicore.IO, input io.Reader) error {
	positionals := clicore.Positionals(args)
	if len(positionals) == 0 || positionals[0] == "help" {
		commands := []string{
			"agency model-key set --stdin [--max-tokens <n>] [--max-model-minutes <n>] [--expected-generation <n>]",
			"agency model-key status",
			"agency model-key revoke --expected-generation <n> --confirm",
		}
		clicore.WriteResult(map[string]any{
			"commands":    commands,
			"permissions": "every command needs platform.organization.manage at the organization of the workspace",
			"key":         "set reads the Anthropic API key from standard input only; no command prints it",
		}, params, ioctx, strings.Join(commands, "\n"))
		return nil
	}
	operation := positionals[0]
	if operation != "set" && operation != "status" && operation != "revoke" || len(positionals) != 1 {
		return clicore.NewError("model-key takes set, status or revoke", clicore.ExitUsage)
	}
	if err := refuseSecretFlags(params, []string{"key", "api-key", "value", "token", "from-file"},
		"the model key is read from standard input only: pipe it to `putnami agency model-key set --stdin`"); err != nil {
		return err
	}
	request := configapiclient.ModelCredentialSetRequest{}
	var revokeGeneration int64
	switch operation {
	case "set":
		budget, err := modelKeyBudget(params)
		if err != nil {
			return err
		}
		if budget != nil {
			request.Budget = client.Some(*budget)
		}
		if raw := clicore.StringParam(params, "expected-generation"); raw != "" {
			generation, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || generation < 0 {
				return clicore.NewError("--expected-generation is the generation `model-key status` printed, or 0 for a first set", clicore.ExitUsage)
			}
			request.ExpectedGeneration = client.Some(generation)
		}
		if !clicore.BoolParam(params, false, "stdin") {
			return clicore.NewError("model-key set reads the key from standard input: pipe it to `putnami agency model-key set --stdin`", clicore.ExitUsage)
		}
		key, ok := readSecret(input, modelKeyMinBytes, modelKeyMaxBytes)
		if !ok {
			return clicore.NewError("standard input is not one Anthropic API key: one line of 20 to 512 printable characters with no space", clicore.ExitUsage)
		}
		defer clear(key)
		value, provider := string(key), modelKeyProvider
		request.Key, request.Provider = &value, &provider
		defer func() { *request.Key = "" }()
	case "revoke":
		generation, err := strconv.ParseInt(clicore.StringParam(params, "expected-generation"), 10, 64)
		if err != nil || generation < 1 {
			return clicore.NewError("model-key revoke needs --expected-generation from `model-key status`", clicore.ExitUsage)
		}
		if !clicore.BoolParam(params, false, "confirm") {
			return clicore.NewError("model-key revoke needs --confirm: every running job that uses the key stops", clicore.ExitUsage)
		}
		revokeGeneration = generation
	}
	s, err := openSession(params, root, env, ioctx, "Config", modelKeyRefusal)
	if err != nil {
		return err
	}
	config, err := clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, s.workspace.ServiceBinding())
	if err != nil {
		return err
	}
	workspace := s.workspace.WorkspaceID
	var accepted []int
	if operation == "status" {
		accepted = []int{http.StatusNotFound}
	}
	if operation == "set" {
		binding, once := s.onceBinding()
		onceConfig, err := clicore.NewServiceClient[configapiclient.ConfigClient](configapiclient.RegisterConfigClient, binding)
		if err != nil {
			return err
		}
		status, err := callOnce(s, once, "key", func(ctx context.Context) (*configapiclient.ModelCredentialStatus, error) {
			return onceConfig.UpdateV1WorkspacesConfigOrganizationModelCredential(ctx, configapiclient.UpdateV1WorkspacesConfigOrganizationModelCredentialInput{
				Path: configapiclient.UpdateV1WorkspacesConfigOrganizationModelCredentialPath{Workspace: workspace},
				Body: request,
			})
		})
		if err != nil {
			return err
		}
		if status == nil {
			return clicore.NewError("Config answered no status", clicore.ExitAPI)
		}
		result := modelKeyResult(status)
		clicore.WriteResult(result, params, ioctx, modelKeyText(result))
		return nil
	}
	status, answered, err := call(s, func(ctx context.Context) (*configapiclient.ModelCredentialStatus, error) {
		switch operation {
		case "revoke":
			provider := modelKeyProvider
			return config.CreateV1WorkspacesConfigOrganizationModelCredentialRevoke(ctx, configapiclient.CreateV1WorkspacesConfigOrganizationModelCredentialRevokeInput{
				Path: configapiclient.CreateV1WorkspacesConfigOrganizationModelCredentialRevokePath{Workspace: workspace},
				Body: configapiclient.ModelCredentialRevokeRequest{Provider: &provider, ExpectedGeneration: &revokeGeneration},
			})
		default:
			return config.GetV1WorkspacesConfigOrganizationModelCredential(ctx, configapiclient.GetV1WorkspacesConfigOrganizationModelCredentialInput{
				Path: configapiclient.GetV1WorkspacesConfigOrganizationModelCredentialPath{Workspace: workspace},
			})
		}
	}, accepted...)
	if err != nil {
		return err
	}
	if answered == http.StatusNotFound || status == nil {
		clicore.WriteResult(map[string]any{"provider": modelKeyProvider, "state": "not_set"}, params, ioctx,
			"The organization has no Anthropic API key for Putnami jobs. Set one with `putnami agency model-key set --stdin`.")
		return nil
	}
	result := modelKeyResult(status)
	clicore.WriteResult(result, params, ioctx, modelKeyText(result))
	return nil
}

// modelKeyBudget reads --max-tokens and --max-model-minutes. Neither set
// answers nil: Config keeps the stored budget, or takes its default.
func modelKeyBudget(params map[string]any) (*configapiclient.RunBudget, error) {
	tokensRaw, minutesRaw := clicore.StringParam(params, "max-tokens"), clicore.StringParam(params, "max-model-minutes")
	if tokensRaw == "" && minutesRaw == "" {
		return nil, nil
	}
	if tokensRaw == "" || minutesRaw == "" {
		return nil, clicore.NewError("set --max-tokens and --max-model-minutes together", clicore.ExitUsage)
	}
	tokens, err := strconv.ParseInt(tokensRaw, 10, 64)
	if err != nil || tokens < modelKeyMinTokens || tokens > modelKeyMaxTokens {
		return nil, clicore.NewError(fmt.Sprintf("--max-tokens is a whole number from %d to %d", modelKeyMinTokens, modelKeyMaxTokens), clicore.ExitUsage)
	}
	minutes, err := strconv.ParseInt(minutesRaw, 10, 64)
	if err != nil || minutes < modelKeyMinModelMinutes || minutes > modelKeyMaxModelMinutes {
		return nil, clicore.NewError(fmt.Sprintf("--max-model-minutes is a whole number from %d to %d", modelKeyMinModelMinutes, modelKeyMaxModelMinutes), clicore.ExitUsage)
	}
	seconds := minutes * 60
	return &configapiclient.RunBudget{Tokens: &tokens, ModelSeconds: &seconds}, nil
}

func modelKeyRefusal(status int) error {
	switch status {
	case http.StatusForbidden:
		return clicore.NewError("only an organization admin manages the organization's model key: this needs platform.organization.manage at the workspace's organization", clicore.ExitAuth)
	case http.StatusConflict:
		return clicore.NewError("the organization's model key changed since you read it, or is already revoked: run `putnami agency model-key status` and use the generation it prints", clicore.ExitAPI)
	case http.StatusBadRequest:
		return clicore.NewError("Config refused the key or the budget", clicore.ExitUsage)
	case http.StatusServiceUnavailable:
		return clicore.NewError("organization model keys are not enabled on this Putnami Cloud yet", clicore.ExitAPI)
	}
	return nil
}

func modelKeyResult(status *configapiclient.ModelCredentialStatus) map[string]any {
	result := map[string]any{
		"organizationId": deref(status.OrganizationId),
		"provider":       deref(status.Provider),
		"state":          deref(status.State),
		"generation":     derefInt(status.Generation),
		"updatedBy":      deref(status.UpdatedBy),
	}
	if status.UpdatedAt != nil {
		result["updatedAt"] = status.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if budget := status.Budget; budget != nil {
		result["budget"] = map[string]any{"tokens": derefInt(budget.Tokens), "modelMinutes": derefInt(budget.ModelSeconds) / 60}
	}
	return result
}

func modelKeyText(result map[string]any) string {
	text := fmt.Sprintf("Organization %v: Anthropic API key %v, generation %v.", result["organizationId"], result["state"], result["generation"])
	if budget, ok := result["budget"].(map[string]any); ok {
		text += fmt.Sprintf(" Budget per run: %v tokens and %v minutes of model time.", budget["tokens"], budget["modelMinutes"])
	}
	if by, ok := result["updatedBy"].(string); ok && by != "" {
		text += fmt.Sprintf(" Last changed by %s at %v.", by, result["updatedAt"])
	}
	return text
}
