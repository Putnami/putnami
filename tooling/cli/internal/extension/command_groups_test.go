package extension

import (
	"reflect"
	"testing"
)

func sampleExtensionWithGroup() *ExtensionDescription {
	jobLogin := &JobDefinition{
		ExtensionName: "@putnami/cloud",
		Name:          "cloud-login",
		Kind:          "command",
	}
	jobStatus := &JobDefinition{
		ExtensionName: "@putnami/cloud",
		Name:          "cloud-status",
		Kind:          "command",
		// Flat command target flags — the lowest-precedence layer. "env" is
		// overridden by the group, "region" survives untouched.
		Flags: map[string]FlagDefinition{
			"env":    {Type: "string", Default: "command"},
			"region": {Type: "string"},
		},
	}
	return &ExtensionDescription{
		Name: "@putnami/cloud",
		Commands: map[string]string{
			"cloud-login":  "Sign in",
			"cloud-status": "Show status",
		},
		Jobs: map[string]*JobDefinition{
			"cloud-login":  jobLogin,
			"cloud-status": jobStatus,
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Description: "Manage Putnami Cloud",
				// Group-level shared flags — middle-precedence layer. "env"
				// overrides the flat command target; "format" is new and
				// inherited by every subcommand.
				Flags: map[string]FlagDefinition{
					"env":    {Type: "string", Default: "group"},
					"format": {Type: "string"},
				},
				Subcommands: map[string]SubcommandDefinition{
					"login": {Command: "cloud-login", Interactive: true},
					// The subcommand overrides "env" — highest-precedence layer.
					"status": {
						Command: "cloud-status",
						Flags: map[string]FlagDefinition{
							"env": {Type: "string", Default: "sub"},
						},
					},
				},
			},
		},
	}
}

func TestCommandGroupNames(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	names := CommandGroupNames(exts)
	if !reflect.DeepEqual(names, map[string]bool{"cloud": true}) {
		t.Fatalf("CommandGroupNames = %v, want {cloud:true}", names)
	}
}

func TestCommandGroupNames_Empty(t *testing.T) {
	if names := CommandGroupNames(nil); names != nil {
		t.Errorf("CommandGroupNames(nil) = %v, want nil", names)
	}
	if names := CommandGroupNames([]*ExtensionDescription{{Name: "foo"}}); names != nil {
		t.Errorf("CommandGroupNames(no groups) = %v, want nil", names)
	}
}

// TestCommandSubcommandOwners reports executable claimants of one subcommand,
// allowing different extensions to contribute to the same parent group.
func TestCommandSubcommandOwners(t *testing.T) {
	rival := sampleExtensionWithGroup()
	rival.Name = "@acme/cloud"
	rival.Jobs["cloud-deploy"] = &JobDefinition{
		ExtensionName: rival.Name,
		Name:          "cloud-deploy",
		Kind:          "command",
	}
	rival.CommandGroups["cloud"] = CommandGroupDefinition{
		Subcommands: map[string]SubcommandDefinition{
			"deploy": {Command: "cloud-deploy"},
		},
	}

	single := []*ExtensionDescription{sampleExtensionWithGroup()}
	if got := CommandSubcommandOwners(single, "cloud", "login"); !reflect.DeepEqual(got, []string{"@putnami/cloud"}) {
		t.Errorf("CommandSubcommandOwners(single) = %v, want one owner", got)
	}
	if got := CommandSubcommandOwners(single, "cloud", "nope"); got != nil {
		t.Errorf("CommandSubcommandOwners(unknown subcommand) = %v, want nil", got)
	}

	// Different subcommands under the same group remain independently owned.
	both := []*ExtensionDescription{sampleExtensionWithGroup(), rival}
	if got := CommandSubcommandOwners(both, "cloud", "login"); !reflect.DeepEqual(got, []string{"@putnami/cloud"}) {
		t.Errorf("CommandSubcommandOwners(login) = %v, want the login owner", got)
	}
	if got := CommandSubcommandOwners(both, "cloud", "deploy"); !reflect.DeepEqual(got, []string{"@acme/cloud"}) {
		t.Errorf("CommandSubcommandOwners(deploy) = %v, want the deploy owner", got)
	}

	// Duplicate definitions are still deterministic in their diagnostic.
	duplicate := sampleExtensionWithGroup()
	duplicate.Name = "@acme/duplicate-cloud"
	duplicates := []*ExtensionDescription{sampleExtensionWithGroup(), duplicate}
	want := []string{"@acme/duplicate-cloud", "@putnami/cloud"}
	if got := CommandSubcommandOwners(duplicates, "cloud", "login"); !reflect.DeepEqual(got, want) {
		t.Errorf("CommandSubcommandOwners(duplicate) = %v, want %v", got, want)
	}
	reversed := []*ExtensionDescription{duplicate, sampleExtensionWithGroup()}
	if got := CommandSubcommandOwners(reversed, "cloud", "login"); !reflect.DeepEqual(got, want) {
		t.Errorf("CommandSubcommandOwners is order-dependent: %v, want %v", got, want)
	}
}

