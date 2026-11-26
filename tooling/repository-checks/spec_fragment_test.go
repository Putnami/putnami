package repositorychecks

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// spectestSourceRepoPath declares the observation a spec-check proof writes.
const spectestSourceRepoPath = "protocols/features/spectest/spectest.go"

// spectestMeasurementMembers are the members of a threshold observation. An
// acceptance proof, the only kind these checks write, omits them.
var spectestMeasurementMembers = []string{"environment", "measurement", "window"}

// TestSpecFragmentFollowsTheSpectestObservation holds the fragment proves
// writes to the observation the features protocol declares: the same
// collection directory variable and the same members, in the same order, so
// the collector reads it as it reads a spectest proof.
func TestSpecFragmentFollowsTheSpectestObservation(t *testing.T) {
	t.Parallel()
	source := filepath.Join(repositoryRoot(t), filepath.FromSlash(spectestSourceRepoPath))
	if env := stringConstant(t, source, "FragmentDirEnv"); env != specFragmentDirEnv {
		t.Fatalf("%s collects fragments from $%s; proves writes them to $%s", spectestSourceRepoPath, env, specFragmentDirEnv)
	}
	declared := spectestFragmentMembers(t, source)
	var want []string
	for _, member := range declared {
		if !slices.Contains(spectestMeasurementMembers, member) {
			want = append(want, member)
		}
	}
	var got []string
	fields := reflect.TypeFor[specFragment]()
	for index := range fields.NumField() {
		got = append(got, strings.Split(fields.Field(index).Tag.Get("json"), ",")[0])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the spec fragment writes members %v; %s declares %v (measurement members %v aside)",
			got, spectestSourceRepoPath, want, spectestMeasurementMembers)
	}

	encoded, err := json.Marshal(specFragment{Feature: "f", Requirement: "r", Check: "c", Status: "passed", File: "x_test.go", Symbol: "TestX"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"feature":"f","requirement":"r","check":"c","status":"passed","file":"x_test.go","symbol":"TestX"}`; string(encoded) != want {
		t.Fatalf("fragment = %s, want %s", encoded, want)
	}
}

// spectestFragmentMembers returns the JSON member names of the Fragment type
// declared in the Go file at path, in declaration order.
func spectestFragmentMembers(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.TYPE {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Fragment" {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s declares Fragment as something other than a struct", path)
			}
			var members []string
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					t.Fatalf("%s: a Fragment field has no JSON tag", path)
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					t.Fatalf("%s: unquote a Fragment tag: %v", path, err)
				}
				members = append(members, strings.Split(reflect.StructTag(tag).Get("json"), ",")[0])
			}
			return members
		}
	}
	t.Fatalf("%s declares no Fragment type", path)
	return nil
}
