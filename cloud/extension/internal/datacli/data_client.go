package datacli

import (
	"net/http"
	"time"

	"go.putnami.dev/client"
	dataapiclient "go.putnami.dev/cloud/clients/data-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

// dataClient resolves data-api's generated Go client bound to ctx's control
// plane, forwarding ctx's workspace bearer per call. Every `cloud db` read and
// grant verb reaches the workspace database surface through it, each call
// wrapped in clicore.CallWithSession for the single 401 re-mint.
func dataClient(ctx *clicore.WorkspaceContext) (*dataapiclient.DataClient, error) {
	return clicore.NewServiceClient[dataapiclient.DataClient](dataapiclient.RegisterDataClient, ctx.ServiceBinding())
}

// dataCallError renders a failed generated data-api call as the CLI error the
// db verbs print. A provider refusal reads "request failed for <target>:
// <status line>" and exits ExitAuth on 401, ExitAPI otherwise: the generated
// client withholds the provider's free-text message, so
// the status line is what the CLI can name. An answer outside the provider
// contract reads invalidMessage; anything else is a transport failure.
func dataCallError(target, invalidMessage string, err error) error {
	if status := clicore.ServiceStatus(err); status != 0 {
		code := clicore.ExitAPI
		if status == http.StatusUnauthorized {
			code = clicore.ExitAuth
		}
		return clicore.NewError("request failed for "+target+": "+clicore.StatusLine(status), code)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return clicore.NewError(invalidMessage, clicore.ExitAPI)
	}
	return clicore.NewError("request failed for "+target+": "+err.Error(), clicore.ExitAPI)
}

// The db verbs print their own result shapes. data-api's generated types order
// JSON keys alphabetically and omit every empty member, so writing them as a
// command result would change the bytes `--output json` has always printed.
// Each shape below keeps that output's key order and tags, and a converter
// fills it once from the generated response.

// grantCredentials is the open-grant result: the connection payload plus the
// one-time login password. It is the only shape that carries the password,
// and the CLI prints it once and never stores it.
type grantCredentials struct {
	GrantID     string `json:"grant_id"`
	ScopeKind   string `json:"scope_kind"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Database    string `json:"database"`
	Level       string `json:"level"`
	Username    string `json:"username"`
	// Password is the one-time login password the open-grant POST returns.
	// This result is its single intended carrier, printed once to stdout.
	Password               string     `json:"password"` //nolint:gosec // G117: intended one-time credential, printed once and never stored
	InstanceConnectionName string     `json:"instance_connection_name,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

// grantView is the secret-free projection of one access grant. It has no
// password field by construction.
type grantView struct {
	ID          string     `json:"id"`
	ScopeKind   string     `json:"scope_kind"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
	Database    string     `json:"database"`
	Level       string     `json:"level"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	RequestorID string     `json:"requestor_id"`
	RequestedAt time.Time  `json:"requested_at"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
}

// grantList is the `db grant list` result.
type grantList struct {
	Grants []grantView `json:"grants"`
}

// databaseInventoryItem is one database of the inventory, with the caller's
// active grant when one exists.
type databaseInventoryItem struct {
	Database               string     `json:"database"`
	Engine                 string     `json:"engine,omitempty"`
	InstanceConnectionName string     `json:"instance_connection_name,omitempty"`
	Schemas                []string   `json:"schemas,omitempty"`
	ActiveGrant            *grantView `json:"active_grant,omitempty"`
}

// databaseInventory is the `db list` result.
type databaseInventory struct {
	Databases []databaseInventoryItem `json:"databases"`
}

func grantCredentialsFrom(in dataapiclient.GrantCredentials) grantCredentials {
	return grantCredentials{
		GrantID:                clicore.Deref(in.GrantId),
		ScopeKind:              clicore.Deref(in.ScopeKind),
		WorkspaceID:            clicore.Deref(in.WorkspaceId),
		Database:               clicore.Deref(in.Database),
		Level:                  clicore.Deref(in.Level),
		Username:               clicore.Deref(in.Username),
		Password:               clicore.Deref(in.Password),
		InstanceConnectionName: clicore.Deref(in.InstanceConnectionName),
		ExpiresAt:              optionalValue(in.ExpiresAt),
	}
}

func grantViewFrom(in dataapiclient.GrantView) grantView {
	return grantView{
		ID:          clicore.Deref(in.Id),
		ScopeKind:   clicore.Deref(in.ScopeKind),
		WorkspaceID: clicore.Deref(in.WorkspaceId),
		Database:    clicore.Deref(in.Database),
		Level:       clicore.Deref(in.Level),
		Status:      clicore.Deref(in.Status),
		Reason:      clicore.Deref(in.Reason),
		RequestorID: clicore.Deref(in.RequestorId),
		RequestedAt: clicore.Deref(in.RequestedAt),
		ActivatedAt: optionalValue(in.ActivatedAt),
		ExpiresAt:   optionalValue(in.ExpiresAt),
		ClosedAt:    optionalValue(in.ClosedAt),
	}
}

// grantListFrom keeps an absent list null and a present empty list [].
func grantListFrom(in *dataapiclient.GrantList) grantList {
	if in == nil || in.Grants == nil {
		return grantList{}
	}
	grants := make([]grantView, 0, len(*in.Grants))
	for _, grant := range *in.Grants {
		grants = append(grants, grantViewFrom(grant))
	}
	return grantList{Grants: grants}
}

// databaseInventoryFrom keeps an absent list null and a present empty list [].
func databaseInventoryFrom(in *dataapiclient.DatabaseInventory) databaseInventory {
	if in == nil || in.Databases == nil {
		return databaseInventory{}
	}
	databases := make([]databaseInventoryItem, 0, len(*in.Databases))
	for _, item := range *in.Databases {
		var activeGrant *grantView
		if grant, ok := item.ActiveGrant.Value(); ok {
			view := grantViewFrom(grant)
			activeGrant = &view
		}
		databases = append(databases, databaseInventoryItem{
			Database:               clicore.Deref(item.Database),
			Engine:                 clicore.Deref(item.Engine),
			InstanceConnectionName: clicore.Deref(item.InstanceConnectionName),
			Schemas:                clicore.Deref(item.Schemas),
			ActiveGrant:            activeGrant,
		})
	}
	return databaseInventory{Databases: databases}
}

// optionalValue reads a generated optional member: nil when the provider
// omitted it or sent null.
func optionalValue[T any](in client.Optional[T]) *T {
	value, ok := in.Value()
	if !ok {
		return nil
	}
	return &value
}
