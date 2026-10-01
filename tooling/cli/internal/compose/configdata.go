package compose

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ConfigDataEnv is the environment variable both runtimes read inline
// configuration from, at priority 60 (above every file source). Compose never
// logs, prints or records its value.
const ConfigDataEnv = "CONFIG_DATA"

// Configuration sections compose injects.
const (
	SectionClients  = "clients"
	SectionDatabase = "database"
)

// noContractNote is recorded on a provider without a committed client contract:
// its consumers reach it under its project name only.
const noContractNote = "no client contract committed (schema/openapi.json); dependants reach it under its project name only"

// contractCandidates are the provider documents that carry the
// `x-putnami-client` metadata, in the order the client generator's committed
// view reads them (tooling/clientgen-extension workspaceclient.committedSpecPath).
var contractCandidates = []string{
	"schema/openapi.json",
	".gen/schema/openapi.json",
	".gen/schema/openapi.json.gz",
}

// providerServiceIDs returns the service ids a provider's client contract
// declares: `x-putnami-client.service.id`, strictly validated. A provider that
// commits no contract, or whose contract carries no client metadata, declares
// none. A contract that is present and invalid is an error: injecting under a
// guessed id would hide it.
func providerServiceIDs(workspaceRoot string, provider *workspace.Project) ([]string, error) {
	projectRoot := filepath.Join(workspaceRoot, provider.Path)
	for _, candidate := range contractCandidates {
		path := filepath.Join(projectRoot, filepath.FromSlash(candidate))
		data, err := readContract(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, newError(CodeInvalidRequirement, provider.ID, PhasePlan,
				"read client contract "+candidate+": "+err.Error())
		}
		var document struct {
			Client json.RawMessage `json:"x-putnami-client"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, newError(CodeInvalidRequirement, provider.ID, PhasePlan,
				"client contract "+candidate+" is not JSON: "+err.Error())
		}
		if len(document.Client) == 0 {
			return nil, nil
		}
		metadata, diags := clientcontract.ParseAndValidateDocument(document.Client)
		if metadata == nil || diag.HasErrors(diags) {
			return nil, newError(CodeInvalidRequirement, provider.ID, PhasePlan,
				"client contract "+candidate+" is invalid: "+firstErrorMessage(diags))
		}
		if metadata.Service.ID == "" {
			return nil, nil
		}
		return []string{metadata.Service.ID}, nil
	}
	return nil, nil
}

func readContract(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a fixed path under a workspace project
	if err != nil {
		return nil, err
	}
	if filepath.Ext(path) != ".gz" {
		return data, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// serviceBinding is the `clients.services.<id>` entry both runtimes read.
type serviceBinding struct {
	URL string `json:"url"`
}

// configDocument is the CONFIG_DATA document of one member.
type configDocument struct {
	Clients  *clientsSection  `json:"clients,omitempty"`
	Database *databaseSection `json:"database,omitempty"`
}

type clientsSection struct {
	Services map[string]serviceBinding `json:"services"`
}

type databaseSection struct {
	ProtocolVersion int                     `json:"protocolVersion"`
	Databases       map[string]pdb.Database `json:"databases"`
}

// configData builds the CONFIG_DATA document of m: the proxy URL of every
// member it runs with, under each service id that member's client contract
// declares and under its project name, and a database binding per datasource.
// A section with nothing to say is omitted. The document is JSON, which both
// runtimes' YAML readers accept.
//
// The second result names the sections the document carries: that list, never
// the document, is what a composition reports.
func configData(m *Member, plan *Plan) ([]byte, []string, error) {
	if plan == nil || m == nil {
		return nil, nil, errors.New("configData needs a member and its plan")
	}
	var document configDocument
	var sections []string

	if len(m.runsWith) > 0 {
		services := make(map[string]serviceBinding)
		for _, provider := range m.runsWith {
			if provider.ProxyURL == "" {
				return nil, nil, newError(CodeStartFailed, m.Project.ID, PhaseStart,
					"provider "+provider.Project.ID+" has no proxy URL yet")
			}
			binding := serviceBinding{URL: provider.ProxyURL}
			for _, id := range provider.serviceIDs {
				services[id] = binding
			}
			if provider.Project.Name != "" {
				services[provider.Project.Name] = binding
			}
		}
		document.Clients = &clientsSection{Services: services}
		sections = append(sections, SectionClients)
	}

	if len(m.Databases) > 0 {
		databases := make(map[string]pdb.Database, len(m.Databases))
		for _, binding := range m.Databases {
			if binding.connection == nil {
				return nil, nil, newError(CodeDatabaseFailed, m.Project.ID, PhaseDatabases,
					"datasource "+binding.Datasource+" has no provisioned connection")
			}
			connection := *binding.connection
			databases[binding.Datasource] = pdb.Database{
				Engine:     pdb.EnginePostgres,
				Schema:     binding.Schema,
				Connection: &connection,
			}
		}
		document.Database = &databaseSection{ProtocolVersion: pdb.ProtocolVersion, Databases: databases}
		sections = append(sections, SectionDatabase)
	}

	sort.Strings(sections)
	data, err := json.Marshal(document)
	if err != nil {
		return nil, nil, err
	}
	return data, sections, nil
}

// checkConfigDataConflict fails closed when the member's process would already
// receive CONFIG_DATA from somewhere compose does not own: the invoking
// environment, or the serve task's own manifest Env (which the job runner
// appends after, and would therefore silently replace, the injected value).
func checkConfigDataConflict(m *Member) error {
	if os.Getenv(ConfigDataEnv) != "" {
		return newError(CodeConfigDataConflict, m.Project.ID, PhaseStart,
			ConfigDataEnv+" is already set in this environment; compose injects it and refuses to merge or replace a value it does not own")
	}
	if m.ServeJob != nil && m.ServeJob.JobDef != nil {
		if _, declared := m.ServeJob.JobDef.Env[ConfigDataEnv]; declared {
			return newError(CodeConfigDataConflict, m.Project.ID, PhaseStart,
				"the serve task declares "+ConfigDataEnv+" in its own environment, which would replace the injected document")
		}
	}
	return nil
}
