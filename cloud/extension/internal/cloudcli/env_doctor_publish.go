package cloudcli

import (
	"fmt"
	"path/filepath"
	"strings"

	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	runtimecli "go.putnami.dev/cloud/extension/internal/runtimecli"
)

// envDoctorPublish answers the publish-namespaces row of
// `putnami cloud env doctor`. It lives here, beside the workspace
// probe, because it must apply the probe's own rules: a workload publishes a
// Config member when it has a Config schema, and a migration member when it
// declares a migration namespace. A workload that has either but declares no
// namespace publishes nothing, and its first deploy fails later.
//
// A workload counts as migrated when it declares a migration namespace or its
// last build produced a migration bundle. The second case is the one to catch:
// a bundle with no namespace never reaches Put. Before any build, a workload
// that declares nothing looks like one without migrations; the row says so.
func envDoctorPublish(root, namespace string, workloads []runtimecli.EnvDoctorWorkload) (runtimecli.EnvDoctorRow, []runtimecli.EnvDoctorWorkload) {
	row := runtimecli.EnvDoctorRow{ID: runtimecli.EnvDoctorRowPublishNamespaces, Title: runtimecli.EnvDoctorPublishNamespacesTitle}
	if namespace == "" {
		namespace = "<ns>"
	}
	out := make([]runtimecli.EnvDoctorWorkload, 0, len(workloads))
	var problems, fixes []string
	configs, migrations := 0, 0
	for _, workload := range workloads {
		manifest := workspaceFile(workload.Path, cloudProjectMarker)
		project, found, err := readCloudProject(filepath.Join(root, filepath.FromSlash(manifest)))
		if err != nil || !found {
			problems = append(problems, manifest+" could not be read")
			fixes = append(fixes, "restore "+manifest)
			out = append(out, workload)
			continue
		}
		var layers []string
		if len(existingConfigMemberFiles(root, workload.Path)) != 0 {
			_, declared, err := project.configNamespace()
			switch {
			case err != nil:
				problems = append(problems, workload.Path+": "+err.Error())
				layers = append(layers, "@putnami/cloud:publish-config")
			case !declared:
				problems = append(problems, workload.Path+" has a Config schema but declares no publish-config namespace")
				layers = append(layers, "@putnami/cloud:publish-config")
			default:
				configs++
			}
		}
		_, declared, err := project.migrationNamespace()
		built := datacli.MigrationBundleBuilt(root, workload.Path)
		switch {
		case err != nil:
			problems = append(problems, workload.Path+": "+err.Error())
			layers = append(layers, "@putnami/cloud:publish-migration")
		case declared:
			migrations++
		case built:
			problems = append(problems, workload.Path+" builds a migration bundle but declares no publish-migration namespace")
			layers = append(layers, "@putnami/cloud:publish-migration")
		}
		workload.Migrated = declared || built
		out = append(out, workload)
		if len(layers) != 0 {
			fixes = append(fixes, envDoctorOptionsSnippet(manifest, namespace, layers))
		}
	}
	if len(problems) != 0 {
		row.Status = runtimecli.EnvDoctorMissing
		row.Detail = strings.Join(problems, "; ")
		row.Fix = strings.Join(fixes, "\n")
		return row, out
	}
	row.Status = runtimecli.EnvDoctorOK
	row.Detail = fmt.Sprintf("%d workload(s) checked: %d publish Config, %d publish migrations; migrations are seen through a declared namespace or a built bundle",
		len(workloads), configs, migrations)
	return row, out
}

// envDoctorOptionsSnippet is the manifest change that declares the missing
// namespaces, in the layer each publish step reads last.
func envDoctorOptionsSnippet(manifest, namespace string, layers []string) string {
	entries := make([]string, 0, len(layers))
	for _, layer := range layers {
		entries = append(entries, fmt.Sprintf(`%q: {"namespace": %q}`, layer, namespace))
	}
	return "in " + manifest + ` set "options": {` + strings.Join(entries, ", ") + "}"
}
