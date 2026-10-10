package distributioncli

import (
	"fmt"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// WriteRegistryTokenRecipes records the configured endpoint index plus, for
// each registry, the token recipe a native client runs to obtain a bearer:
// `putnami cloud token --for <kind>`. The recipe is safe to write at login
// because it names only the registry KIND — no owner workspace, no package, no
// action. The registry derives what the user may read or write per namespace
// from IAM, so a recipe cannot widen authority.
//
// This writes ~/.putnami/registries.json ONLY. It never touches a native
// credential store (~/.npmrc, ~/.netrc, the Docker config): those files may
// hold a credential the user still needs, and this function runs from
// `cloud install` and `cloud setup`, both of which fire routinely and
// non-interactively (every workspace install, every CI run). An install must
// never remove a valid credential — see the 2026-09-02/03 regression where
// every `putnami install` deleted a working ~/.npmrc `_authToken` line,
// breaking `bun install` of private packages until the line was restored by
// hand. Tearing down native credentials is reserved for an explicit action:
// `cloud login` and `cloud registries setup` call
// ScrubLegacyNativeRegistryCredentials themselves to migrate a session off
// static credentials, and `cloud logout` / `cloud registries logout` call
// TeardownRegistries.
func WriteRegistryTokenRecipes(params map[string]any, env map[string]string) (*RegistriesState, error) {
	state, err := readRegistriesState(env)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &RegistriesState{Version: 1}
	}
	if state.Version == 0 {
		state.Version = 1
	}
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		ref := KeyRef{
			Registry: endpoint.Registry,
			Host:     endpoint.Host,
			URL:      endpoint.URL,
		}
		if recipe := RegistryTokenRecipe(endpoint.Registry); len(recipe) > 0 {
			ref.Token = &clicore.TokenSource{Command: recipe}
		}
		if existing, ok := registryKeyByRegistry(state, endpoint.Registry); ok {
			ref.ID = existing.ID
			ref.Prefix = existing.Prefix
			ref.Name = existing.Name
			ref.CreatedAt = existing.CreatedAt
		}
		upsertRegistryKey(state, ref)
		removeRegistryAccess(state, endpoint.Host)
	}
	if err := WriteRegistriesState(env, state); err != nil {
		return nil, err
	}
	return state, nil
}

// ScrubLegacyNativeRegistryCredentials removes legacy static per-host entries
// from the native credential stores (~/.npmrc, ~/.netrc, the Docker config)
// for every configured registry endpoint, plus the matching raw entries in
// registries.json's own Auth map. This is explicit, user-initiated cleanup —
// it exists to migrate a session off static credentials onto the token-recipe
// resolver — and callers must run it only from an explicit action:
// `cloud login` (a fresh session is the natural point to drop stale static
// auth) and `cloud registries setup` (which documents this as its job).
// Automatic paths (`cloud install`, `cloud setup`) must never call it: see
// WriteRegistryTokenRecipes for why routine, non-interactive runs must not
// touch native credential files at all.
func ScrubLegacyNativeRegistryCredentials(params map[string]any, env map[string]string, state *RegistriesState) error {
	if state == nil {
		return nil
	}
	for _, endpoint := range resolveRegistryEndpoints(params, env) {
		switch endpoint.Registry {
		case RegistryNPM:
			delete(state.Auth, endpoint.Host)
			if err := removeNpmrcAuth(env, endpoint.Host); err != nil {
				return err
			}
		case RegistryGomod:
			if err := (gomodWriter{}).Teardown(env, endpoint.Host); err != nil {
				return err
			}
		case RegistryOCI:
			delete(state.Auth, endpoint.Host)
			if err := removeDockerCredential(env, endpoint.Host); err != nil {
				return err
			}
		}
	}
	if len(state.Auth) == 0 {
		state.Auth = nil
	}
	return WriteRegistriesState(env, state)
}

// TeardownRegistries revokes every recorded api key and clears every
// associated client-config entry. Run from `cloud logout`; idempotent when
// the state file is missing (treated as no-op).
func TeardownRegistries(params map[string]any, env map[string]string, ioctx clicore.IO, accessToken string) error {
	state, err := readRegistriesState(env)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	authURL := clicore.AuthBaseURL(params, env, "")
	for _, ref := range state.Keys {
		if accessToken != "" && ref.ID != "" {
			if rerr := revokeAPIKey(ioctx.Client, authURL, accessToken, ref.ID); rerr != nil {
				ioctx.Stderr(fmt.Sprintf("warning: failed to revoke %s key %s: %s", ref.Registry, ref.Prefix, rerr.Error()))
			}
		}
		if w := writerFor(ref.Registry); w != nil {
			if werr := w.Teardown(env, ref.Host); werr != nil {
				ioctx.Stderr(fmt.Sprintf("warning: failed to clear %s config for %s: %s", ref.Registry, ref.Host, werr.Error()))
			}
		}
	}
	for host := range state.PutAuth {
		if w := writerFor(RegistryPut); w != nil {
			_ = w.Teardown(env, host)
		}
	}
	return removeRegistriesState(env)
}

// Registries is the subcommand handler that exposes setup/status/logout
// as visible operations alongside the implicit calls from login/logout.
// The user-facing trio:
//
//	putnami cloud registries setup      record endpoints + clear unsafe native auth
//	putnami cloud registries status     check each registry and its stored key
//	putnami cloud registries logout     selective or full revoke
//
// `setup` needs no session because it mints nothing. Logout uses an active
// session when available to revoke legacy keys, while status remains readable
// for local/JSON inspection.
func Registries(params map[string]any, args []string, env map[string]string, ioctx clicore.IO) error {
	sub := clicore.FirstPositional(args)
	switch sub {
	case "", "status":
		return registriesShowStatus(params, args, env, ioctx)
	case "setup":
		return registriesRunSetup(params, env, ioctx)
	case "logout":
		return registriesRunLogout(params, env, ioctx)
	default:
		return clicore.NewError("unknown registries subcommand: "+sub, clicore.ExitUsage)
	}
}

