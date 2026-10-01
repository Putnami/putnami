package composecmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/compose"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestResolvePort(t *testing.T) {
	target := &workspace.Project{Config: &wsproto.ProjectConfig{Options: map[string]map[string]any{"serve": {"port": float64(3911)}}}}
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: 3911},
		{raw: "0", want: 0},
		{raw: " 4100 ", want: 4100},
		{raw: "-1", wantErr: true},
		{raw: "70000", wantErr: true},
		{raw: "http", wantErr: true},
	}
	for _, tc := range cases {
		got, err := resolvePort(tc.raw, target)
		if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.want) {
			t.Errorf("resolvePort(%q) = %d, %v", tc.raw, got, err)
		}
	}
}

func TestResolveReadyTimeout(t *testing.T) {
	if got, err := resolveReadyTimeout(""); err != nil || got != compose.DefaultReadyTimeout {
		t.Errorf("default = %s, %v", got, err)
	}
	if got, err := resolveReadyTimeout("2m"); err != nil || got != 2*time.Minute {
		t.Errorf("2m = %s, %v", got, err)
	}
	for _, raw := range []string{"0s", "-1s", "soon"} {
		if _, err := resolveReadyTimeout(raw); err == nil {
			t.Errorf("resolveReadyTimeout(%q) accepted", raw)
		}
	}
}

func TestResolveTarget_ByIDThenName(t *testing.T) {
	api := &workspace.Project{ID: "/apps/api", Name: "@acme/api", Path: "apps/api"}
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{}, []*workspace.Project{api})
	for _, selector := range []string{"/apps/api", "@acme/api", " @acme/api "} {
		if got, err := resolveTarget(ws, selector); err != nil || got != api {
			t.Errorf("resolveTarget(%q) = %v, %v", selector, got, err)
		}
	}
	for _, selector := range []string{"", "apps/api", "@acme/web"} {
		if _, err := resolveTarget(ws, selector); err == nil {
			t.Errorf("resolveTarget(%q) accepted", selector)
		}
	}
}

func TestWriteStatus_StructuredAndHuman(t *testing.T) {
	status := compose.Status{
		ID:     "0123456789abcdef",
		Target: "/apps/web",
		Members: []compose.MemberStatus{{
			Project: "/apps/web", ProxyURL: "http://127.0.0.1:3000", BackendPort: 51234,
			Databases: []string{"default"}, ConfigSections: []string{"clients", "database"}, ReadyMs: 412,
		}},
		Isolation: compose.IsolationDatabase,
	}
	var structured bytes.Buffer
	if err := writeStatus(&structured, "jsonl", status); err != nil {
		t.Fatalf("writeStatus jsonl: %v", err)
	}
	var envelope struct {
		Command string         `json:"command"`
		Status  string         `json:"status"`
		Data    compose.Status `json:"data"`
	}
	if err := json.Unmarshal(structured.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, structured.String())
	}
	if envelope.Command != CommandPath || envelope.Status != "success" || envelope.Data.Members[0].ProxyURL != "http://127.0.0.1:3000" {
		t.Errorf("envelope = %+v", envelope)
	}
	if strings.Count(structured.String(), "\n") != 1 {
		t.Errorf("a jsonl document spans more than one line: %q", structured.String())
	}

	var human bytes.Buffer
	if err := writeStatus(&human, "", status); err != nil {
		t.Fatalf("writeStatus text: %v", err)
	}
	if !strings.Contains(human.String(), "/apps/web  http://127.0.0.1:3000  ready in 412ms  (clients, database)") {
		t.Errorf("human start output = %q", human.String())
	}
	status.Cleanup = &compose.CleanupReport{State: compose.CleanupPartial, Leftovers: []string{"database compose_x"}}
	human.Reset()
	if err := writeStatus(&human, "", status); err != nil {
		t.Fatalf("writeStatus text exit: %v", err)
	}
	if !strings.Contains(human.String(), "cleanup partial") || !strings.Contains(human.String(), "left behind: database compose_x") {
		t.Errorf("human exit output = %q", human.String())
	}
}

func TestFailure_NamesTheCompositionAndItsCleanupButNoMemberOutput(t *testing.T) {
	composeErr := &compose.Error{
		Code: compose.CodeReadyTimeout, ID: "0123456789abcdef", Member: "/apps/web", Phase: compose.PhaseReadiness,
		Message: "no typed ready event within 1s",
		Detail:  []string{"connect postgres://web:s3cret@127.0.0.1/web: refused"},
		Cleanup: &compose.CleanupReport{State: compose.CleanupPartial, Leftovers: []string{"database compose_0123456789abcdef_web_default"}},
	}
	err := failure(composeErr)
	data, ok := shared.ResultData(err).(failureData)
	if !ok {
		t.Fatalf("failure data = %#v", shared.ResultData(err))
	}
	if data.ID != composeErr.ID || data.Cleanup == nil || data.Cleanup.State != compose.CleanupPartial || len(data.Cleanup.Leftovers) != 1 {
		t.Errorf("failure data = %+v, want the composition id and its teardown report", data)
	}
	encoded, _ := json.Marshal(data)
	for _, document := range []string{string(encoded), err.Error()} {
		if strings.Contains(document, "s3cret") {
			t.Errorf("a failure document carries member output: %s", document)
		}
	}

	var stderr bytes.Buffer
	writeDetail(&stderr, err, true)
	if stderr.Len() != 0 {
		t.Errorf("structured output printed member output: %q", stderr.String())
	}
	writeDetail(&stderr, err, false)
	if !strings.Contains(stderr.String(), "last output of /apps/web:\n  | connect postgres://web:s3cret@127.0.0.1/web: refused") {
		t.Errorf("text output stderr = %q, want the member output", stderr.String())
	}
}
