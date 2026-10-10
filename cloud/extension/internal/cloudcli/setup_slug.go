package cloudcli

import (
	"fmt"
	"regexp"
	"strings"

	identityapi "go.putnami.dev/cloud/clients/identity-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The slug of a new workspace. `putnami cloud setup --slug <slug>`
// sends it when setup creates the workspace. A slug is the workspace's short
// name, such as "acme". It starts the names of the workspace's services, and
// the workspace holds the event topic names that start with it and a dot:
// "acme.orders.created".
//
// identity-api checks the slug's shape when it creates the workspace
// (its ValidateSlug function). Setup checks the
// same shape first, so a wrong slug fails before any request. This module does
// not import that package, so the rule is written again here: a change there
// is repeated here.

// maxWorkspaceSlugLen is the longest slug identity-api accepts.
const maxWorkspaceSlugLen = 12

// workspaceSlugPattern is the shape identity-api accepts: lowercase letters
// and digits, in parts joined by single dashes, starting with a letter.
var workspaceSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// reservedWorkspaceSlugs are the first parts of the platform's own event topic
// names, such as "identity" in "identity.user.merged". Runtime gives no topic
// name to a workspace through one of these slugs (reservedEventTopicSlugs in
// the runtime provisioner), and a workspace keeps its slug.
// So setup refuses them for a new workspace. identity-api does not refuse
// them. The two modules share no package, so a name added there is added
// here.
var reservedWorkspaceSlugs = map[string]bool{
	"business":      true,
	"catalog":       true,
	"cloud":         true,
	"config":        true,
	"control":       true,
	"data":          true,
	"delivery":      true,
	"distribution":  true,
	"events":        true,
	"identity":      true,
	"infra":         true,
	"intelligence":  true,
	"observability": true,
	"platform":      true,
	"putnami":       true,
	"qa":            true,
	"runtime":       true,
	"source":        true,
	"surfaces":      true,
	"tooling":       true,
}

// setupSlugParam reads --slug. A flag given with no value parses as the
// boolean true, which would name the workspace "true" for good: setup refuses
// it.
func setupSlugParam(params map[string]any) (string, error) {
	if _, bare := param(params, "slug").(bool); bare {
		return "", newError("--slug needs a value: putnami cloud setup --slug <slug>", ExitUsage)
	}
	return stringParam(params, "slug"), nil
}

// validateNewWorkspaceSlug checks the slug of a workspace that setup creates.
func validateNewWorkspaceSlug(slug string) error {
	if len(slug) > maxWorkspaceSlugLen {
		return newError(fmt.Sprintf("--slug %q is longer than %d characters", slug, maxWorkspaceSlugLen), ExitUsage)
	}
	if !workspaceSlugPattern.MatchString(slug) {
		return newError(fmt.Sprintf("--slug %q must be lowercase letters, digits and single dashes, starting with a letter (for example \"acme-prod\")", slug), ExitUsage)
	}
	if reservedWorkspaceSlugs[slug] {
		return newError(fmt.Sprintf("--slug %q is a platform name: a workspace with this slug holds no event topic name under it; choose another slug", slug), ExitUsage)
	}
	// The subscription ID of a push receive of "my-data.orders" reads
	// "data.orders" after a dash, a platform name, so Runtime refuses every
	// push receive of a topic named under such a slug.
	if last := slug[strings.LastIndexByte(slug, '-')+1:]; last != slug && reservedWorkspaceSlugs[last] {
		return newError(fmt.Sprintf("--slug %q ends with \"-%s\", a platform name: Runtime refuses every push receive of a topic named under this slug; choose another slug", slug, last), ExitUsage)
	}
	return nil
}

// checkLinkedWorkspaceSlug refuses --slug when setup links an existing
// workspace that has another slug. Setup sends the slug only when it creates
// the workspace, and a workspace keeps the slug it was created with. So the
// flag cannot rename a workspace, and setup says so instead of ignoring it.
// The same command still runs twice: the second run links the workspace the
// first run created, and its slug is the asked one.
func checkLinkedWorkspaceSlug(slug, workspaceID string, workspace *identityapi.Workspace2) error {
	if slug == "" {
		return nil
	}
	current := clicore.Deref(workspace.Slug)
	if current == slug {
		return nil
	}
	if current == "" {
		return newError(fmt.Sprintf("--slug %q cannot be checked: workspace %s answered no slug. A workspace keeps the slug it was created with, so run setup without --slug", slug, workspaceID), ExitUsage)
	}
	return newError(fmt.Sprintf("--slug %q does not apply: workspace %s already exists with the slug %q, and a workspace keeps the slug it was created with. Run setup without --slug", slug, workspaceID, current), ExitUsage)
}
