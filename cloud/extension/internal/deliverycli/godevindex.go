package deliverycli

import (
	"net/http"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// readGoDevIndex reads go.dev's machine-readable download index at indexURL
// (`?mode=json`): a JSON array of releases, each listing its artifacts with the
// sha256 go.dev publishes for them. go.dev is a third-party contract, not a
// Putnami provider, so the read is a plain request with the CLI's JSON
// handling: the CLI User-Agent, a 200-only answer, and a refusal that names
// the server's message.
func readGoDevIndex(client *http.Client, indexURL string) ([]goDistRelease, error) {
	req, err := http.NewRequest(http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, clicore.RequestBuildError(indexURL, err)
	}
	releases, err := clicore.SendJSONInto[[]goDistRelease](client, req, nil)
	if err != nil {
		return nil, err
	}
	return *releases, nil
}
