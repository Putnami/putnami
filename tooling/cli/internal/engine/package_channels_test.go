package engine

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// packagingWorkspace writes, at root, a workspace with two projects, api and
// web, and an extension whose package command builds every project and
// writes an image only on its docker channel, as the language extensions do.
// apiConfig is api's putnami.json.
func packagingWorkspace(t *testing.T, root, apiConfig string) *workspacepb.Config {
	t.Helper()
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/pack", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		Commands: map[string]extensionproto.CommandDefinition{"package": {
			Traits: &extensionproto.CommandTraits{InjectPublishChannels: true},
			Flags: map[string]extensionproto.FlagDefinition{
				"docker":  {Type: "boolean", Default: false},
				"dry-run": {Type: "boolean", Default: false},
			},
			Run: []extensionproto.PipelineStep{
				{ID: "compile", Task: "noop"},
				{ID: "docker", Task: "noop", DependsOn: []string{"compile"}, If: "params.docker"},
			},
		}},
		Tasks: map[string]extensionproto.TaskDefinition{
			"noop": {Kind: "command", Command: "true", Cwd: "{projectRoot}", TimeoutMs: -1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		"putnami.workspace.json":           `{"name":"packaging","includes":["api","web","extension"]}`,
		"api/putnami.json":                 apiConfig,
		"web/putnami.json":                 `{"name":"web","extensions":["@test/pack"]}`,
		"extension/putnami.json":           `{"name":"@test/pack"}`,
		"extension/putnami.extension.json": string(manifest),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	return workspacepb.Load(root)
}

// A package run that would package nothing fails for a project the caller
// named, with the option and the flag that turn a channel on, and warns for a
// project an --all selection reached, keeping its exit code. A project with a
// channel on is never named.
func TestPackage_WithNoChannelOnFailsWhenNamedAndWarnsOtherwise(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-package-run-with-no-channel-on-says-which-option-to-set", "fail-when-named-warn-otherwise")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	const hint = `Turn one on under options.package in the project's putnami.json, such as "options": {"package": {"docker": true}}, or pass its flag: --docker`

	cases := []struct {
		name      string
		apiConfig string
		global    GlobalFlags
		params    extension.ParamMap
		watch     bool
		wantCode  int
		wantError string
		wantWarn  string
	}{
		{
			name:      "named",
			apiConfig: `{"name":"api","extensions":["@test/pack"]}`,
			global:    GlobalFlags{Projects: "api"},
			wantCode:  ExitUsage,
			wantError: "putnami: package packages nothing for /api: no channel is on. " + hint,
		},
		{
			name:      "named with its flag",
			apiConfig: `{"name":"api","extensions":["@test/pack"]}`,
			global:    GlobalFlags{Projects: "api"},
			params:    extension.ParamMap{"docker": true},
			wantCode:  ExitSuccess,
		},
		{
			name:      "named with options.package",
			apiConfig: `{"name":"api","extensions":["@test/pack"],"options":{"package":{"docker":true}}}`,
			global:    GlobalFlags{Projects: "api"},
			wantCode:  ExitSuccess,
		},
		{
			name:      "a glob that reaches one project",
			apiConfig: `{"name":"api","extensions":["@test/pack"],"options":{"package":{"docker":true}}}`,
			global:    GlobalFlags{Projects: "/web/...,api"},
			wantCode:  ExitSuccess,
			wantWarn:  "package packages nothing for /web: no channel is on. " + hint,
		},
		{
			name:      "a watch iteration, whose selection the loop spells as IDs",
			apiConfig: `{"name":"api","extensions":["@test/pack"],"options":{"package":{"docker":true}}}`,
			global:    GlobalFlags{Projects: "/api,/web"},
			watch:     true,
			wantCode:  ExitSuccess,
			wantWarn:  "package packages nothing for /web: no channel is on. " + hint,
		},
		{
			name:      "all",
			apiConfig: `{"name":"api","extensions":["@test/pack"],"options":{"package":{"docker":true}}}`,
			global:    GlobalFlags{Projects: "*", All: true},
			wantCode:  ExitSuccess,
			// The extension project packages itself and offers the same channel.
			wantWarn: "package packages nothing for /web, /extension: no channel is on. " +
				strings.Replace(hint, "the project's", "each project's", 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := rootWithoutRepository(t)
			cfg := packagingWorkspace(t, root, tc.apiConfig)
			global := tc.global
			global.Plan = true
			var result SessionResult
			var runErr error
			stderr := captureStderr(t, func() {
				result, runErr = New().Run(context.Background(), Request{
					WorkspaceRoot: root, Config: cfg, Commands: []string{"package"},
					CommandParams: tc.params, Global: global, Stdout: io.Discard, ErrorOut: io.Discard,
					watchIteration: tc.watch,
				}, nil)
			})
			if result.ExitCode != tc.wantCode {
				t.Fatalf("exit code = %d (%v), want %d; stderr=%q", result.ExitCode, runErr, tc.wantCode, stderr)
			}
			if tc.wantError != "" && !strings.Contains(stderr, tc.wantError+"\n") {
				t.Fatalf("stderr = %q, want %q", stderr, tc.wantError)
			}
			var warned []string
			for _, warning := range result.WorkspaceWarnings {
				if warning.Code == workspace.WarningCodePackageChannelsOff {
					warned = append(warned, warning.Message)
				}
			}
			switch {
			case tc.wantWarn == "" && len(warned) != 0:
				t.Fatalf("warnings = %q, want none", warned)
			case tc.wantWarn != "" && (len(warned) != 1 || warned[0] != tc.wantWarn):
				t.Fatalf("warnings = %q, want %q", warned, tc.wantWarn)
			}
		})
	}
}
