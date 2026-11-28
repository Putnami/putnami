package deliverycli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	"go.putnami.dev/protocol/cache"
)

// markerScopeEnvPrefixes name the variables a session's configuration and
// credentials are read from: the PUTNAMI_CACHE_* overrides and token, the
// machine token and the hosted ingest, and the cloud endpoints.
var markerScopeEnvPrefixes = []string{"PUTNAMI_CACHE_", "PUTNAMI_CLOUD_", "PUTNAMI_ALLOW_INSECURE_", "PUTNAMI_AUTH_"}

// markerScopeEnv are the variables that locate the files of the scope, the
// control plane the token recipe asks, and the CLI it runs.
var markerScopeEnv = []string{"HOME", "USERPROFILE", "PUTNAMI_HOME", "PUTNAMI_CONTROL_PLANE_URL", "PUTNAMI_CLI_EXECUTABLE", "PATH"}

// markerScope is the scope of the session's run-marker answers
// (cache.MarkerScope): the endpoint, the credential and the Cloud workspace
// it resolved, and the files and variables they come from. A session names
// one only when every one of them is in those files and variables: the
// signed-in user's recipe, which reads ~/.putnami/auth.json, or a token from
// a variable. A configured command, a token URL, a metadata identity or a
// hosted run's credential, which arrives in authenticate, is read from
// elsewhere, so the session names no scope and core publishes every marker.
func (s *providerSession) markerScope(cfg *remotecache.Config) *cache.MarkerScope {
	if cfg == nil || !cfg.Active() || s.hosted || s.runCredentialCaller != nil {
		return nil
	}
	if !cfg.SendsSessionRecipe() && !cfg.SendsMachineToken() && cfg.Token.Class() != remotecache.TokenClassEnv {
		return nil
	}
	workspaceID := ""
	if link, err := clicore.ReadCloudLink(s.workspaceRoot); err == nil {
		workspaceID = clicore.StringValue(link["workspace_id"])
	}
	sum := sha256.New()
	for _, field := range []string{cfg.URL, string(cfg.Token.Class()), workspaceID,
		fmt.Sprint(s.readOnly), fmt.Sprint(s.hosted), providerVersion()} {
		_, _ = fmt.Fprintf(sum, "%d:%s", len(field), field)
	}
	files := []string{
		CachePath(s.workspaceRoot),
		clicore.LinkPath(s.workspaceRoot),
		filepath.Join(s.workspaceRoot, "putnami.workspace.json"),
		filepath.Join(s.workspaceRoot, "putnami.json"),
	}
	// The signed-in user's credential, as the token recipe reads it.
	files = append(files, filepath.Join(clicore.PutnamiHome(nil), "auth.json"))
	for i, file := range files {
		abs, err := filepath.Abs(file)
		if err != nil {
			return nil
		}
		files[i] = abs
	}
	return &cache.MarkerScope{
		Key:         "sha256:" + hex.EncodeToString(sum.Sum(nil)),
		Files:       files,
		Env:         markerScopeEnv,
		EnvPrefixes: markerScopeEnvPrefixes,
	}
}
