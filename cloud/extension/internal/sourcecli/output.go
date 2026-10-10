package sourcecli

import (
	"time"

	sourceapiclient "go.putnami.dev/cloud/clients/source-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The source verbs print their own result shapes. source-api's generated types
// order JSON keys alphabetically and omit every empty member, so writing them
// as a command result would change the bytes `--output json` has always
// printed. Each shape below keeps that output's key order and tags, and a
// converter fills it once from the generated response.

// sourceBinding is one GitHub App installation and repository bound to a
// workspace: the result of `source connect`, and the binding `source status`
// shows.
type sourceBinding struct {
	Provider       string    `json:"provider"`
	InstallationID int64     `json:"installation_id"`
	RepoID         int64     `json:"repo_id"`
	WorkspaceID    string    `json:"workspace_id"`
	Owner          string    `json:"owner,omitempty"`
	Repo           string    `json:"repo,omitempty"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	SuspendedAt    time.Time `json:"suspended_at,omitzero"`
	RemovedAt      time.Time `json:"removed_at,omitzero"`
	UninstalledAt  time.Time `json:"uninstalled_at,omitzero"`
}

// sourcePermissionGap is one permission the installation lacks.
type sourcePermissionGap struct {
	Permission string `json:"permission"`
	Required   string `json:"required,omitempty"`
	Granted    string `json:"granted,omitempty"`
	Kind       string `json:"kind"`
	Action     string `json:"action"`
}

// sourceHealth is the lifecycle and permission state of a binding.
type sourceHealth struct {
	Code           string                `json:"code"`
	Healthy        bool                  `json:"healthy"`
	PermissionGaps []sourcePermissionGap `json:"permission_gaps,omitempty"`
}

// sourceRepository is the `source status` result: the workspace's repository
// connection and its health.
type sourceRepository struct {
	Provider  string         `json:"provider"`
	Connected bool           `json:"connected"`
	Binding   *sourceBinding `json:"binding,omitempty"`
	Health    sourceHealth   `json:"health"`
}

func sourceBindingFrom(in sourceapiclient.Binding) sourceBinding {
	return sourceBinding{
		Provider:       clicore.Deref(in.Provider),
		InstallationID: clicore.Deref(in.InstallationId),
		RepoID:         clicore.Deref(in.RepoId),
		WorkspaceID:    clicore.Deref(in.WorkspaceId),
		Owner:          clicore.Deref(in.Owner),
		Repo:           clicore.Deref(in.Repo),
		Active:         clicore.Deref(in.Active),
		CreatedAt:      clicore.Deref(in.CreatedAt),
		UpdatedAt:      clicore.Deref(in.UpdatedAt),
		SuspendedAt:    clicore.Deref(in.SuspendedAt),
		RemovedAt:      clicore.Deref(in.RemovedAt),
		UninstalledAt:  clicore.Deref(in.UninstalledAt),
	}
}

func sourceRepositoryFrom(in sourceapiclient.SourceGitHubRepository) sourceRepository {
	out := sourceRepository{
		Provider:  clicore.Deref(in.Provider),
		Connected: clicore.Deref(in.Connected),
	}
	if binding, ok := in.Binding.Value(); ok {
		view := sourceBindingFrom(binding)
		out.Binding = &view
	}
	if in.Health != nil {
		out.Health = sourceHealth{Code: clicore.Deref(in.Health.Code), Healthy: clicore.Deref(in.Health.Healthy)}
		for _, gap := range clicore.Deref(in.Health.PermissionGaps) {
			out.Health.PermissionGaps = append(out.Health.PermissionGaps, sourcePermissionGap{
				Permission: clicore.Deref(gap.Permission),
				Required:   clicore.Deref(gap.Required),
				Granted:    clicore.Deref(gap.Granted),
				Kind:       clicore.Deref(gap.Kind),
				Action:     clicore.Deref(gap.Action),
			})
		}
	}
	return out
}
