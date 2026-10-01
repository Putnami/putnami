package configextract

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// capturePhaseStatus runs the config-extract job and returns the status string
// carried by the phase-end event emitted on stdout.
func capturePhaseStatus(t *testing.T, ctx *pctx.Context) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	_, _, runErr := Run(ctx, jsonl.New(), nil)

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	status := ""
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		// Phase-end events carry an "action" of "end" and a "status".
		if event["type"] == "phase" && event["action"] == "end" {
			if s, ok := event["status"].(string); ok {
				status = s
			}
		}
	}
	_ = runErr
	return status
}

// canonicalPhaseStatuses is the runtime protocol's PhaseStatus vocabulary.
var canonicalPhaseStatuses = map[string]bool{"success": true, "failed": true, "skipped": true}

func TestRun_PhaseEndSkippedIsCanonical(t *testing.T) {
	// A project with no config definitions takes the !ok branch, which must
	// emit a canonical "skipped" phase status (not the legacy "SKIP").
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	status := capturePhaseStatus(t, ctx)
	if status != "skipped" {
		t.Fatalf("phase-end status = %q, want %q", status, "skipped")
	}
	if !canonicalPhaseStatuses[status] {
		t.Fatalf("phase-end status %q is not in the canonical PhaseStatus vocabulary", status)
	}
}

func TestRun_PhaseEndSuccessIsCanonical(t *testing.T) {
	// A project with a config block takes the success branch, which must emit
	// a canonical "success" phase status (not the legacy "OK").
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.25\n",
		"server/server.go": `package server

import "go.putnami.dev/config"

type Server struct {
	Host string ` + "`json:\"host\"`" + `
	Port int    ` + "`json:\"port\"`" + `
}

var Cfg = config.Config[Server]("server")
`,
	})
	ctx := &pctx.Context{Project: pctx.Project{Name: "app", FullPath: dir}}

	status := capturePhaseStatus(t, ctx)
	if status != "success" {
		t.Fatalf("phase-end status = %q, want %q", status, "success")
	}
	if !canonicalPhaseStatuses[status] {
		t.Fatalf("phase-end status %q is not in the canonical PhaseStatus vocabulary", status)
	}
}
