package runtimecli

// env_doctor_checkout.go answers the rows of `putnami cloud env doctor` only
// the checkout can: the workspace's OCI registry, the CI environment, and,
// through the EnvDoctorPublish the CLI workload supplies, the publish
// namespaces. It also names the workloads the server rows check. Every read
// is a file read.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// envDoctorWorkloadPattern is the exact workload selector ordinary
// publish-v2 accepts, the same rule the readiness route enforces.
var envDoctorWorkloadPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,199}$`)

// envDoctorCIDocument is the part of putnami.ci.json the checks read.
type envDoctorCIDocument struct {
	Distribution struct {
		Namespace         string `json:"namespace"`
		MemberAttribution bool   `json:"memberAttribution"`
	} `json:"distribution"`
	Envs map[string]struct {
		Channel   string `json:"channel"`
		Workloads []struct {
			Select json.RawMessage `json:"select"`
		} `json:"workloads"`
	} `json:"envs"`
}

// envDoctorCheckout computes the checkout rows and the selected workloads,
// and returns the distribution namespace putnami.ci.json names ("" when it
// names none), which `env enable` activates the bindings under.
func envDoctorCheckout(workspaceRoot, environment string, publish EnvDoctorPublish) ([]EnvDoctorRow, []EnvDoctorWorkload, string) {
	ciRow, namespace, workloads := envDoctorCIEnvs(workspaceRoot, environment)
	rows := []EnvDoctorRow{envDoctorOCIRegistry(workspaceRoot, namespace), ciRow}
	switch {
	case len(workloads) == 0:
		rows = append(rows, EnvDoctorRow{
			ID: EnvDoctorRowPublishNamespaces, Title: EnvDoctorPublishNamespacesTitle, Status: EnvDoctorUnknown,
			Detail: "putnami.ci.json selects no workload for " + environment + ", so no manifest was checked",
			Fix:    "declare the environment's workloads in putnami.ci.json (see ci-envs), then run `putnami cloud env doctor` again",
		})
	case publish == nil:
		rows = append(rows, EnvDoctorRow{
			ID: EnvDoctorRowPublishNamespaces, Title: EnvDoctorPublishNamespacesTitle, Status: EnvDoctorUnknown,
			Detail: "this CLI cannot read the publish namespace options",
			Fix:    "run `putnami cloud env doctor` from the Putnami CLI",
		})
	default:
		var row EnvDoctorRow
		row, workloads = publish(workspaceRoot, namespace, workloads)
		rows = append(rows, row)
	}
	return rows, workloads, namespace
}

// envDoctorOCIRegistry checks registries.oci.publish with the reader deploy
// uses, so the two never disagree on where the images go.
func envDoctorOCIRegistry(workspaceRoot, namespace string) EnvDoctorRow {
	row := EnvDoctorRow{ID: EnvDoctorRowOCIRegistry, Title: "OCI publish registry"}
	registry := manifestOCIRegistry(filepath.Join(workspaceRoot, "putnami.workspace.json"))
	segments := 0
	for _, segment := range strings.Split(registry, "/") {
		if strings.TrimSpace(segment) != "" {
			segments++
		}
	}
	if segments >= 2 {
		row.Status, row.Detail = EnvDoctorOK, "images publish to "+registry
		return row
	}
	row.Status = EnvDoctorMissing
	row.Detail = "putnami.workspace.json names no registries.oci.publish with a host and a namespace"
	if registry != "" {
		row.Detail = "putnami.workspace.json registries.oci.publish " + registry + " names no namespace"
	}
	row.Fix = `set in putnami.workspace.json: "registries": {"oci": {"publish": "oci.putnami.dev/` + envDoctorNamespace(namespace) + `"}}` +
		"\nthen activate the matching OCI binding (see oci-binding)"
	return row
}

// envDoctorCIEnvs checks the environment's putnami.ci.json declaration and
// returns the distribution namespace and the workloads it selects.
func envDoctorCIEnvs(workspaceRoot, environment string) (EnvDoctorRow, string, []EnvDoctorWorkload) {
	row := EnvDoctorRow{ID: EnvDoctorRowCIEnvs, Title: "CI environment declaration"}
	fix := func(namespace string) string {
		return "add to putnami.ci.json:\n" +
			`"distribution": {"namespace": "` + envDoctorNamespace(namespace) + `", "memberAttribution": true},` + "\n" +
			`"envs": {"` + environment + `": {"channel": "canary", "workloads": [{"select": ["<workload path>"]}]}}`
	}
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.ci.json"))
	if err != nil {
		row.Status, row.Detail, row.Fix = EnvDoctorMissing, "the workspace has no readable putnami.ci.json", fix("")
		if !errors.Is(err, os.ErrNotExist) {
			row.Detail = "putnami.ci.json could not be read: " + err.Error()
		}
		return row, "", nil
	}
	var document envDoctorCIDocument
	if err := json.Unmarshal(data, &document); err != nil {
		row.Status, row.Detail, row.Fix = EnvDoctorMissing, "putnami.ci.json does not parse: "+err.Error(), fix("")
		return row, "", nil
	}
	namespace := strings.TrimSpace(document.Distribution.Namespace)
	declared, found := document.Envs[environment]
	if !found {
		row.Status, row.Detail, row.Fix = EnvDoctorMissing, "putnami.ci.json declares no envs."+environment, fix(namespace)
		return row, namespace, nil
	}
	var problems []string
	var workloads []EnvDoctorWorkload
	seen := map[string]bool{}
	for _, rule := range declared.Workloads {
		selectors, err := envDoctorSelectors(rule.Select)
		if err != nil {
			problems = append(problems, "a workloads[].select is neither a string nor a list of strings")
			continue
		}
		for _, selector := range selectors {
			switch {
			case !envDoctorWorkloadPattern.MatchString(selector):
				problems = append(problems, fmt.Sprintf("%q is not an exact workload path (ordinary publish-v2 accepts only exact paths)", selector))
			case seen[selector]:
			default:
				seen[selector] = true
				name, err := clicore.ProjectNameAt(filepath.Join(workspaceRoot, filepath.FromSlash(selector)))
				if err != nil {
					problems = append(problems, selector+" has no readable putnami.json in this checkout")
					continue
				}
				workloads = append(workloads, EnvDoctorWorkload{Path: selector, Name: name})
			}
		}
	}
	if !document.Distribution.MemberAttribution {
		problems = append(problems, "distribution.memberAttribution is not true")
	}
	if len(workloads) == 0 {
		problems = append(problems, "envs."+environment+" selects no workload")
	}
	if len(problems) != 0 {
		row.Status, row.Detail, row.Fix = EnvDoctorMissing, strings.Join(problems, "; "), fix(namespace)
		return row, namespace, workloads
	}
	channel := declared.Channel
	if channel == "" {
		channel = "no channel"
	}
	row.Status = EnvDoctorOK
	row.Detail = fmt.Sprintf("%s follows %s and selects %d workload(s); member attribution is on", environment, channel, len(workloads))
	return row, namespace, workloads
}

// envDoctorSelectors reads a select value: one selector or a list of them.
func envDoctorSelectors(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func envDoctorNamespace(namespace string) string {
	if namespace == "" {
		return "<ns>"
	}
	return namespace
}

// manifestOCIRegistry reads registries.oci.publish from a Putnami manifest,
// or "" when the file or the member is absent.
func manifestOCIRegistry(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var manifest struct {
		Registries struct {
			OCI struct {
				Publish string `json:"publish"`
			} `json:"oci"`
		} `json:"registries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Registries.OCI.Publish)
}