func registriesRunSetup(params map[string]any, env map[string]string, ioctx clicore.IO) error {
	state, err := WriteRegistryTokenRecipes(params, env)
	if err != nil {
		return err
	}
	// `registries setup` is an explicit, user-run command (see the doc
	// comment on Registries above: "record endpoints + clear unsafe native
	// auth"), so it also migrates away any legacy static native credential —
	// unlike the automatic `cloud install` / `cloud setup` paths, which must
	// leave native credential files untouched.
	if err := ScrubLegacyNativeRegistryCredentials(params, env, state); err != nil {
		return err
	}
	clicore.WriteResult(
		map[string]any{"registries": RegistriesSummary(state)},
		params,
		ioctx,
		fmt.Sprintf("Configured %d registry endpoint%s; exact credentials are acquired per target.", len(state.Keys), pluralS(len(state.Keys))),
	)
	return nil
}

func registriesRunLogout(params map[string]any, env map[string]string, ioctx clicore.IO) error {
	// `clicore.ActiveAuth` is best-effort here: if the session has expired, we still
	// want to clear the local files. Server-side keys stay tracked in
	// state.Keys until a logout/teardown with an active session revokes
	// them (or the user hits the auth admin UI); `registries setup` only
	// rewrites endpoint recipes and removes legacy native credentials
	// locally — it never revokes.
	accessToken := ""
	if auth, err := clicore.ActiveAuth(params, env, ioctx); err == nil {
		accessToken = auth.AccessToken
	} else {
		ioctx.Stderr("warning: no active session, removing local registry credentials only")
	}
	only := clicore.StringParam(params, "registry")
	if only != "" {
		return registriesRunSelectiveLogout(params, env, ioctx, accessToken, Registry(only))
	}
	if err := TeardownRegistries(params, env, ioctx, accessToken); err != nil {
		return err
	}
	clicore.WriteResult(map[string]any{"registries_cleared": true}, params, ioctx, "Cleared all registry credentials.")
	return nil
}

func registriesRunSelectiveLogout(params map[string]any, env map[string]string, ioctx clicore.IO, accessToken string, target Registry) error {
	state, err := readRegistriesState(env)
	if err != nil {
		return err
	}
	if state == nil {
		clicore.WriteResult(map[string]any{"registries_cleared": false}, params, ioctx, "No registry credentials to clear.")
		return nil
	}
	authURL := clicore.AuthBaseURL(params, env, "")
	remaining := state.Keys[:0]
	cleared := false
	for _, ref := range state.Keys {
		if ref.Registry != target {
			remaining = append(remaining, ref)
			continue
		}
		cleared = true
		if accessToken != "" && ref.ID != "" {
			if rerr := revokeAPIKey(ioctx.Client, authURL, accessToken, ref.ID); rerr != nil {
				ioctx.Stderr(fmt.Sprintf("warning: failed to revoke %s key: %s", ref.Registry, rerr.Error()))
			}
		}
		if w := writerFor(ref.Registry); w != nil {
			if werr := w.Teardown(env, ref.Host); werr != nil {
				ioctx.Stderr(fmt.Sprintf("warning: failed to clear %s config: %s", ref.Registry, werr.Error()))
			}
		}
		delete(state.Auth, ref.Host)
		if len(state.Auth) == 0 {
			state.Auth = nil
		}
		delete(state.PutAuth, ref.Host)
		if len(state.PutAuth) == 0 {
			state.PutAuth = nil
		}
	}
	if !cleared {
		clicore.WriteResult(map[string]any{"registries_cleared": false, "registry": string(target)}, params, ioctx, fmt.Sprintf("No %s credentials to clear.", target))
		return nil
	}
	state.Keys = remaining
	if err := WriteRegistriesState(env, state); err != nil {
		return err
	}
	clicore.WriteResult(map[string]any{"registries_cleared": true, "registry": string(target)}, params, ioctx, fmt.Sprintf("Cleared %s credentials.", target))
	return nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// RegistriesSummary projects state down to the fields safe for JSON output
// (no raw tokens, no put_auth secrets) so login responses can advertise
// which registries were configured.
func RegistriesSummary(state *RegistriesState) []map[string]any {
	if state == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(state.Keys))
	for _, ref := range state.Keys {
		row := map[string]any{
			"registry": string(ref.Registry),
			"host":     ref.Host,
			"url":      ref.URL,
			"token":    ref.Token != nil,
		}
		if ref.ID != "" {
			row["key_id"] = ref.ID
			row["key_prefix"] = ref.Prefix
		}
		out = append(out, row)
	}
	return out
}

func registryKeyByRegistry(state *RegistriesState, registry Registry) (KeyRef, bool) {
	if state == nil {
		return KeyRef{}, false
	}
	for _, ref := range state.Keys {
		if ref.Registry == registry {
			return ref, true
		}
	}
	return KeyRef{}, false
}

func registryKeyByHost(state *RegistriesState, host string) (KeyRef, bool) {
	if state == nil {
		return KeyRef{}, false
	}
	for _, ref := range state.Keys {
		if ref.Host == host {
			return ref, true
		}
	}
	return KeyRef{}, false
}

func upsertRegistryKey(state *RegistriesState, ref KeyRef) {
	for i := range state.Keys {
		if state.Keys[i].Registry == ref.Registry {
			state.Keys[i] = ref
			return
		}
	}
	state.Keys = append(state.Keys, ref)
}
