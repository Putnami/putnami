package runtimecli

import (
	"fmt"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// EnvDoctorStatusNodeFrom folds an env doctor table into the node
// `putnami cloud env doctor` prints: one child per prerequisite. A
// missing prerequisite fails the environment; a row that could not be
// checked leaves it unknown, which fails the command only under --strict.
func EnvDoctorStatusNodeFrom(result EnvDoctorResult) clicore.StatusNode {
	node := clicore.StatusNode{
		ID:    "env.doctor",
		Title: "env doctor " + result.Environment,
		Fix:   "putnami cloud env doctor " + result.Environment,
	}
	ok := 0
	for _, row := range result.Rows {
		child := EnvDoctorRowNode(row)
		node.Children = append(node.Children, child)
		if child.State == clicore.StatusOK {
			ok++
		}
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail = fmt.Sprintf("%d of %d prerequisites ok", ok, len(result.Rows))
	if result.Missing > 0 {
		node.Detail += fmt.Sprintf(", %d missing", result.Missing)
	}
	if result.Unchecked > 0 {
		node.Detail += fmt.Sprintf(", %d could not be checked", result.Unchecked)
	}
	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("prerequisites_ok", "prerequisites ok", ok, clicore.MetricHealth).Of(float64(len(result.Rows))),
		clicore.CountMetric("prerequisites_missing", "prerequisites missing", result.Missing, clicore.MetricHealth),
		clicore.CountMetric("prerequisites_unchecked", "prerequisites unchecked", result.Unchecked, clicore.MetricHealth),
	}
	return node
}

// EnvDoctorRowNode maps one prerequisite onto the shared scale: ok is ok,
// missing is failing, anything else could not be checked and is unknown.
// The title is the row id, the name the fixes refer to ("see oci-binding");
// a multi-line fix reads as one line.
func EnvDoctorRowNode(row EnvDoctorRow) clicore.StatusNode {
	node := clicore.StatusNode{
		ID: "env.doctor." + row.ID, Title: row.ID,
		Detail: statusOneLine(row.Detail), Fix: statusOneLine(row.Fix),
	}
	switch row.Status {
	case EnvDoctorOK:
		node.State = clicore.StatusOK
	case EnvDoctorMissing:
		node.State = clicore.StatusFailing
	default:
		node.State = clicore.StatusUnknown
	}
	return node
}
