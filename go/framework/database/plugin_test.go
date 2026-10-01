package database

import (
	"context"
	"reflect"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
)

// TestPlugin_Configure_RequiresContainer covers the guard that rejects a
// migration-enabled plugin configured without a DI container — the full
// configure-through-Migrate flow is exercised by the app package's
// lifecycle tests.
func TestPlugin_Configure_RequiresContainer(t *testing.T) {
	p := NewPlugin(PluginConfig{Migration: &MigrationConfig{}})
	if err := p.Configure(context.Background(), nil); err == nil {
		t.Fatal("expected error: a migration-enabled plugin requires a container")
	}
}

func TestPluginDesignInfraRequirementsMatchNativeDatasourceDeclarations(t *testing.T) {
	tests := []struct {
		name string
		cfg  PluginConfig
		want []string
	}{
		{name: "canonical names sorted and deduplicated", cfg: PluginConfig{
			Datasource: "platform", Datasources: []DatasourceConfig{{Name: "billing"}, {Name: "platform"}},
		}, want: []string{"billing", "platform"}},
		{name: "pool datasource", cfg: PluginConfig{
			Pool: PoolConfig{Datasource: Datasource{Name: "identity", Schema: "auth"}},
		}, want: []string{"identity"}},
		{name: "legacy database", cfg: PluginConfig{Pool: PoolConfig{Database: "primary"}}, want: []string{"primary"}},
		{name: "default datasource", cfg: PluginConfig{}, want: []string{"default"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := NewPlugin(test.cfg).DesignInfraRequirements()
			got := make([]string, 0, len(requirements))
			for _, requirement := range requirements {
				if requirement.Kind != protocaps.InfraKindDatabase {
					t.Fatalf("infra kind = %q, want database", requirement.Kind)
				}
				got = append(got, requirement.Name)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("datasources = %v, want %v", got, test.want)
			}
		})
	}
}

// TestPlugin_OpenDBForRunner_UnresolvablePool covers the openDB thunk's
// error path: with no owner and no cached pool, the runner cannot obtain a
// *sql.DB and must surface a coded error instead of panicking.
func TestPlugin_OpenDBForRunner_UnresolvablePool(t *testing.T) {
	p := NewPlugin(PluginConfig{})
	thunk := p.openDBForRunner(nil)
	if _, err := thunk(); err == nil {
		t.Fatal("expected error when the pool cannot be resolved")
	}
}
