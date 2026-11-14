package collabcli

import (
	"errors"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// errNoSourceClient is the refusal of a build whose generated source-api
// client does not carry Source's collaboration routes yet.
var errNoSourceClient = errors.New("the source-api client of this build does not carry the collaboration routes of Putnami Source")

// connectSource binds Source's generated client to the workspace's control
// plane and bearer.
func connectSource(*clicore.WorkspaceContext) (Source, error) {
	return nil, errNoSourceClient
}
