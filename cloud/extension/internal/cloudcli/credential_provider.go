package cloudcli

import (
	stdcontext "context"
	"time"

	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	registry "go.putnami.dev/protocol/registry"
)

// credentialProviderRequestTimeout bounds each auth-server request of the
// developer-machine read credential. The server bounds the whole answer.
const credentialProviderRequestTimeout = 10 * time.Second

// credentialProvider serves the engine's credential-provider RPC. Delivery
// owns the server and the hosted run's install credential; Distribution owns
// the developer machine's registry bearer. This command joins the two, since
// neither domain library may import the other.
//
// The protocol owns stdout, so the developer-machine source runs with an IO
// that writes nothing to it. Its own warnings are dropped as well: they are not
// redacted, and the server reports the source's error on stderr in a bounded,
// redacted line. The server's ctx reaches the source, so a read the server
// stopped waiting for stops too, and stores nothing.
func credentialProvider(params map[string]any, workspaceRoot string, env map[string]string, ioctx IO) error {
	client := *clientOrDefault(ioctx.Client)
	client.Timeout = credentialProviderRequestTimeout
	quiet := IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: &client,
		Now:    nowOrDefault(ioctx.Now),
	}
	local := func(ctx stdcontext.Context) (*registry.Credential, error) {
		return distributioncli.ProviderReadCredential(ctx, params, workspaceRoot, env, quiet)
	}
	return deliverycli.CredentialProvider(env, local)
}
