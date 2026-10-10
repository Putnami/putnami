package runtimecli

import (
	"fmt"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// deployStatusUsage is the usage error of `putnami cloud deploy status`
// without a release id: it names where the ids are printed.
const deployStatusUsage = "usage: putnami cloud deploy status <release-id>; `putnami cloud env status` prints the release " +
	"each channel move opened (cm_…), and `putnami cloud deploy publish-v2` prints the release it submits (rel_…)"

// deployStatusReleaseID reads the one release id `deploy status` takes. A
// release set id (rs_…) is refused: it names the artifacts a channel move
// promotes, not a run, and the deploy route would answer 404.
func deployStatusReleaseID(args []string) (string, error) {
	positionals := clicore.Positionals(args)
	if len(positionals) == 0 || strings.TrimSpace(positionals[0]) == "" {
		return "", clicore.NewError(deployStatusUsage, clicore.ExitUsage)
	}
	if len(positionals) > 1 {
		return "", clicore.NewError("cloud deploy status takes one release id; got "+strings.Join(positionals, " "), clicore.ExitUsage)
	}
	id := strings.TrimSpace(positionals[0])
	if strings.HasPrefix(id, "rs_") {
		return "", clicore.NewError(id+" is a release set: the artifacts a channel move promotes, not a release. "+
			"Pass the release the move opened (cm_…), which `putnami cloud env status` prints, "+
			"or the release `putnami cloud deploy publish-v2` printed (rel_…)", clicore.ExitUsage)
	}
	return id, nil
}

// DeployStatusNodeFrom folds one release into its status node: the
// release is the node, each workload a child. A Ready release is ok; one
// still converging is degraded; one that failed or ended Partial is failing.
// A held workload makes a Ready release degraded.
func DeployStatusNodeFrom(releaseID string, resp *deployStatusResponse) clicore.StatusNode {
	node := clicore.StatusNode{ID: "deploy", Title: "release " + releaseID}
	if resp == nil {
		node.State = clicore.StatusUnknown
		node.Detail = "the control plane answered no release"
		return node
	}
	release := resp.releaseResponse
	state := strings.TrimSpace(release.State)
	stateNode := deployReleaseState(state)
	for _, project := range release.Projects {
		node.Children = append(node.Children, deployWorkloadNode(project))
	}
	node.State = clicore.WorstStatus(append(node.ChildStates(), stateNode)...)

	parts := []string{statusHealth(strings.ToLower(state))}
	if release.Environment != "" {
		parts = append(parts, "environment "+release.Environment)
	}
	counts := releaseCountsForOutput(state, release.Projects, release.RequestedWorkloadCount, release.ServingNewRevisionCount,
		release.ReusedWorkloadCount, release.HeldWorkloadCount, release.TerminalClassification)
	serving := counts.servingNew + counts.reused
	if counts.requested > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d workloads serving", serving, counts.requested))
	}
	node.Detail = strings.Join(parts, ", ")
	if stateNode == clicore.StatusDegraded {
		node.Fix = "putnami cloud deploy status " + releaseID
	}

	node.Metrics = []clicore.StatusMetric{
		clicore.CountMetric("workloads_requested", "workloads requested", counts.requested, clicore.MetricHealth),
		clicore.CountMetric("workloads_serving", "workloads serving", serving, clicore.MetricHealth).Of(float64(counts.requested)),
		clicore.CountMetric("workloads_held", "workloads held", counts.held, clicore.MetricHealth),
	}
	if release.Timings != nil && release.Timings.TotalWallClockMS != nil {
		node.Metrics = append(node.Metrics, clicore.StatusMetric{
			ID: "total_wall_clock", Title: "total wall clock", Value: float64(*release.Timings.TotalWallClockMS) / 1000,
			Unit: clicore.UnitSeconds, Kind: clicore.MetricHealth,
		})
	}
	return node
}

// deployReleaseState maps the release state onto the shared scale.
func deployReleaseState(state string) clicore.StatusState {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ready":
		return clicore.StatusOK
	case "partial", "failed", "skipped":
		return clicore.StatusFailing
	case "provisioning", "pending", "queued", "running":
		return clicore.StatusDegraded
	default:
		return clicore.StatusUnknown
	}
}

// deployWorkloadNode is one workload of the release: its state, the revision
// that serves it and its traffic, and why it is held when it is.
func deployWorkloadNode(project releaseProject) clicore.StatusNode {
	node := clicore.StatusNode{ID: "deploy." + project.Name, Title: project.Name}
	status := strings.TrimSpace(project.Status)
	node.State = deployReleaseState(status)
	parts := []string{statusHealth(strings.ToLower(status))}
	if revision := clicore.FirstString(project.ServingRevision, project.Revision); revision != "" {
		parts = append(parts, "revision "+revision)
	}
	if project.TrafficPercent != nil {
		parts = append(parts, fmt.Sprintf("traffic %d%%", *project.TrafficPercent))
	}
	if project.Action == "skip" {
		parts = append(parts, "unchanged")
	}
	if project.HoldReason != "" {
		parts = append(parts, "held: "+statusShortReason(project.HoldReason))
		if node.State == clicore.StatusOK {
			node.State = clicore.StatusDegraded
		}
	} else if node.State != clicore.StatusOK && project.Detail != "" {
		parts = append(parts, statusShortReason(project.Detail))
	}
	node.Detail = strings.Join(parts, ", ")
	return node
}
