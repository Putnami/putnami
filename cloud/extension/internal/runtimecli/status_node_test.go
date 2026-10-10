package runtimecli

import (
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestEnvDoctorStatusNodeFromFoldsTheDoctorTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result EnvDoctorResult
		state  clicore.StatusState
		detail string
	}{
		{
			name:   "ready",
			result: EnvDoctorResult{Environment: "prod", Ready: true, Rows: []EnvDoctorRow{{ID: "put-binding", Status: "ok"}}},
			state:  clicore.StatusOK,
			detail: "1 of 1 prerequisites ok",
		},
		{
			name: "an unchecked row",
			result: EnvDoctorResult{Environment: "prod", Ready: true, Unchecked: 1, Rows: []EnvDoctorRow{
				{ID: "put-binding", Status: "ok"}, {ID: "oci-registry", Status: "unchecked"},
			}},
			state:  clicore.StatusUnknown,
			detail: "1 of 2 prerequisites ok, 1 could not be checked",
		},
		{
			name: "missing prerequisite",
			result: EnvDoctorResult{Environment: "dev", Missing: 1, Rows: []EnvDoctorRow{
				{ID: "put-binding", Status: "ok"}, {ID: "oci-registry", Status: "missing", Fix: "set registries.oci.publish"},
			}},
			state:  clicore.StatusFailing,
			detail: "1 of 2 prerequisites ok, 1 missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := EnvDoctorStatusNodeFrom(tc.result)
			if node.ID != "env.doctor" || node.Title != "env doctor "+tc.result.Environment ||
				node.State != tc.state || node.Detail != tc.detail || node.Fix != "putnami cloud env doctor "+tc.result.Environment {
				t.Fatalf("node = %+v, want %s %q", node, tc.state, tc.detail)
			}
			if len(node.Children) != len(tc.result.Rows) {
				t.Fatalf("children = %d, want one per row", len(node.Children))
			}
			if len(node.Metrics) != 3 || node.Metrics[1].ID != "prerequisites_missing" || int(node.Metrics[1].Value) != tc.result.Missing {
				t.Fatalf("metrics = %+v", node.Metrics)
			}
		})
	}
}

func TestEnvDoctorRowNodeKeepsTheFixOnOneLine(t *testing.T) {
	node := EnvDoctorRowNode(EnvDoctorRow{ID: "oci-registry", Title: "oci registry", Status: "missing", Detail: "no publish target", Fix: "set it\nthen activate it"})
	if node.ID != "env.doctor.oci-registry" || node.State != clicore.StatusFailing || node.Fix != "set it then activate it" || node.Title != "oci-registry" {
		t.Fatalf("node = %+v", node)
	}
}
