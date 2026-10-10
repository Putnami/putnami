package cloudcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/put"
	runtimeproto "go.putnami.dev/protocol/runtime"
	workspaceproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/releaseset"
)

const (
	deploymentFixturePath       = "shop/workloads/api"
	deploymentFixtureProjectID  = "/" + deploymentFixturePath
	deploymentFixtureCoordinate = "cloud/shop-workloads-api-deployment"
)

// writeDeploymentProbeProject writes a project whose Config member the probe
// declares (a schema and the explicit "cloud" namespace), with the given
// project manifest members merged over that base.
func writeDeploymentProbeProject(t *testing.T, root string, project map[string]any) {
	t.Helper()
	projectRoot := filepath.Join(root, filepath.FromSlash(deploymentFixturePath))
	if err := os.MkdirAll(filepath.Join(projectRoot, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "schema", "config.json"), []byte(`{"appName":"shop/workloads/api","configs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"name": deploymentFixturePath}
	for key, value := range project {
		manifest[key] = value
	}
	writeJSONFile(t, filepath.Join(projectRoot, cloudProjectMarker), manifest)
}

func deploymentProbeOptions(extra map[string]any) map[string]any {
	options := map[string]any{
		"@putnami/cloud:publish-config": map[string]any{"namespace": "cloud"},
		"@putnami/cloud":                map[string]any{"deploy": map[string]any{"enabled": true}},
	}
	for key, value := range extra {
		options[key] = value
	}
	return options
}

// probeDeploymentMember runs the probe over the fixture project and returns
// the deployment member's routes, if the probe declared one.
func probeDeploymentMember(t *testing.T, project map[string]any) (publisher, packageStep, publishStep string, declared bool) {
	t.Helper()
	root := t.TempDir()
	writeDeploymentProbeProject(t, root, project)
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{deploymentFixturePath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %+v", result.Projects)
	}
	metadata, found, err := releaseset.ProjectReleaseMetadata(map[string]json.RawMessage{cloudRuntimeIdentity: result.Projects[0].Metadata})
	if err != nil || !found {
		t.Fatalf("metadata = %+v, %v, %v", metadata, found, err)
	}
	publisher, declared = metadata.PackagePublisherFor(cloudDeploymentEcosystem, deploymentFixtureCoordinate)
	if !declared {
		for _, declaration := range metadata.Ecosystems {
			if strings.HasSuffix(declaration.Coordinate, cloudDeploymentPackageSuffix) || declaration.PackageStep == cloudDeploymentPackageStep {
				t.Fatalf("probe declared an unexpected deployment member %+v", declaration)
			}
		}
		return "", "", "", false
	}
	packageStep, _ = metadata.PackageStepFor(cloudDeploymentEcosystem, deploymentFixtureCoordinate)
	publishStep, _ = metadata.PublishStepFor(cloudDeploymentEcosystem, deploymentFixtureCoordinate)
	return publisher, packageStep, publishStep, true
}

func TestCloudWorkspaceProbeDeclaresTheDeploymentMemberWhenTheLanguageStepIsOn(t *testing.T) {
	for name, test := range map[string]struct {
		project   map[string]any
		publisher string
	}{
		"go extension option": {
			project:   map[string]any{"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}})},
			publisher: "@putnami/go",
		},
		"typescript extension option": {
			project:   map[string]any{"extensions": []string{"@putnami/typescript"}, "options": deploymentProbeOptions(map[string]any{"@putnami/typescript": map[string]any{"deployment": true}})},
			publisher: "@putnami/typescript",
		},
		"deployment publish channel": {
			project:   map[string]any{"type": "application", "extensions": []string{"@putnami/go"}, "publish": []string{"docker", "deployment"}, "options": deploymentProbeOptions(nil)},
			publisher: "@putnami/go",
		},
		"package command option": {
			project:   map[string]any{"extensions": []string{"@putnami/typescript"}, "options": deploymentProbeOptions(map[string]any{"package": map[string]any{"deployment": true}})},
			publisher: "@putnami/typescript",
		},
		"extension package option": {
			project:   map[string]any{"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/go:package": map[string]any{"deployment": true}})},
			publisher: "@putnami/go",
		},
		"non-language extension beside go": {
			project:   map[string]any{"extensions": []string{"@putnami/cloud", "@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}})},
			publisher: "@putnami/go",
		},
	} {
		t.Run(name, func(t *testing.T) {
			publisher, packageStep, publishStep, declared := probeDeploymentMember(t, test.project)
			if !declared {
				t.Fatal("the probe did not declare the deployment member")
			}
			if publisher != test.publisher || packageStep != "deployment" || publishStep != "cloud-publish-deployment" {
				t.Fatalf("routes = %q %q %q, want %q deployment cloud-publish-deployment", publisher, packageStep, publishStep, test.publisher)
			}
		})
	}
}

func TestCloudWorkspaceProbeDeclaresNoDeploymentMemberUnlessAllConditionsHold(t *testing.T) {
	goOn := map[string]any{"@putnami/go": map[string]any{"deployment": true}}
	for name, project := range map[string]map[string]any{
		"deploy not enabled": {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{
			"@putnami/cloud": map[string]any{"deploy": map[string]any{"enabled": false}}, "@putnami/go": map[string]any{"deployment": true},
		})},
		"no deploy block": {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{
			"@putnami/cloud": map[string]any{}, "@putnami/go": map[string]any{"deployment": true},
		})},
		"deploy enabled is not a boolean": {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{
			"@putnami/cloud": map[string]any{"deploy": map[string]any{"enabled": "true"}}, "@putnami/go": map[string]any{"deployment": true},
		})},
		"library":             {"type": "library", "extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(goOn)},
		"step off":            {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(nil)},
		"step turned off":     {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}, "@putnami/go:package": map[string]any{"deployment": false}})},
		"image option":        {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true, "image": true}})},
		"image channel":       {"extensions": []string{"@putnami/go"}, "publish": []string{"image"}, "options": deploymentProbeOptions(goOn)},
		"other language":      {"extensions": []string{"@putnami/go"}, "options": deploymentProbeOptions(map[string]any{"@putnami/typescript": map[string]any{"deployment": true}})},
		"no language":         {"options": deploymentProbeOptions(map[string]any{"package": map[string]any{"deployment": true}})},
		"no Config namespace": {"extensions": []string{"@putnami/go"}, "options": map[string]any{"@putnami/cloud": map[string]any{"deploy": map[string]any{"enabled": true}}, "@putnami/go": map[string]any{"deployment": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeDeploymentProbeProject(t, root, project)
			result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{deploymentFixturePath}})
			if err != nil {
				t.Fatal(err)
			}
			for _, announced := range result.Projects {
				if bytes.Contains(announced.Metadata, []byte(cloudDeploymentPublishStep)) {
					t.Fatalf("probe declared a deployment member: %s", announced.Metadata)
				}
			}
		})
	}
}

func TestCloudWorkspaceProbeDeclaresNoDeploymentMemberWithoutAConfigMember(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, filepath.FromSlash(deploymentFixturePath))
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(projectRoot, cloudProjectMarker), map[string]any{
		"name": deploymentFixturePath, "extensions": []string{"@putnami/go"},
		"options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}}),
	})
	result, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{deploymentFixturePath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 0 {
		t.Fatalf("a project without a Config schema announced members: %+v", result.Projects)
	}
}

func TestCloudWorkspaceProbeRefusesAnAmbiguousDeploymentPublisher(t *testing.T) {
	root := t.TempDir()
	writeDeploymentProbeProject(t, root, map[string]any{
		"extensions": []string{"@putnami/go", "@putnami/typescript"},
		"options":    deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}}),
	})
	_, err := probeCloudWorkspace(root, workspaceproto.ProbeRequest{Paths: []string{deploymentFixturePath}})
	if err == nil || !strings.Contains(err.Error(), "exactly one deployment language publisher") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeploymentStepActiveMirrorsTheFrameworkStepCondition(t *testing.T) {
	for name, test := range map[string]struct {
		options map[string]map[string]any
		publish []string
		want    bool
	}{
		"absent":                      {want: false},
		"true":                        {options: map[string]map[string]any{"@putnami/go": {"deployment": true}}, want: true},
		"string false":                {options: map[string]map[string]any{"@putnami/go": {"deployment": "false"}}, want: false},
		"empty string":                {options: map[string]map[string]any{"@putnami/go": {"deployment": ""}}, want: false},
		"non-empty string":            {options: map[string]map[string]any{"@putnami/go": {"deployment": "yes"}}, want: true},
		"zero":                        {options: map[string]map[string]any{"@putnami/go": {"deployment": float64(0)}}, want: false},
		"number":                      {options: map[string]map[string]any{"@putnami/go": {"deployment": float64(1)}}, want: true},
		"object":                      {options: map[string]map[string]any{"@putnami/go": {"deployment": map[string]any{}}}, want: true},
		"null replaces a lower layer": {options: map[string]map[string]any{"package": {"deployment": true}, "@putnami/go": {"deployment": nil}}, want: false},
		"higher layer turns it on":    {options: map[string]map[string]any{"package": {"deployment": false}, "@putnami/go:package": {"deployment": true}}, want: true},
		"channel overrides an option": {options: map[string]map[string]any{"@putnami/go:package": {"deployment": false}}, publish: []string{"deployment"}, want: true},
		"image channel wins":          {options: map[string]map[string]any{"@putnami/go": {"deployment": true}}, publish: []string{"image"}, want: false},
		"image false keeps it on":     {options: map[string]map[string]any{"@putnami/go": {"deployment": true, "image": false}}, want: true},
		"other extension is not read": {options: map[string]map[string]any{"@putnami/typescript": {"deployment": true}}, want: false},
		"other command is not read":   {options: map[string]map[string]any{"publish": {"deployment": true}, "@putnami/go:publish": {"deployment": true}}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			project := cloudProjectProbe{Options: test.options, Publish: test.publish}
			if got := project.deploymentStepActive("@putnami/go"); got != test.want {
				t.Fatalf("deploymentStepActive = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDeploymentMemberCoordinateFollowsTheConfigNamingRule(t *testing.T) {
	if coordinate, err := deploymentMemberCoordinate("cloud", "shop/workloads/api"); err != nil || coordinate != deploymentFixtureCoordinate {
		t.Fatalf("coordinate = %q, %v", coordinate, err)
	}
	if coordinate, err := deploymentMemberCoordinate("workspace-native", "api"); err != nil || coordinate != "workspace-native/api-deployment" {
		t.Fatalf("coordinate = %q, %v", coordinate, err)
	}
	for name, input := range map[string][2]string{
		"namespace case":       {"Cloud", "api"},
		"namespace path":       {"cloud/x", "api"},
		"empty project":        {"cloud", ""},
		"padded project":       {"cloud", " api"},
		"leading slash":        {"cloud", "/api"},
		"trailing slash":       {"cloud", "api/"},
		"empty segment":        {"cloud", "shop//api"},
		"parent segment":       {"cloud", "shop/../api"},
		"project case":         {"cloud", "Shop/api"},
		"package beyond bound": {"cloud", strings.Repeat("a", 245)},
	} {
		t.Run(name, func(t *testing.T) {
			if coordinate, err := deploymentMemberCoordinate(input[0], input[1]); err == nil {
				t.Fatalf("coordinate = %q, want an error", coordinate)
			}
		})
	}
}

func TestDeploymentMemberMediaTypeIsTheFrameworkDeploymentProfile(t *testing.T) {
	if distributioncli.DeploymentMemberMediaType != put.DeploymentManifestMediaType {
		t.Fatalf("Distribution publishes %q, the framework profile is %q", distributioncli.DeploymentMemberMediaType, put.DeploymentManifestMediaType)
	}
}

// deploymentPublishFixture writes a workload with a declared Config namespace
// and the given .gen/deployment.json; nil writes no declaration.
func deploymentPublishFixture(t *testing.T, declaration []byte) string {
	t.Helper()
	root := t.TempDir()
	projectRoot := filepath.Join(root, filepath.FromSlash(deploymentFixturePath))
	if err := os.MkdirAll(filepath.Join(projectRoot, infra.AggregatedManifestDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(projectRoot, cloudProjectMarker), map[string]any{
		"name": deploymentFixturePath, "extensions": []string{"@putnami/go"},
		"options": deploymentProbeOptions(map[string]any{"@putnami/go": map[string]any{"deployment": true}}),
	})
	if declaration != nil {
		if err := os.WriteFile(filepath.Join(projectRoot, infra.AggregatedManifestDir, infra.DeploymentFilename), declaration, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// canonicalDeploymentDeclaration is the language step's output for the
// fixture workload. Its source project carries '<' and '&', which canonical
// bytes escape, so a re-encoding publisher would change them.
func canonicalDeploymentDeclaration(t *testing.T) []byte {
	t.Helper()
	data, diagnostics := infra.MarshalDeployment(&infra.AggregatedManifest{
		ProtocolVersion: infra.ProtocolVersion, Workload: deploymentFixturePath,
		Secrets: []infra.AggregatedSecret{{Name: "api-token", Sources: []infra.Source{{Project: "a<b&c", Contributor: infra.ContributorManual}}}},
	})
	if data == nil {
		t.Fatalf("fixture declaration is invalid: %v", diagnostics)
	}
	if !bytes.Contains(data, []byte(`\u003c`)) || !bytes.Contains(data, []byte(`\u0026`)) {
		t.Fatalf("fixture declaration does not exercise escaped bytes: %s", data)
	}
	return data
}

func deploymentMemberPlanParams(members ...releaseset.PlannedMember) map[string]any {
	return map[string]any{
		"app": deploymentFixturePath,
		releaseset.ContextParamName: &releaseset.Plan{
			ProtocolVersion: distributionproto.ProtocolVersion, Namespace: "release", Channels: []string{"stable"},
			Heads:   map[string]*distributionproto.ChannelHead{"stable": nil},
			Members: members,
		},
	}
}

func selectedDeploymentPlanMember() releaseset.PlannedMember {
	return releaseset.PlannedMember{
		Ecosystem: "put", Coordinate: deploymentFixtureCoordinate, Version: "rs-20261003-1",
		SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("b", 64),
		Selected: true, ProjectID: deploymentFixtureProjectID, Kind: distributionproto.KindDeployment,
	}
}

func refusingDeploymentClient(requests *int) IO {
	return IO{Stdout: func(string) {}, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		*requests++
		return nil, io.ErrUnexpectedEOF
	})}}
}

func TestManagedDeploymentPublishStoresTheDeclarationByteForByteAndEmitsTheMember(t *testing.T) {
	declaration := canonicalDeploymentDeclaration(t)
	root := deploymentPublishFixture(t, declaration)
	bearer := jwt(map[string]any{"aud": "distribution", "scope": "put", "sub": "fixture-publisher"})
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if got := request.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Fatalf("authorization = %q", got)
		}
		if request.URL.Path != "/put/"+deploymentFixtureCoordinate+"/publish" {
			t.Fatalf("request path = %s", request.URL.Path)
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body["payload"], declaration) || string(body["channel"]) != `""` ||
			string(body["version"]) != `"rs-20261003-1"` || string(body["media_type"]) != `"`+put.DeploymentManifestMediaType+`"` {
			t.Fatalf("publish body = %s", raw)
		}
		writeCreatedJSONResponse(t, w, map[string]any{
			"package": deploymentFixtureCoordinate,
			"version": map[string]any{
				"id": "version-1", "package_id": "package-1", "version": "rs-20261003-1",
				"manifest_id": "manifest-1", "state": "published", "visibility": "private",
			},
			"manifest": map[string]any{
				"id": "manifest-1", "package_id": "package-1", "media_type": put.DeploymentManifestMediaType, "payload": body["payload"],
			},
			"channel": nil,
		})
	}))
	defer server.Close()
	params := deploymentMemberPlanParams(selectedDeploymentPlanMember())
	params["registry-put-url"] = server.URL
	params["channel"] = "stable"
	var event map[string]any
	err := publishDeployment(params, nil, root, configMemberPutCredential(t, server.URL, bearer), IO{
		Client: server.Client(), Stdout: func(string) {},
		Artifact: func(id, name, kind, _ string, data map[string]any) error {
			if id != "deployment-cloud-shop-workloads-api-deployment" || name != deploymentFixtureCoordinate || kind != extensionproto.PublishedMemberEventKind {
				t.Fatalf("event envelope = %s/%s/%s", id, name, kind)
			}
			event = data
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || event["ecosystem"] != "put" || event["coordinate"] != deploymentFixtureCoordinate ||
		event["version"] != "rs-20261003-1" || event["artifactDigest"] != put.Digest(declaration) {
		t.Fatalf("calls=%v event=%+v", calls, event)
	}
}

func TestManagedDeploymentPublishDryRunReadsAndChecksWithoutRegistryAccess(t *testing.T) {
	declaration := canonicalDeploymentDeclaration(t)
	root := deploymentPublishFixture(t, declaration)
	params := deploymentMemberPlanParams(selectedDeploymentPlanMember())
	params["dry-run"] = true
	params["json"] = true
	requests := 0
	var output []string
	ioctx := refusingDeploymentClient(&requests)
	ioctx.Stdout = func(line string) { output = append(output, line) }
	if err := publishDeployment(params, nil, root, nil, ioctx); err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(output, "\n")
	if requests != 0 || !strings.Contains(rendered, put.Digest(declaration)) || !strings.Contains(rendered, `"dry-run"`) {
		t.Fatalf("requests=%d output=%v", requests, output)
	}
}

func TestManagedDeploymentPublishFailsOnAMissingOrInvalidDeclaration(t *testing.T) {
	valid := canonicalDeploymentDeclaration(t)
	var indented bytes.Buffer
	if err := json.Indent(&indented, valid, "", "  "); err != nil {
		t.Fatal(err)
	}
	withSchema := append([]byte(`{"$schema":"https://putnami.dev/schemas/infra-aggregated.json",`), valid[1:]...)
	otherWorkload, diagnostics := infra.MarshalDeployment(&infra.AggregatedManifest{ProtocolVersion: infra.ProtocolVersion, Workload: "shop/workloads/other"})
	if otherWorkload == nil {
		t.Fatalf("fixture: %v", diagnostics)
	}
	for name, test := range map[string]struct {
		declaration []byte
		want        string
	}{
		"missing":          {declaration: nil, want: "does not exist"},
		"indented":         {declaration: indented.Bytes(), want: "is invalid"},
		"trailing newline": {declaration: append(append([]byte(nil), valid...), '\n'), want: "is invalid"},
		"schema member":    {declaration: withSchema, want: "is invalid"},
		"unknown member":   {declaration: append([]byte(`{"extra":1,`), valid[1:]...), want: "is invalid"},
		"not json":         {declaration: []byte("not json"), want: "is invalid"},
		"empty":            {declaration: []byte{}, want: "is invalid"},
		"another workload": {declaration: otherWorkload, want: `declares workload "shop/workloads/other"`},
		"beyond the bound": {declaration: bytes.Repeat([]byte(" "), put.MaxManifestBytes+1), want: "exceeds"},
	} {
		t.Run(name, func(t *testing.T) {
			root := deploymentPublishFixture(t, test.declaration)
			requests := 0
			err := publishDeployment(deploymentMemberPlanParams(selectedDeploymentPlanMember()), nil, root, nil, refusingDeploymentClient(&requests))
			if err == nil || !strings.Contains(err.Error(), test.want) || requests != 0 {
				t.Fatalf("error=%v requests=%d, want %q before any request", err, requests, test.want)
			}
		})
	}
}

// A project whose name differs from its path, like sites/putnami.dev named
// putnami.dev: the framework names the declaration's workload by project ID,
// the value the release set and Control use, so the step accepts the ID and
// refuses the name.
func TestManagedDeploymentPublishMatchesTheWorkloadByProjectIDNotName(t *testing.T) {
	const path, name, coordinate = "sites/example", "example", "cloud/example-deployment"
	declaration := func(workload string) []byte {
		t.Helper()
		data, diagnostics := infra.MarshalDeployment(&infra.AggregatedManifest{ProtocolVersion: infra.ProtocolVersion, Workload: workload})
		if data == nil {
			t.Fatalf("fixture declaration is invalid: %v", diagnostics)
		}
		return data
	}
	// dir is the project's physical path. A grouping folder such as "(web)"
	// is not part of the project ID (the framework's ProjectIDFromPath).
	for label, test := range map[string]struct {
		dir      string
		workload string
		want     string
	}{
		"project id":                {dir: path, workload: path},
		"project name":              {dir: path, workload: name, want: `declares workload "example", not "sites/example"`},
		"grouping folder":           {dir: "sites/(web)/example", workload: path},
		"grouping folder kept":      {dir: "sites/(web)/example", workload: "sites/(web)/example", want: `declares workload "sites/(web)/example", not "sites/example"`},
		"grouping folder with name": {dir: "sites/(web)/example", workload: name, want: `declares workload "example", not "sites/example"`},
	} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			projectRoot := filepath.Join(root, filepath.FromSlash(test.dir))
			if err := os.MkdirAll(filepath.Join(projectRoot, infra.AggregatedManifestDir), 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSONFile(t, filepath.Join(projectRoot, cloudProjectMarker), map[string]any{
				"name": name, "extensions": []string{"@putnami/typescript"},
				"options": deploymentProbeOptions(map[string]any{"@putnami/typescript": map[string]any{"deployment": true}}),
			})
			payload := declaration(test.workload)
			if err := os.WriteFile(filepath.Join(projectRoot, infra.AggregatedManifestDir, infra.DeploymentFilename), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			member := selectedDeploymentPlanMember()
			member.Coordinate, member.ProjectID = coordinate, "/"+path
			params := deploymentMemberPlanParams(member)
			params["app"], params["dry-run"], params["json"] = name, true, true
			requests := 0
			var output []string
			ioctx := refusingDeploymentClient(&requests)
			ioctx.Stdout = func(line string) { output = append(output, line) }
			err := publishDeployment(params, nil, root, nil, ioctx)
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) || requests != 0 {
					t.Fatalf("error=%v requests=%d, want %q before any request", err, requests, test.want)
				}
				return
			}
			rendered := strings.Join(output, "\n")
			if err != nil || requests != 0 || !strings.Contains(rendered, `"dry-run"`) || !strings.Contains(rendered, put.Digest(payload)) {
				t.Fatalf("error=%v requests=%d output=%v", err, requests, output)
			}
		})
	}
}

func TestDeploymentTargetWorkloadIsTheMemberProjectID(t *testing.T) {
	for label, test := range map[string]struct {
		target deploymentTarget
		want   string
	}{
		"nested project":         {target: deploymentTarget{ProjectID: "/sites/putnami.dev", Name: "putnami.dev"}, want: "sites/putnami.dev"},
		"name equals path":       {target: deploymentTarget{ProjectID: deploymentFixtureProjectID, Name: deploymentFixturePath}, want: deploymentFixturePath},
		"workspace root project": {target: deploymentTarget{ProjectID: "/", Name: "app"}, want: "app"},
	} {
		t.Run(label, func(t *testing.T) {
			if got := test.target.workload(); got != test.want {
				t.Fatalf("workload() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestManagedDeploymentPublishSkipsWithoutASelectedDeploymentMember(t *testing.T) {
	// A plan without a baseline head selects every member, so the plan that
	// leaves this project out selects another workload's deployment member.
	otherWorkload := selectedDeploymentPlanMember()
	otherWorkload.Coordinate, otherWorkload.ProjectID = "cloud/shop-workloads-web-deployment", "/shop/workloads/web"
	config := releaseset.PlannedMember{
		Ecosystem: "put", Coordinate: "cloud/shop-workloads-api-config", Version: "1.2.3",
		SourceRevision: strings.Repeat("a", 40), SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
		Selected: true, ProjectID: deploymentFixtureProjectID, Kind: distributionproto.KindConfig,
	}
	for name, params := range map[string]map[string]any{
		"no plan":              {"app": deploymentFixturePath},
		"only a config member": deploymentMemberPlanParams(config),
		"another workload":     deploymentMemberPlanParams(otherWorkload),
	} {
		t.Run(name, func(t *testing.T) {
			// No declaration exists: a skip must not read it.
			root := deploymentPublishFixture(t, nil)
			requests := 0
			if err := publishDeployment(params, nil, root, nil, refusingDeploymentClient(&requests)); err != nil || requests != 0 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}
}

func TestManagedDeploymentPublishSkipsAProjectWithoutAConfigNamespace(t *testing.T) {
	root := deploymentPublishFixture(t, nil)
	writeJSONFile(t, filepath.Join(root, filepath.FromSlash(deploymentFixturePath), cloudProjectMarker), map[string]any{
		"name": deploymentFixturePath, "extensions": []string{"@putnami/go"},
	})
	requests := 0
	member := selectedDeploymentPlanMember()
	member.ProjectID = "/shop/workloads/other"
	if err := publishDeployment(deploymentMemberPlanParams(member), nil, root, nil, refusingDeploymentClient(&requests)); err != nil || requests != 0 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
}

func TestDeploymentMemberSelectionRefusesEveryDisagreement(t *testing.T) {
	target := deploymentTarget{ProjectID: deploymentFixtureProjectID, Coordinate: deploymentFixtureCoordinate}
	selected := selectedDeploymentPlanMember()
	otherProject := selected
	otherProject.ProjectID = "/shop/workloads/other"
	otherKind := selected
	otherKind.Kind = distributionproto.KindConfig
	elsewhere := selected
	elsewhere.Coordinate = "cloud/shop-workloads-api-other"
	for name, test := range map[string]struct {
		target  deploymentTarget
		params  map[string]any
		members []releaseset.PlannedMember
		want    string
	}{
		"another project":    {target: target, members: []releaseset.PlannedMember{otherProject}, want: "belongs to project /shop/workloads/other"},
		"another kind":       {target: target, members: []releaseset.PlannedMember{otherKind}, want: `classifies cloud/shop-workloads-api-deployment as "config"`},
		"another coordinate": {target: target, members: []releaseset.PlannedMember{elsewhere}, want: "whose deployment coordinate is"},
		"no coordinate":      {target: deploymentTarget{ProjectID: deploymentFixtureProjectID}, members: []releaseset.PlannedMember{selected}, want: "whose deployment coordinate is"},
		"repeat":             {target: target, members: []releaseset.PlannedMember{selected, selected}, want: "more than once"},
		"unplanned channel":  {target: target, params: map[string]any{"channel": "stable,canary"}, members: []releaseset.PlannedMember{selected}, want: "--channel"},
		"empty channel":      {target: target, params: map[string]any{"channel": " , "}, members: []releaseset.PlannedMember{selected}, want: "--channel"},
		"non-string channel": {target: target, params: map[string]any{"channel": true}, members: []releaseset.PlannedMember{selected}, want: "--channel"},
	} {
		t.Run(name, func(t *testing.T) {
			params := test.params
			if params == nil {
				params = map[string]any{}
			}
			plan := &releaseset.Plan{Channels: []string{"stable"}, Members: test.members}
			if _, _, err := selectedDeploymentMember(params, plan, test.target); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	unattributed := selected
	unattributed.Kind = ""
	member, found, err := selectedDeploymentMember(map[string]any{"channel": "stable"}, &releaseset.Plan{Channels: []string{"stable"}, Members: []releaseset.PlannedMember{unattributed}}, target)
	if err != nil || !found || member.Version != selected.Version {
		t.Fatalf("an unattributed member at the coordinate must be selected: %+v %v %v", member, found, err)
	}
}

func TestPublishedDeploymentMemberUsesTheNativeRuntimeArtifactEnvelope(t *testing.T) {
	var output []string
	runtimeIO := putnamiEventIO(IO{Stdout: func(line string) { output = append(output, line) }}, &eventState{}, runtimeproto.MaxKnownProtocolVersion)
	err := emitDeploymentPublishedMember(runtimeIO, &distributioncli.DeploymentMemberPublicationResult{
		Coordinate: deploymentFixtureCoordinate, Version: "rs-20261003-1", ArtifactDigest: "sha256:" + strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 {
		t.Fatalf("runtime output = %q, want one artifact", output)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(output[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "artifact" || event["kind"] != extensionproto.PublishedMemberEventKind ||
		event["id"] != "deployment-cloud-shop-workloads-api-deployment" || event["ecosystem"] != "put" || event["coordinate"] != deploymentFixtureCoordinate {
		t.Fatalf("artifact envelope = %+v", event)
	}
	if err := emitDeploymentPublishedMember(runtimeIO, &distributioncli.DeploymentMemberPublicationResult{Coordinate: deploymentFixtureCoordinate}); err == nil {
		t.Fatal("an incomplete publication result must be refused before emission")
	}
	if err := emitDeploymentPublishedMember(IO{}, &distributioncli.DeploymentMemberPublicationResult{}); err != nil {
		t.Fatalf("without a runtime emitter the step emits nothing: %v", err)
	}
}

func TestExtensionManifestRoutesTheDeploymentMemberToItsPublishStep(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	publish := manifest["commands"].(map[string]any)["publish"].(map[string]any)
	var step map[string]any
	for _, raw := range publish["run"].([]any) {
		if candidate := raw.(map[string]any); candidate["id"] == cloudDeploymentPublishStep {
			step = candidate
		}
	}
	// Like every other publish channel, the step runs unless the operator
	// skips it alone; otherwise it decides from the plan.
	if step == nil || step["task"] != "cloud-publish-deployment-project" || step["if"] != "!params.skip-publish-deployment && !params.skipPublishDeployment" {
		t.Fatalf("publish step %s = %v", cloudDeploymentPublishStep, step)
	}
	if _, ok := publish["flags"].(map[string]any)["skip-publish-deployment"]; !ok {
		t.Fatal("publish must expose skip-publish-deployment so the deployment step can be skipped alone")
	}
	if !isBooleanFlag("skip-publish-deployment") || !isBooleanFlag("skipPublishDeployment") {
		t.Fatal("skip-publish-deployment must parse as a boolean flag")
	}
	task, ok := manifest["tasks"].(map[string]any)["cloud-publish-deployment-project"].(map[string]any)
	if !ok {
		t.Fatal("manifest missing cloud-publish-deployment-project task")
	}
	// No --if-present: a missing declaration of a selected member fails.
	args := task["args"].([]any)
	if len(args) != 1 || args[0] != "publish-deployment" || task["cwd"] != "{projectRoot}" || task["cache"] != false {
		t.Fatalf("task = %v", task)
	}
	effects := task["declares"].(map[string]any)["effects"].([]any)
	if len(effects) != 2 || effects[0] != "network" || effects[1] != "registry" {
		t.Fatalf("task effects = %v", effects)
	}
}

func TestRunPublishDeploymentDispatchesAndSkipsWithoutAPlan(t *testing.T) {
	root := deploymentPublishFixture(t, nil)
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot": root,
		"params":        map[string]any{"json": true, "app": deploymentFixturePath},
	})
	var output []string
	code := RunMain([]string{"publish-deployment", "--putnamiContext", contextFile}, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if code != 0 {
		t.Fatalf("code = %d, output = %v", code, output)
	}
	result := findEvent(decodeEvents(t, output), "result")
	payload := result["data"].(map[string]any)["data"].(map[string]any)
	if payload["status"] != "skipped" || payload["reason"] != "no release-set plan" {
		t.Fatalf("result = %+v", payload)
	}
}

// deploymentOutboxEnv is the environment the engine gives a publish job under
// publication-v1: a private outbox directory and no registry credential.
func deploymentOutboxEnv(t *testing.T) (map[string]string, string) {
	t.Helper()
	outbox := filepath.Join(t.TempDir(), "outbox")
	return map[string]string{"PUTNAMI_HOME": t.TempDir(), extensionproto.PublicationOutboxEnv: outbox}, outbox
}

// Under publication-v1 the step packs exactly the bytes the direct path
// uploads: the declaration under the deployment media type, with no blob, so
// the engine's upload carries the same artifact digest. It reaches no
// registry and emits no published member; the engine reports the member.
func TestManagedDeploymentPublishPacksTheBytesTheDirectPathUploads(t *testing.T) {
	declaration := canonicalDeploymentDeclaration(t)
	root := deploymentPublishFixture(t, declaration)

	var uploaded []byte
	var uploadedMediaType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		uploaded = append([]byte(nil), body["payload"]...)
		if err := json.Unmarshal(body["media_type"], &uploadedMediaType); err != nil {
			t.Fatal(err)
		}
		writeCreatedJSONResponse(t, w, map[string]any{
			"package": deploymentFixtureCoordinate,
			"version": map[string]any{
				"id": "version-1", "package_id": "package-1", "version": "rs-20261003-1",
				"manifest_id": "manifest-1", "state": "published", "visibility": "private",
			},
			"manifest": map[string]any{
				"id": "manifest-1", "package_id": "package-1", "media_type": uploadedMediaType, "payload": body["payload"],
			},
			"channel": nil,
		})
	}))
	defer server.Close()
	bearer := jwt(map[string]any{"aud": "distribution", "scope": "put", "sub": "fixture-publisher"})
	direct := deploymentMemberPlanParams(selectedDeploymentPlanMember())
	direct["registry-put-url"] = server.URL
	if err := publishDeployment(direct, nil, root, configMemberPutCredential(t, server.URL, bearer), IO{
		Client: server.Client(), Stdout: func(string) {}, Artifact: func(string, string, string, string, map[string]any) error { return nil },
	}); err != nil {
		t.Fatalf("direct publish: %v", err)
	}

	env, outbox := deploymentOutboxEnv(t)
	var stdout []string
	err := publishDeployment(deploymentMemberPlanParams(selectedDeploymentPlanMember()), nil, root, env, IO{
		Client: refusingOutboxClient(t), Artifact: refusingArtifact(t),
		Stdout: func(line string) { stdout = append(stdout, line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	packed, err := publicationoutbox.Read(outbox)
	if err != nil {
		t.Fatalf("engine reader refused the outbox: %v", err)
	}
	if len(packed.Descriptor.Members) != 1 {
		t.Fatalf("outbox members = %+v, want exactly the planned deployment member", packed.Descriptor.Members)
	}
	member := packed.Descriptor.Members[0]
	if member.Ecosystem != extensionproto.OutboxEcosystemPut || member.Coordinate != deploymentFixtureCoordinate ||
		member.Version != "rs-20261003-1" || member.Project != deploymentFixtureProjectID || member.Put == nil ||
		member.Put.MediaType != uploadedMediaType || member.Put.MediaType != put.DeploymentManifestMediaType || len(member.Put.Blobs) != 0 {
		t.Fatalf("packed member = %+v, want the planned deployment member with no blob", member)
	}
	manifest, err := packed.ReadFile(member.Put.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, uploaded) || !bytes.Equal(manifest, declaration) || member.Put.Manifest.Digest != put.Digest(declaration) {
		t.Fatalf("packed manifest %s (%s), direct upload %s, declaration digest %s", manifest, member.Put.Manifest.Digest, uploaded, put.Digest(declaration))
	}
	if got := strings.Join(stdout, "\n"); !strings.Contains(got, "Packed deployment member "+deploymentFixtureCoordinate+"@rs-20261003-1 into the publication outbox.") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestDeploymentPublishUnderThePublicationOutboxPacksNothingElse(t *testing.T) {
	otherWorkload := selectedDeploymentPlanMember()
	otherWorkload.Coordinate, otherWorkload.ProjectID = "cloud/shop-workloads-web-deployment", "/shop/workloads/web"
	dryRun := deploymentMemberPlanParams(selectedDeploymentPlanMember())
	dryRun["dry-run"] = true
	// An outbox job holds no credential, so without a plan it has nothing it
	// may pack and fails, as the Config step does. With a plan it packs only
	// a selected member, and a dry run packs nothing.
	for name, test := range map[string]struct {
		params map[string]any
		want   string
	}{
		"no plan":            {params: map[string]any{"app": deploymentFixturePath}, want: "the job carries no plan"},
		"no selected member": {params: deploymentMemberPlanParams(otherWorkload)},
		"dry run":            {params: dryRun},
	} {
		t.Run(name, func(t *testing.T) {
			root := deploymentPublishFixture(t, canonicalDeploymentDeclaration(t))
			env, outbox := deploymentOutboxEnv(t)
			err := publishDeployment(test.params, nil, root, env, IO{
				Client: refusingOutboxClient(t), Artifact: refusingArtifact(t), Stdout: func(string) {},
			})
			if (test.want == "" && err != nil) || (test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want))) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
				t.Fatalf("the step touched the outbox: %v", statErr)
			}
		})
	}
}

func TestDeploymentPublishUnderThePublicationOutboxFailsOnAnInvalidDeclaration(t *testing.T) {
	valid := canonicalDeploymentDeclaration(t)
	root := deploymentPublishFixture(t, append(append([]byte(nil), valid...), '\n'))
	env, outbox := deploymentOutboxEnv(t)
	err := publishDeployment(deploymentMemberPlanParams(selectedDeploymentPlanMember()), nil, root, env, IO{
		Client: refusingOutboxClient(t), Artifact: refusingArtifact(t), Stdout: func(string) {},
	})
	if err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("error = %v, want the invalid declaration refused", err)
	}
	if _, statErr := os.Stat(outbox); !os.IsNotExist(statErr) {
		t.Fatalf("a refused declaration touched the outbox: %v", statErr)
	}
}
