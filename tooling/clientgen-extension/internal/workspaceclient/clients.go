package workspaceclient

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// defaultGoClientName is the client type name the Go emitter writes when the
// generation options name none.
const defaultGoClientName = "Client"

// clientProject is the part of a member's putnami.json that makes it a
// generated client.
type clientProject struct {
	GeneratedFrom *struct {
		Service    string `json:"service"`
		Language   string `json:"language"`
		ClientName string `json:"clientName"`
	} `json:"generatedFrom"`
}

// discoverClients finds the generated clients of every first-party provider,
// keyed by the provider's project path. A client is a member project whose
// committed putnami.json names the provider's service in generatedFrom, or the
// in-module Go client the provider's committed clientgen.json declares. Each
// symbol derives from committed files only: the contract, the generation
// options, and the client's own go.mod or package.json. A client whose import
// path cannot be read is left out, so the guard names no binding it cannot
// spell; client-generate reports why that client does not generate.
func discoverClients(view workspaceView, providers []provider) map[string][]ClientReport {
	byService := map[string]provider{}
	clients := map[string][]ClientReport{}
	for _, item := range providers {
		// An empty contract generates no client (ADR 0004), so it has no
		// binding to name.
		if item.classification != ClassificationFirstParty || item.document == nil || item.emptyFirstPartyContract() {
			continue
		}
		byService[item.document.Service.ID] = item
		if client, ok := inModuleClient(view.workspaceFiles, item); ok {
			clients[item.rel] = append(clients[item.rel], client)
		}
	}
	members, err := view.members()
	if err != nil {
		return clients // discoverWith reports the membership error.
	}
	for _, projectRel := range members {
		data, readErr := view.Read(joinRel(projectRel, "putnami.json"))
		if readErr != nil {
			continue
		}
		var project clientProject
		if json.Unmarshal(data, &project) != nil || project.GeneratedFrom == nil {
			continue
		}
		owner, found := byService[strings.TrimSpace(project.GeneratedFrom.Service)]
		if !found {
			continue
		}
		var client ClientReport
		var ok bool
		switch clientcontract.GeneratedLanguage(project.GeneratedFrom.Language) {
		case clientcontract.GeneratedLanguageGo:
			client, ok = goClient(view.workspaceFiles, projectRel, "", owner, project.GeneratedFrom.ClientName)
		case clientcontract.GeneratedLanguageTypeScript:
			client, ok = typeScriptClient(view.workspaceFiles, projectRel, owner)
		}
		if ok {
			clients[owner.rel] = append(clients[owner.rel], client)
		}
	}
	for providerRel := range clients {
		sort.Slice(clients[providerRel], func(i, j int) bool {
			left, right := clients[providerRel][i], clients[providerRel][j]
			return left.Project+"\x00"+string(left.Language)+"\x00"+left.ImportPath <
				right.Project+"\x00"+string(right.Language)+"\x00"+right.ImportPath
		})
	}
	return clients
}

// inModuleClient is the Go client a provider's committed clientgen.json
// declares inside the provider's own module, at the go.output package.
func inModuleClient(files workspaceFiles, item provider) (ClientReport, bool) {
	if item.config == nil || !hasGoTarget(item.config.Targets) || item.config.Go.ModulePath != "" {
		return ClientReport{}, false
	}
	output, err := safeWorkspacePath(item.config.Go.Output)
	if err != nil {
		return ClientReport{}, false
	}
	return goClient(files, item.rel, output, item, item.config.Go.ClientName)
}

func hasGoTarget(targets []string) bool {
	for _, target := range targets {
		if target == "go" {
			return true
		}
	}
	return false
}

// goClient reports the Go client the project at projectRel generates into its
// package directory output ("" for the project root). Its import path is the
// project's go.mod module joined with output, and its one binding is
// Register<ClientName>, as the Go emitter writes it.
func goClient(files workspaceFiles, projectRel, output string, owner provider, clientName string) (ClientReport, bool) {
	data, err := files.Read(joinRel(projectRel, "go.mod"))
	if err != nil {
		return ClientReport{}, false
	}
	module := goModulePath(data)
	if module == "" {
		return ClientReport{}, false
	}
	importPath := module
	if output != "" && output != "." {
		importPath = module + "/" + output
	}
	if strings.TrimSpace(clientName) == "" {
		clientName = defaultGoClientName
	}
	return ClientReport{
		Project:    projectRel,
		Language:   clientcontract.GeneratedLanguageGo,
		ImportPath: importPath,
		Bindings: []ClientBinding{{
			Service:       owner.document.Service.ID,
			ClientSymbol:  clientName,
			BindingSymbol: "Register" + clientName,
		}},
	}, true
}

// goModulePath reads the module path a go.mod file declares.
func goModulePath(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		rest, found := strings.CutPrefix(line, "module")
		if !found || rest == "" || (rest[0] != ' ' && rest[0] != '\t' && rest[0] != '"') {
			continue
		}
		rest, _, _ = strings.Cut(rest, "//")
		rest = strings.TrimSpace(rest)
		if unquoted, err := strconv.Unquote(rest); err == nil {
			return unquoted
		}
		return rest
	}
	return ""
}

// typeScriptClient reports the TypeScript client the project at projectRel
// generates. Its import path is the name in its package.json. The emitter
// writes one service per first literal path segment of the contract's
// first-party routes, each with a <Name>Client class and a register<Name>Client
// binding.
func typeScriptClient(files workspaceFiles, projectRel string, owner provider) (ClientReport, bool) {
	data, err := files.Read(joinRel(projectRel, "package.json"))
	if err != nil {
		return ClientReport{}, false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pkg) != nil || strings.TrimSpace(pkg.Name) == "" {
		return ClientReport{}, false
	}
	seen := map[string]bool{}
	var bindings []ClientBinding
	for _, route := range owner.routes {
		service := typeScriptServiceName(route)
		if seen[service] {
			continue
		}
		seen[service] = true
		className := strings.TrimSuffix(service, "Service") + "Client"
		bindings = append(bindings, ClientBinding{Service: service, ClientSymbol: className, BindingSymbol: "register" + className})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Service < bindings[j].Service })
	return ClientReport{
		Project:    projectRel,
		Language:   clientcontract.GeneratedLanguageTypeScript,
		ImportPath: pkg.Name,
		Bindings:   bindings,
	}, true
}

var nonIdentifierRun = regexp.MustCompile(`[^A-Za-z0-9]+`)

// typeScriptServiceName is the service the TypeScript emitter groups a route
// under. It mirrors inferServiceName in
// typescript/framework/client/src/generator/openapi-reader.ts: the first path
// segment that is no parameter, its non-identifier runs read as word breaks,
// in PascalCase, with the Service suffix.
func typeScriptServiceName(route string) string {
	first := "api"
	for _, segment := range strings.Split(route, "/") {
		if segment != "" && !strings.HasPrefix(segment, "{") {
			first = segment
			break
		}
	}
	var name strings.Builder
	for _, word := range strings.FieldsFunc(nonIdentifierRun.ReplaceAllString(first, "-"), func(r rune) bool {
		return r == '-' || r == '_'
	}) {
		name.WriteString(strings.ToUpper(word[:1]) + word[1:])
	}
	result := name.String()
	if result == "" {
		result = "Api"
	}
	if result[0] >= '0' && result[0] <= '9' {
		result = "_" + result
	}
	return result + "Service"
}
