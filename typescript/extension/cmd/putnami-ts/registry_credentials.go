package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// npmRegistriesKey is the ecosystem id this extension owns, and therefore the
// key of its entry in the workspace `registries` section.
const npmRegistriesKey = npmEcosystem

var npmRegistryScope = regexp.MustCompile(`^@[a-z0-9][a-z0-9._-]*$`)

// npmRegistries is the npm entry of the workspace `registries` section — the
// shape this extension's ecosystem profile declares in putnami.extension.json.
//
// It is the single declared source of every npm endpoint this extension needs:
// the registry a publication uploads to, and the registry each package scope
// resolves from. Nothing here is hard-coded any more, and no endpoint is read
// back out of a dotfile the extension itself wrote.
type npmRegistries struct {
	// Publish is the registry a publication uploads to.
	Publish string `json:"publish"`
	// Scopes maps an npm scope ("@putnami") to the registry that serves it.
	Scopes map[string]string `json:"scopes"`
}

// npmRegistriesFrom reads the npm entry out of the job context's `registries`
// member. An absent member or an absent npm entry is not an error: a workspace
// that declares no registries publishes and resolves against npm's own
// defaults.
func npmRegistriesFrom(params pctx.Params) (npmRegistries, error) {
	raw, ok := params["registries"]
	if !ok || len(raw) == 0 {
		return npmRegistries{}, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return npmRegistries{}, fmt.Errorf("parsing the registries job-context member: %w", err)
	}
	entry, ok := entries[npmRegistriesKey]
	if !ok || len(entry) == 0 {
		return npmRegistries{}, nil
	}
	var declared npmRegistries
	decoder := json.NewDecoder(bytes.NewReader(entry))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		return npmRegistries{}, fmt.Errorf("parsing the registries.npm entry: %w", err)
	}
	var err error
	if declared.Publish, err = validatedNPMRegistryURL("registries.npm.publish", declared.Publish, true); err != nil {
		return npmRegistries{}, err
	}
	for scope, registry := range declared.Scopes {
		if !npmRegistryScope.MatchString(scope) {
			return npmRegistries{}, fmt.Errorf("registries.npm.scopes contains an invalid npm scope")
		}
		validated, validateErr := validatedNPMRegistryURL("registries.npm.scopes", registry, false)
		if validateErr != nil {
			return npmRegistries{}, validateErr
		}
		declared.Scopes[scope] = validated
	}
	return declared, nil
}

// validatedNPMRegistryURL admits only a registry endpoint that can be written
// as one .npmrc value without smuggling another directive or a repository-owned
// credential. Scope registries are declarations, so an empty value is an error;
// publish remains optional and falls back to npm's native default when absent.
func validatedNPMRegistryURL(field, raw string, optional bool) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("%s must not be empty", field)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must be one URL on one line", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) URL", field)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not contain credentials, a query, or a fragment", field)
	}
	return value, nil
}

// scopeNames returns the declared scopes, sorted, so every file this extension
// writes from them is byte-stable.
func (r npmRegistries) scopeNames() []string {
	names := make([]string, 0, len(r.Scopes))
	for scope := range r.Scopes {
		if strings.TrimSpace(scope) == "" || strings.TrimSpace(r.Scopes[scope]) == "" {
			continue
		}
		names = append(names, scope)
	}
	sort.Strings(names)
	return names
}

// registryForPackage returns the registry that serves a package: the entry for
// its scope when the workspace declares one, otherwise nothing. A package whose
// scope the workspace says nothing about is resolved by npm's own default, not
// by a guess made here.
func (r npmRegistries) registryForPackage(packageName string) string {
	scope := npmScopeOf(packageName)
	if scope == "" {
		return ""
	}
	return strings.TrimSpace(r.Scopes[scope])
}

// npmScopeOf returns the "@scope" prefix of a package name, or "" when the
// package is unscoped.
func npmScopeOf(packageName string) string {
	if !strings.HasPrefix(packageName, "@") {
		return ""
	}
	slash := strings.IndexByte(packageName, '/')
	if slash <= 1 {
		return ""
	}
	return packageName[:slash]
}

// npmScopeRegistryLine renders the workspace .npmrc line that binds one scope
// to its registry. It is the only spelling this extension writes, and the
// prefix it matches on when it replaces a stale line.
func npmScopeRegistryLine(scope, registry string) string {
	return npmScopeRegistryPrefix(scope) + registry
}

// npmScopeRegistryPrefix is the key half of a scope registry line.
func npmScopeRegistryPrefix(scope string) string {
	return scope + ":registry="
}

// ensureNpmRegistryCredential is a package var so tests exercise every outcome
// class without a `putnami` binary.
var ensureNpmRegistryCredential = registrycred.EnsureNativeCredential

// refreshPutnamiNpmCredential asks @putnami/cloud to write the user's npm
// credential for every scope registry the workspace declares, so `bun install`
// can read private packages from them.
//
// Installs never refreshed credentials before: the workspace .npmrc named the
// registry and nothing supplied a token for it, so a laptop could only install
// the private 0.2 packages if someone had hand-edited ~/.npmrc. The cloud writes
// the `//<host>/:_authToken=` line itself; this extension only names the host.
//
// It never fails the install. Every failure mode — no cloud, no session, a cloud
// that does not implement the shape yet — logs one line and lets bun run with
// whatever credential the machine already has.
func refreshPutnamiNpmCredential(ctx *pctx.Context, emit *jsonl.Emitter) {
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		emit.Log("warn", err.Error())
		return
	}
	for _, host := range putnamiScopeRegistryHost(declared) {
		outcome := ensureNpmRegistryCredential(context.Background(), ctx.WorkspaceRoot, host)
		if outcome.Message != "" {
			emit.Log(outcome.Level, outcome.Message)
		}
	}
}

// putnamiScopeRegistryHost returns the hosts that need a credential: the host
// of every scope registry the workspace `registries` entry declares, sorted and
// deduplicated.
//
// It reads the DECLARATION, not the .npmrc the install writes from it: a file
// this extension generates is not evidence of what the workspace asked for, and
// reading it back made the credential refresh depend on the order the install
// happened to run its steps in.
//
// A relative or non-https value, and a URL with no host, yield nothing: reading
// metadata from a default is harmless; minting a credential for a host the
// workspace never asked for is not.
func putnamiScopeRegistryHost(declared npmRegistries) []string {
	seen := make(map[string]bool, len(declared.Scopes))
	var hosts []string
	for _, scope := range declared.scopeNames() {
		host := httpsHost(declared.Scopes[scope])
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// httpsHost returns the lowercase host of an https URL, or "" for anything else.
// A plain-http or file registry is a local fixture, not a place to send a
// user's credential.
func httpsHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
