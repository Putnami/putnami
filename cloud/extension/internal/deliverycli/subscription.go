package deliverycli

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// ciReencode projects one wire shape onto another through JSON: the CLI's
// local twins and delivery-api's generated types share the provider's wire
// contract, so the round trip is lossless.
func ciReencode(from, to any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, to)
}

// Local wire twins of the delivery API's workspace CI subscription contract. The
// CLI deliberately does not import server persistence or authority packages.
type ciTrack struct {
	ProducerNamespace string `json:"producer_namespace"`
	Channel           string `json:"channel,omitempty"`
	ReleaseSetID      string `json:"release_set_id,omitempty"`
}

type ciSubscription struct {
	WorkspaceID    string          `json:"workspace_id"`
	Revision       int64           `json:"revision"`
	Enabled        bool            `json:"enabled"`
	RunnerChannel  string          `json:"runner_channel"`
	Tracks         []ciTrack       `json:"tracks"`
	RequiredChecks json.RawMessage `json:"required_checks"`
	Drift          json.RawMessage `json:"drift"`
}

type ciSubscriptionPut struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Enabled          bool            `json:"enabled"`
	RunnerChannel    string          `json:"runner_channel"`
	Tracks           []ciTrack       `json:"tracks"`
	RequiredChecks   json.RawMessage `json:"required_checks"`
	Drift            json.RawMessage `json:"drift"`
}

func readCISubscription(reqCtx context.Context, ctx *clicore.WorkspaceContext, create bool) (*ciSubscription, error) {
	row, err := ciCall[ciSubscription](reqCtx, ctx, "subscription", ctx.WorkspaceURL("/ci/subscription"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.Subscription, error) {
			return api.GetV1WorkspacesCiSubscription(callCtx, deliveryapiclient.GetV1WorkspacesCiSubscriptionInput{
				Path: deliveryapiclient.GetV1WorkspacesCiSubscriptionPath{Workspace: ctx.WorkspaceID},
			})
		})
	if create && ciIsNotFound(err) {
		return &ciSubscription{RunnerChannel: "canary", Tracks: []ciTrack{}, RequiredChecks: json.RawMessage(`{"rules":[]}`), Drift: json.RawMessage(`{"apps":[]}`)}, nil
	}
	var invalid ciInvalidResponseError
	if stderrors.As(err, &invalid) {
		return nil, clicore.NewError("cloud ci subscription: invalid owner response", clicore.ExitAPI)
	}
	if err != nil {
		return nil, err
	}
	if row.Revision < 1 || row.WorkspaceID != ctx.WorkspaceID {
		return nil, clicore.NewError("cloud ci subscription: invalid owner response", clicore.ExitAPI)
	}
	return row, nil
}

func writeCISubscription(reqCtx context.Context, ctx *clicore.WorkspaceContext, row *ciSubscription) (*ciSubscription, error) {
	var body deliveryapiclient.PutCISubscriptionRequest
	if err := ciReencode(ciSubscriptionPut{
		ExpectedRevision: row.Revision, Enabled: row.Enabled, RunnerChannel: row.RunnerChannel,
		Tracks: row.Tracks, RequiredChecks: row.RequiredChecks, Drift: row.Drift,
	}, &body); err != nil {
		return nil, clicore.NewError("cloud ci subscription: encode request: "+err.Error(), clicore.ExitAPI)
	}
	return ciCall[ciSubscription](reqCtx, ctx, "subscription", ctx.WorkspaceURL("/ci/subscription"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.Subscription, error) {
			return api.UpdateV1WorkspacesCiSubscription(callCtx, deliveryapiclient.UpdateV1WorkspacesCiSubscriptionInput{
				Path: deliveryapiclient.UpdateV1WorkspacesCiSubscriptionPath{Workspace: ctx.WorkspaceID},
				Body: body,
			})
		})
}

// Track updates a producer selector and optional runner hold in one revision
// CAS. Namespace ownership and read authority are resolved by Delivery.
func Track(params map[string]any, _ []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	namespace := strings.TrimSpace(clicore.StringParam(params, "namespace"))
	channel := strings.TrimSpace(clicore.StringParam(params, "channel"))
	pin := strings.TrimSpace(clicore.StringParam(params, "release-set"))
	runner := strings.TrimSpace(clicore.StringParam(params, "runner-channel"))
	if namespace == "" || (channel == "") == (pin == "") || (channel != "" && channel != "stable" && channel != "canary") || (runner != "" && runner != "stable" && runner != "canary") {
		return clicore.NewError("cloud channels follow requires a namespace and exactly one of stable, canary or an rs_ release-set id (flags: --namespace, --channel, --release-set); --runner-channel accepts stable|canary", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	row, err := readCISubscription(reqCtx, ctx, false)
	if err != nil {
		return err
	}
	track := ciTrack{ProducerNamespace: namespace, Channel: channel, ReleaseSetID: pin}
	replaced := false
	for i := range row.Tracks {
		if row.Tracks[i].ProducerNamespace == namespace {
			row.Tracks[i] = track
			replaced = true
			break
		}
	}
	if !replaced {
		row.Tracks = append(row.Tracks, track)
	}
	if runner != "" {
		row.RunnerChannel = runner
	}
	updated, err := writeCISubscription(reqCtx, ctx, row)
	if err != nil {
		return err
	}
	clicore.WriteResult(updated, params, ioctx, fmt.Sprintf("Tracking %s at %s (revision %d).", namespace, channel+pin, updated.Revision))
	return nil
}

func ciSubscriptionCommand(params map[string]any, verb, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	runner := strings.TrimSpace(clicore.StringParam(params, "runner-channel"))
	if runner != "" && (verb != "enable" || (runner != "stable" && runner != "canary")) {
		return clicore.NewError("--runner-channel stable|canary is supported by cloud ci enable", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	row, err := readCISubscription(reqCtx, ctx, verb == "enable")
	if err != nil {
		return err
	}
	switch verb {
	case "enable":
		row.Enabled = true
		if runner != "" {
			row.RunnerChannel = runner
		}
		row, err = writeCISubscription(reqCtx, ctx, row)
	case "disable":
		expected := row.Revision
		row, err = ciCall[ciSubscription](reqCtx, ctx, "disable", ctx.WorkspaceURL("/ci/subscription/disable"),
			func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.Subscription, error) {
				return api.CreateV1WorkspacesCiSubscriptionDisable(callCtx, deliveryapiclient.CreateV1WorkspacesCiSubscriptionDisableInput{
					Path: deliveryapiclient.CreateV1WorkspacesCiSubscriptionDisablePath{Workspace: ctx.WorkspaceID},
					Body: deliveryapiclient.DisableCISubscriptionRequest{ExpectedRevision: &expected},
				})
			})
	}
	if err != nil {
		return err
	}
	clicore.WriteResult(row, params, ioctx, fmt.Sprintf("CI enabled=%t, runner=%s (revision %d).", row.Enabled, row.RunnerChannel, row.Revision))
	return nil
}