func TestResolveSubcommand_InteractiveLogin(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	res, owner := ResolveSubcommand(exts, "cloud", "login")
	if res == nil {
		t.Fatal("ResolveSubcommand returned nil for cloud login")
	}
	if owner != exts[0] {
		t.Error("expected owner to be the cloud extension")
	}
	if res.CommandName != "cloud-login" {
		t.Errorf("CommandName = %q, want cloud-login", res.CommandName)
	}
	if !res.Subdef.Interactive {
		t.Error("Subdef.Interactive = false, want true")
	}
	if res.JobDefinition == nil || res.JobDefinition.Name != "cloud-login" {
		t.Errorf("JobDefinition = %v, want cloud-login", res.JobDefinition)
	}
}

func TestResolveSubcommand_NonInteractive(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	res, _ := ResolveSubcommand(exts, "cloud", "status")
	if res == nil {
		t.Fatal("ResolveSubcommand returned nil for cloud status")
	}
	if res.Subdef.Interactive {
		t.Error("Subdef.Interactive = true, want false (status is non-interactive)")
	}
}

// TestResolveSubcommand_GroupFlagsAndEffectivePrecedence pins the group-level
// flag inheritance contract: ResolveSubcommand populates GroupFlags from the
// group definition, and EffectiveFlags() merges the three layers in
// command < group < subcommand precedence.
func TestResolveSubcommand_GroupFlagsAndEffectivePrecedence(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	res, _ := ResolveSubcommand(exts, "cloud", "status")
	if res == nil {
		t.Fatal("ResolveSubcommand returned nil for cloud status")
	}

	// GroupFlags carries the group's shared flags verbatim.
	wantGroup := map[string]FlagDefinition{
		"env":    {Type: "string", Default: "group"},
		"format": {Type: "string"},
	}
	if !reflect.DeepEqual(res.GroupFlags, wantGroup) {
		t.Errorf("GroupFlags = %#v, want %#v", res.GroupFlags, wantGroup)
	}

	// EffectiveFlags merges flat command < group < subcommand:
	//   env    → subcommand wins ("sub")
	//   format → only the group declares it (inherited)
	//   region → only the flat command declares it (survives)
	want := map[string]FlagDefinition{
		"env":    {Type: "string", Default: "sub"},
		"format": {Type: "string"},
		"region": {Type: "string"},
	}
	if got := res.EffectiveFlags(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveFlags = %#v, want %#v", got, want)
	}
}

// TestResolveSubcommand_UnknownSubcommand returns nil resolved binding but
// reports the owning extension so the dispatcher can produce a helpful error.
func TestResolveSubcommand_UnknownSubcommand(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	res, owner := ResolveSubcommand(exts, "cloud", "bogus")
	if res != nil {
		t.Errorf("ResolveSubcommand should return nil binding for unknown sub, got %+v", res)
	}
	if owner == nil {
		t.Error("owner should be non-nil so the caller can list available subs")
	}
}

// TestResolveSubcommand_UnknownGroup returns nil for both binding and owner.
func TestResolveSubcommand_UnknownGroup(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	res, owner := ResolveSubcommand(exts, "ghost", "anything")
	if res != nil || owner != nil {
		t.Errorf("unknown group should return (nil, nil), got (%v, %v)", res, owner)
	}
}

func TestListSubcommands(t *testing.T) {
	exts := []*ExtensionDescription{sampleExtensionWithGroup()}
	got := ListSubcommands(exts, "cloud")
	want := []string{"login", "status"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSubcommands = %v, want %v", got, want)
	}
}
