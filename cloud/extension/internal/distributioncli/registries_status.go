package distributioncli

import (
	"fmt"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The registries status: one check per registry this machine can
// sign in to. A registry without a stored key mints its credential on demand;
// a stored key is read from auth-server.

// RegistryAnswer is what this machine and auth-server say about one
// registry. Configured is false when registries.json records no endpoint for
// it. KeyID is empty when the credential is minted on demand. The key fields
// come from GET /apikeys/{id} when KeyID is set; Err says why that read
// failed.
type RegistryAnswer struct {
	Registry   Registry
	Configured bool
	Host       string
	URL        string
	KeyID      string
	KeyPrefix  string
	Active     bool
	NotFound   bool
	RevokedAt  string
	ExpiresAt  string
	LastUsedAt string
	Err        error
}

// registriesStatusFix records every registry endpoint again; it also clears
// a key auth-server no longer knows.
const registriesStatusFix = "putnami cloud registries setup"

// registriesShowStatus serves `putnami cloud registries status [<registry>]`.
// A session that cannot be read before any key is read keeps its exit 3
// error; every other failed read is an unknown check.
func registriesShowStatus(params map[string]any, args []string, env map[string]string, ioctx clicore.IO) error {
	positionals := clicore.Positionals(args)
	if len(positionals) > 2 {
		return clicore.NewError("cloud registries status takes at most one registry", clicore.ExitUsage)
	}
	var only Registry
	if len(positionals) == 2 {
		registry, ok := registryByName(positionals[1])
		if !ok {
			return clicore.NewError("unknown registry "+positionals[1]+"; expected npm, go, oci or put", clicore.ExitUsage)
		}
		only = registry
	}
	answers, err := readRegistryAnswers(params, env, ioctx, only)
	if err != nil {
		if clicore.ExitCode(err) == clicore.ExitAuth {
			return err
		}
		return clicore.WriteStatus(params, ioctx, clicore.UnknownStatus("registries", "registries", err, ""))
	}
	if only != "" {
		return clicore.WriteStatus(params, ioctx, RegistryStatusNodeFrom(answers[0]))
	}
	return clicore.WriteStatus(params, ioctx, RegistriesStatusNodeFrom(answers))
}

// RegistriesStatusNode is the registries status of this machine, for every
// registry. It is the same node `putnami cloud registries status` prints.
// The registries are set up per machine, so the workspace root is not read.
func RegistriesStatusNode(params map[string]any, _ string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	answers, err := readRegistryAnswers(params, env, ioctx, "")
	if err != nil {
		fix := ""
		if clicore.ExitCode(err) == clicore.ExitAuth {
			fix = "putnami cloud login"
		}
		return clicore.UnknownStatus("registries", "registries", err, fix)
	}
	return RegistriesStatusNodeFrom(answers)
}

// readRegistryAnswers reads registries.json and, for each stored key, the key
// from auth-server: at most one call per registry, in sequence. The session
// is read only when a stored key needs it; when it cannot be read, the error
// is returned so the command keeps its exit code. only limits the read to one
// registry.
func readRegistryAnswers(params map[string]any, env map[string]string, ioctx clicore.IO, only Registry) ([]RegistryAnswer, error) {
	state, err := readRegistriesState(env)
	if err != nil {
		return nil, err
	}
	refs := map[Registry]KeyRef{}
	if state != nil {
		for _, ref := range state.Keys {
			refs[ref.Registry] = ref
		}
	}
	registries := AllRegistries
	if only != "" {
		registries = []Registry{only}
	}
	answers := make([]RegistryAnswer, 0, len(registries))
	accessToken := ""
	ctx := commandContext(ioctx)
	authURL := clicore.AuthBaseURL(params, env, "")
	for _, registry := range registries {
		ref, configured := refs[registry]
		answer := RegistryAnswer{Registry: registry, Configured: configured, Host: ref.Host, URL: ref.URL, KeyID: ref.ID, KeyPrefix: ref.Prefix}
		if configured && ref.ID != "" {
			if accessToken == "" {
				auth, err := clicore.ActiveAuth(params, env, ioctx)
				if err != nil {
					return nil, err
				}
				accessToken = auth.AccessToken
			}
			key, err := getAPIKeyStatus(ctx, ioctx.Client, authURL, accessToken, ref.ID, ioctx.Now())
			answer.Err = err
			answer.Active = key.Active
			answer.NotFound = key.NotFound
			answer.RevokedAt = key.RevokedAt
			answer.ExpiresAt = key.ExpiresAt
			answer.LastUsedAt = key.LastUsedAt
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

// RegistriesStatusNodeFrom folds one answer per registry. The node takes the
// worst registry.
//
//	not configured                 degraded
//	no stored key                  ok, on demand
//	active key                     ok
//	key auth-server does not know  degraded, stale key
//	revoked or expired key         failing
//	key not read                   unknown
func RegistriesStatusNodeFrom(answers []RegistryAnswer) clicore.StatusNode {
	node := clicore.StatusNode{ID: "registries", Title: "registries"}
	configured := 0
	type group struct {
		phrase string
		names  []string
	}
	var groups []*group
	for _, answer := range answers {
		child := registryCheck(answer)
		node.Children = append(node.Children, child)
		if answer.Configured {
			configured++
		}
		phrase := registryPhrase(answer)
		var found *group
		for _, g := range groups {
			if g.phrase == phrase {
				found = g
			}
		}
		if found == nil {
			found = &group{phrase: phrase}
			groups = append(groups, found)
		}
		found.names = append(found.names, child.Title)
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, strings.Join(g.names, ", ")+" "+g.phrase)
	}
	node.Detail = strings.Join(parts, "; ")
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("registries_configured", "registries configured", configured, clicore.MetricHealth).Of(float64(len(AllRegistries))),
	}
	return node
}

// RegistryStatusNodeFrom is the status of one registry, as the root node:
// its check, with the endpoint and the key as checks under it.
func RegistryStatusNodeFrom(answer RegistryAnswer) clicore.StatusNode {
	node := registryCheck(answer)
	if !answer.Configured {
		return node
	}
	node.Children = append(node.Children, clicore.StatusNode{
		ID: node.ID + ".endpoint", Title: "endpoint", State: clicore.StatusOK, Detail: clicore.FirstString(answer.URL, answer.Host),
	})
	if answer.KeyID != "" && answer.Err == nil && !answer.NotFound {
		node.Children = append(node.Children, clicore.StatusNode{
			ID: node.ID + ".key", Title: "key", State: node.State, Fix: node.Fix,
			Detail: fmt.Sprintf("%s, expires %s, last used %s", answer.KeyPrefix, dashNever(answer.ExpiresAt), dashNever(answer.LastUsedAt)),
		})
	}
	return node
}

func registryCheck(answer RegistryAnswer) clicore.StatusNode {
	name := registryTokenPurpose(answer.Registry)
	node := clicore.StatusNode{ID: "registries." + name, Title: name}
	switch {
	case !answer.Configured:
		node.State = clicore.StatusDegraded
		node.Detail = "not configured on this machine"
		node.Fix = registriesStatusFix
	case answer.KeyID == "":
		node.State = clicore.StatusOK
		node.Detail = "on demand, " + answer.Host
	case answer.Err != nil:
		node = clicore.UnknownStatus(node.ID, node.Title, answer.Err, "")
		node.Detail = "key " + answer.KeyPrefix + " not read: " + node.Detail
	case answer.NotFound:
		node.State = clicore.StatusDegraded
		node.Detail = "stale key " + answer.KeyPrefix + ": auth-server no longer knows it"
		node.Fix = registriesStatusFix
	case answer.RevokedAt != "":
		node.State = clicore.StatusFailing
		node.Detail = "key " + answer.KeyPrefix + " revoked at " + answer.RevokedAt
		node.Fix = registriesStatusFix
	case !answer.Active:
		node.State = clicore.StatusFailing
		node.Detail = "key " + answer.KeyPrefix + " expired at " + answer.ExpiresAt
		node.Fix = registriesStatusFix
	default:
		node.State = clicore.StatusOK
		node.Detail = "key " + answer.KeyPrefix + " active, " + answer.Host
	}
	return node
}

// registryPhrase is the short state of one registry in the node detail.
func registryPhrase(answer RegistryAnswer) string {
	switch {
	case !answer.Configured:
		return "not configured"
	case answer.KeyID == "":
		return "on demand"
	case answer.Err != nil:
		return "key not read"
	case answer.NotFound:
		return "stale key"
	case answer.RevokedAt != "":
		return "key revoked"
	case !answer.Active:
		return "key expired"
	default:
		return "key active"
	}
}

// registryByName reads a registry name as a user types it: the `--for` kind
// (npm, go, oci, put) or the stored name (gomod).
func registryByName(name string) (Registry, bool) {
	for _, registry := range AllRegistries {
		if name == string(registry) || name == registryTokenPurpose(registry) {
			return registry, true
		}
	}
	return "", false
}

func dashNever(value string) string {
	if strings.TrimSpace(value) == "" {
		return "never"
	}
	return value
}
