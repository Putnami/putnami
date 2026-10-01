package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// withRegistries puts a raw `registries` job-context member on a context, the
// way BuildJobContext does for every task of a project.
func withRegistries(t *testing.T, ctx *pctx.Context, body string) {
	t.Helper()
	if ctx.Params == nil {
		ctx.Params = pctx.Params{}
	}
	ctx.Params["registries"] = json.RawMessage(body)
}

func TestNpmRegistriesFromReadsTheDeclaredEntry(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	withRegistries(t, ctx, `{"npm":{"publish":"https://npm.putnami.dev","scopes":{"@putnami":"https://npm.putnami.dev"}},"oci":{"publish":"oci.putnami.dev/putnami"}}`)

	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		t.Fatal(err)
	}
	if declared.Publish != "https://npm.putnami.dev" {
		t.Errorf("publish = %q", declared.Publish)
	}
	if got := declared.registryForPackage("@putnami/web"); got != "https://npm.putnami.dev" {
		t.Errorf("scope registry = %q", got)
	}
	if got := declared.registryForPackage("lodash"); got != "" {
		t.Errorf("undeclared scope resolved to %q, want empty", got)
	}
}

func TestNpmRegistriesFromToleratesAnAbsentEntry(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil || declared.Publish != "" || len(declared.Scopes) != 0 {
		t.Fatalf("declared = %+v, err = %v", declared, err)
	}

	withRegistries(t, ctx, `{"oci":{"publish":"oci.putnami.dev/putnami"}}`)
	declared, err = npmRegistriesFrom(ctx.Params)
	if err != nil || declared.Publish != "" {
		t.Fatalf("declared = %+v, err = %v", declared, err)
	}

	withRegistries(t, ctx, `{"npm":"not an object"}`)
	if _, err := npmRegistriesFrom(ctx.Params); err == nil {
		t.Fatal("a malformed npm entry must be reported, not ignored")
	}
}

func TestNpmRegistriesFromRejectsUnsafeDeclarations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"npm":{"publish":"https://npm.example.test","unknown":true}}`},
		{"directive in scope", `{"npm":{"scopes":{"@safe\nalways-auth":"https://npm.example.test"}}}`},
		{"directive in registry", `{"npm":{"scopes":{"@safe":"https://npm.example.test\nalways-auth=true"}}}`},
		{"relative registry", `{"npm":{"scopes":{"@safe":"./registry"}}}`},
		{"credential in registry", `{"npm":{"publish":"https://token@npm.example.test"}}`},
		{"empty scope registry", `{"npm":{"scopes":{"@safe":"  "}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := makeTestCtx(t)
			withRegistries(t, ctx, tc.body)
			if _, err := npmRegistriesFrom(ctx.Params); err == nil {
				t.Fatal("unsafe registries.npm declaration was accepted")
			}
		})
	}
}

func TestPutnamiScopeRegistryHost(t *testing.T) {
	tests := []struct {
		name   string
		scopes map[string]string
		want   []string
	}{
		{"managed scope", map[string]string{"@putnami": "https://npm.putnami.dev"}, []string{"npm.putnami.dev"}},
		{"trailing path and port", map[string]string{"@putnami": "https://npm.example.test:8443/scoped/"}, []string{"npm.example.test"}},
		{"uppercase host is normalized", map[string]string{"@putnami": "https://NPM.Example.Test"}, []string{"npm.example.test"}},
		{"two scopes on one host are one call", map[string]string{"@a": "https://npm.example.test", "@b": "https://npm.example.test"}, []string{"npm.example.test"}},
		{"two scopes on two hosts", map[string]string{"@a": "https://a.example.test", "@b": "https://b.example.test"}, []string{"a.example.test", "b.example.test"}},
		{"no scopes", nil, nil},
		{"plain http is a local fixture", map[string]string{"@putnami": "http://127.0.0.1:4873"}, nil},
		{"not a url", map[string]string{"@putnami": "./local-mirror"}, nil},
		{"empty value", map[string]string{"@putnami": ""}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := putnamiScopeRegistryHost(npmRegistries{Scopes: tc.scopes})
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("hosts = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubNpmCredential replaces the seam and records the hosts it was asked about.
func stubNpmCredential(t *testing.T, outcome registrycred.Outcome) *[]string {
	t.Helper()
	original := ensureNpmRegistryCredential
	t.Cleanup(func() { ensureNpmRegistryCredential = original })
	var hosts []string
	ensureNpmRegistryCredential = func(_ context.Context, _ string, host string) registrycred.Outcome {
		hosts = append(hosts, host)
		return outcome
	}
	return &hosts
}

func TestRefreshPutnamiNpmCredential_AsksForTheScopeHostAndLogsTheOutcome(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	withRegistries(t, ctx, `{"npm":{"scopes":{"@putnami":"https://npm.putnami.dev"}}}`)
	hosts := stubNpmCredential(t, registrycred.Outcome{
		Kind: registrycred.KindNotSignedIn, Level: "warn",
		Message: "not signed in for npm.putnami.dev; " + registrycred.SignInHint,
	})

	events := captureEvents(t, func() { refreshPutnamiNpmCredential(ctx, jsonl.New()) })

	if len(*hosts) != 1 || (*hosts)[0] != "npm.putnami.dev" {
		t.Fatalf("hosts = %v, want [npm.putnami.dev]", *hosts)
	}
	logged := findEvent(events, func(e map[string]any) bool { return e["level"] == "warn" })
	if logged == nil {
		t.Fatalf("no warning emitted: %#v", events)
	}
	if msg, _ := logged["message"].(string); !strings.Contains(msg, "run putnami cloud login") {
		t.Errorf("warning = %q, want the exact sign-in hint", logged["message"])
	}
}

func TestRefreshPutnamiNpmCredential_SilentOutcomeLogsNothing(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	withRegistries(t, ctx, `{"npm":{"scopes":{"@putnami":"https://npm.putnami.dev"}}}`)
	stubNpmCredential(t, registrycred.Outcome{Kind: registrycred.KindCloudAbsent})

	events := captureEvents(t, func() { refreshPutnamiNpmCredential(ctx, jsonl.New()) })
	for _, e := range events {
		if e["kind"] == "log" {
			t.Fatalf("an already-reported outcome logged again: %#v", e)
		}
	}
}

func TestRefreshPutnamiNpmCredential_SkipsWithoutAnHttpsScopeRegistry(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"npm":{"publish":"https://npm.putnami.dev"}}`,
		`{"npm":{"scopes":{"@putnami":"http://127.0.0.1:4873"}}}`,
	} {
		ctx, _ := makeTestCtx(t)
		withRegistries(t, ctx, body)
		hosts := stubNpmCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})

		refreshPutnamiNpmCredential(ctx, jsonl.New())

		if len(*hosts) != 0 {
			t.Errorf("%q asked the cloud about %v, want no call", body, *hosts)
		}
	}
}
