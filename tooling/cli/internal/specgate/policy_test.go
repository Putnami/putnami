package specgate

import (
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

func gateWorkspace(workspaceVerification any, projectVerification any) *workspace.Workspace {
	ws := &workspace.Workspace{Root: "/ws", Config: &wsproto.Config{}}
	if workspaceVerification != nil {
		ws.Config.Options = map[string]map[string]any{"sdd": {"verification": workspaceVerification}}
	}
	project := &workspace.Project{ID: "/go/framework/logger", Name: "go.putnami.dev/logger", Config: &wsproto.ProjectConfig{}}
	if projectVerification != nil {
		project.Config.Options = map[string]map[string]any{"sdd": {"verification": projectVerification}}
	}
	ws.Projects = []*workspace.Project{project}
	return ws
}

func TestValidatePolicyAcceptsAbsentAndValidBlocks(t *testing.T) {
	for name, ws := range map[string]*workspace.Workspace{
		"nil workspace": nil,
		"no options":    gateWorkspace(nil, nil),
		"valid blocks":  gateWorkspace(map[string]any{"specs": "report"}, map[string]any{"specs": "enforce", "features": "off"}),
	} {
		if err := ValidatePolicy(ws); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestValidatePolicyFailsClosedBeforeJobsExecute pins the pre-execution
// contract: an unreadable committed policy is an invalid-config error carrying
// the usage exit code, wherever it is committed, and the error names the
// owning configuration.
func TestValidatePolicyFailsClosedBeforeJobsExecute(t *testing.T) {
	cases := map[string]*workspace.Workspace{
		"workspace unknown value":  gateWorkspace(map[string]any{"specs": "on"}, nil),
		"workspace unknown domain": gateWorkspace(map[string]any{"spec": "report"}, nil),
		"workspace wrong type":     gateWorkspace("enforce", nil),
		"project unknown value":    gateWorkspace(nil, map[string]any{"specs": "info"}),
		"project non-string":       gateWorkspace(nil, map[string]any{"specs": 1}),
	}
	for name, ws := range cases {
		err := ValidatePolicy(ws)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("%s: error %v is not classified invalid-config", name, err)
		}
		if protocolcli.ExitCodeForError(err) != protocolcli.ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, protocolcli.ExitCodeForError(err), protocolcli.ExitUsage)
		}
	}
	if err := ValidatePolicy(gateWorkspace(nil, map[string]any{"specs": true})); err == nil ||
		!strings.Contains(err.Error(), "/go/framework/logger") {
		t.Errorf("project policy error %v does not name the owning project", err)
	}
}

func TestEffectiveModeResolvesWithProvenance(t *testing.T) {
	ws := gateWorkspace(map[string]any{"specs": "off"}, map[string]any{"specs": "enforce"})
	project := ws.Projects[0]

	mode, source, err := EffectiveMode(features.VerificationDomainSpecs, ws, project)
	if err != nil || mode != features.VerificationModeEnforce || source != features.VerificationModeSourceProject {
		t.Errorf("project override resolved (%s, %s, %v)", mode, source, err)
	}
	mode, source, err = EffectiveMode(features.VerificationDomainSpecs, ws, nil)
	if err != nil || mode != features.VerificationModeOff || source != features.VerificationModeSourceWorkspace {
		t.Errorf("workspace policy resolved (%s, %s, %v)", mode, source, err)
	}
	mode, source, err = EffectiveMode(features.VerificationDomainSpecs, gateWorkspace(nil, nil), nil)
	if err != nil || mode != features.VerificationModeReport || source != features.VerificationModeSourceDefault {
		t.Errorf("default resolved (%s, %s, %v)", mode, source, err)
	}
	if _, _, err := EffectiveMode(features.VerificationDomainSpecs, gateWorkspace("broken", nil), nil); err == nil ||
		!errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Errorf("unreadable policy resolved: %v", err)
	}
}
