package testprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// datasourcesCorpus is the datasource-selection corpus both test providers run.
// Its TypeScript twin is the "datasource selection corpus" suite of
// typescript/framework/database/test/test-provider.test.ts.
const datasourcesCorpus = "../../../../protocols/database/conformance/test-provider-datasources.json"

type datasourcesCase struct {
	ID          string       `json:"id"`
	Mode        pdb.TestMode `json:"mode"`
	Datasources []string     `json:"datasources"`
	Expect      struct {
		Datasources []string `json:"datasources"`
		Error       string   `json:"error"`
		Names       []string `json:"names"`
	} `json:"expect"`
}

func TestDatasourceSelectionCorpus(t *testing.T) {
	raw, err := os.ReadFile(datasourcesCorpus)
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Binding json.RawMessage   `json:"binding"`
		Cases   []datasourcesCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus has no cases")
	}
	for _, c := range corpus.Cases {
		t.Run(c.ID, func(t *testing.T) {
			tb, diags := pdb.ParseAndValidateTestBinding(corpus.Binding)
			if diag.HasErrors(diags) {
				t.Fatalf("corpus binding is invalid: %v", diags)
			}
			if c.Mode != "" {
				tb.Mode = c.Mode
			}

			if c.Expect.Error != "" {
				_, err := Provision(context.Background(), Options{Binding: tb, Datasources: c.Datasources})
				if err == nil {
					t.Fatal("Provision succeeded, want an error")
				}
				if got, want := errors.Is(err, ErrSkip), c.Expect.Error == "skip"; got != want {
					t.Fatalf("errors.Is(err, ErrSkip) = %v, want %v (err: %v)", got, want, err)
				}
				if want := "has no datasource " + quoteNames(c.Expect.Names); !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
				return
			}

			selected, err := selectDatasources(tb, c.Datasources)
			if err != nil {
				t.Fatalf("selectDatasources: %v", err)
			}
			plans, err := planDatabases(selected, fixedSuffix("s"))
			if err != nil {
				t.Fatalf("planDatabases: %v", err)
			}
			planned := make([]string, len(plans))
			for i, p := range plans {
				planned[i] = p.name
			}
			if !slices.Equal(planned, c.Expect.Datasources) {
				t.Errorf("planned %v, want %v", planned, c.Expect.Datasources)
			}
			binding, err := runtimeBinding(plans)
			if err != nil {
				t.Fatalf("runtimeBinding: %v", err)
			}
			bound := make([]string, 0, len(binding.Databases))
			for name := range binding.Databases {
				bound = append(bound, name)
			}
			sort.Strings(bound)
			if !slices.Equal(bound, c.Expect.Datasources) {
				t.Errorf("binding carries %v, want %v", bound, c.Expect.Datasources)
			}
		})
	}
}

func TestSelectDatasources_LeavesTheCallersBindingIntact(t *testing.T) {
	tb := &pdb.TestBinding{
		Mode: pdb.TestModeRequire,
		Databases: map[string]pdb.Database{
			"auth":    {Engine: pdb.EnginePostgres, Connection: tcp("auth")},
			"billing": {Engine: pdb.EnginePostgres, Connection: tcp("billing")},
		},
	}
	if got, err := selectDatasources(tb, nil); err != nil || got != tb {
		t.Fatalf("selectDatasources(nil) = %p, %v; want the binding itself", got, err)
	}
	got, err := selectDatasources(tb, []string{"auth"})
	if err != nil {
		t.Fatalf("selectDatasources: %v", err)
	}
	if len(got.Databases) != 1 || got.Mode != pdb.TestModeRequire {
		t.Errorf("selected = %+v, want auth only with the binding's policy", got)
	}
	if len(tb.Databases) != 2 {
		t.Errorf("caller's binding lost entries: %v", tb.Databases)
	}
}
