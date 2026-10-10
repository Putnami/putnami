package sdd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The recipe gate of `validate-workspace`.
//
// A recipe index maps an intention ("add a database migration") to the sample
// that demonstrates it, the framework primitives that sample uses, and the
// hand-rolled shapes it replaces. `putnami context generate` renders the
// indexes into the agent guidance, so an agent plans from the sample instead
// of copying the nearest legacy code. A recipe that points at a sample which
// no longer exists sends that agent nowhere, and two recipes for one intention
// leave it to guess; this gate refuses both.
//
// An absent index is adoption, never a failure: a workspace without samples
// has nothing to index. The verdict depends on whether each named sample
// directory exists, so the task reads the workspace through its candidate cut
// (worktree.go) and is cached on the input `git:**`: a sample directory exists
// when it holds a candidate file, and renaming, emptying or deleting one moves
// the key.

// RecipeIndexFilename is the index file inside a language samples directory.
const RecipeIndexFilename = "recipes.json"

// RecipeIndexProtocolVersion selects the exact index wire contract.
const RecipeIndexProtocolVersion = 1

// RecipeIndexDirectories are the language samples directories an index may
// live in, in the order they are validated and rendered.
var RecipeIndexDirectories = []string{"typescript/samples", "go/samples", "python/samples"}

// Recipe diagnostic codes.
const (
	ErrorCodeInvalidRecipeIndex  = "sdd.invalid_recipe_index"
	ErrorCodeRecipeSampleMissing = "sdd.recipe_sample_missing"
)

// recipeLineMaxLength bounds every one-line field: an index is rendered whole
// into agent guidance, so a field is a line, not a document.
const recipeLineMaxLength = 256

// RecipeIndex is one committed `<lang>/samples/recipes.json`.
type RecipeIndex struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Recipes         []Recipe `json:"recipes"`
}

// Recipe maps one intention to the sample that demonstrates it.
type Recipe struct {
	Intention    string   `json:"intention"`
	Sample       string   `json:"sample"`
	Primitives   []string `json:"primitives"`
	AntiPatterns []string `json:"antiPatterns"`
	Since        string   `json:"since"`
}

// RecipesReport is the typed data of one recipe judgment.
type RecipesReport struct {
	// Indexes lists the workspace-relative index files that were read.
	Indexes []string `json:"indexes"`
	// Recipes counts the recipes across every index.
	Recipes int `json:"recipes"`
	// Diagnostics carries every finding, anchored on its index file.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// BuildRecipesResult validates every committed recipe index in the worktree.
func BuildRecipesResult(ws *workspace.Workspace) (RecipesReport, error) {
	report := RecipesReport{Indexes: []string{}}
	tree, err := openWorktree(ws.Root)
	if err != nil {
		return report, fmt.Errorf("recipes: %w", err)
	}
	for _, dir := range RecipeIndexDirectories {
		relative := path.Join(dir, RecipeIndexFilename)
		data, err := tree.readRegular(relative)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		report.Indexes = append(report.Indexes, relative)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeInvalidRecipeIndex, relative,
				"read recipe index: %v", err))
			continue
		}
		index, findings := parseRecipeIndex(relative, data)
		report.Diagnostics = append(report.Diagnostics, findings...)
		if index == nil {
			continue
		}
		report.Recipes += len(index.Recipes)
		report.Diagnostics = append(report.Diagnostics, missingRecipeSamples(tree, relative, index)...)
	}
	sortDiagnostics(report.Diagnostics)
	if diag.HasErrors(report.Diagnostics) {
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("recipes: %d error(s) in the committed recipe indexes", len(report.Diagnostics)),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// parseRecipeIndex strictly decodes one index and validates every entry. It
// fails closed: an index with an unknown member returns nil.
func parseRecipeIndex(file string, data []byte) (*RecipeIndex, []diag.Diagnostic) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var index RecipeIndex
	if err := decoder.Decode(&index); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRecipeIndex, file, "parse recipe index: %v", err)}
	}
	var findings []diag.Diagnostic
	if index.ProtocolVersion != RecipeIndexProtocolVersion {
		findings = append(findings, diag.Errorf(ErrorCodeInvalidRecipeIndex, file+"#protocolVersion",
			"protocolVersion %d is not supported (want %d)", index.ProtocolVersion, RecipeIndexProtocolVersion))
	}
	seen := make(map[string]int, len(index.Recipes))
	for i, recipe := range index.Recipes {
		field := fmt.Sprintf("%s#recipes[%d]", file, i)
		findings = append(findings, validateRecipe(field, recipe)...)
		key := strings.ToLower(strings.TrimSpace(recipe.Intention))
		if first, duplicate := seen[key]; duplicate && key != "" {
			findings = append(findings, diag.Errorf(ErrorCodeInvalidRecipeIndex, field+".intention",
				"intention %q already appears in recipes[%d]; an intention has exactly one recipe", recipe.Intention, first))
			continue
		}
		seen[key] = i
	}
	return &index, findings
}

func validateRecipe(field string, recipe Recipe) []diag.Diagnostic {
	var findings []diag.Diagnostic
	for member, value := range map[string]string{"intention": recipe.Intention, "since": recipe.Since} {
		findings = append(findings, validateRecipeLine(field+"."+member, value)...)
	}
	if !validRecipeSample(recipe.Sample) {
		findings = append(findings, diag.Errorf(ErrorCodeInvalidRecipeIndex, field+".sample",
			"sample %q must be a clean workspace-relative slash path", recipe.Sample))
	}
	for member, values := range map[string][]string{"primitives": recipe.Primitives, "antiPatterns": recipe.AntiPatterns} {
		if len(values) == 0 {
			findings = append(findings, diag.Errorf(ErrorCodeInvalidRecipeIndex, field+"."+member,
				"%s must list at least one entry", member))
		}
		for i, value := range values {
			findings = append(findings, validateRecipeLine(fmt.Sprintf("%s.%s[%d]", field, member, i), value)...)
		}
	}
	return findings
}

// validRecipeSample reports a clean, workspace-relative slash path that stays
// inside the worktree.
func validRecipeSample(sample string) bool {
	clean := path.Clean(sample)
	return sample != "" && clean == sample && !path.IsAbs(clean) && clean != ".." &&
		!strings.HasPrefix(clean, "../") && !strings.Contains(sample, `\`)
}

func validateRecipeLine(field, value string) []diag.Diagnostic {
	switch {
	case strings.TrimSpace(value) == "":
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRecipeIndex, field, "the value is required")}
	case strings.ContainsAny(value, "\r\n"):
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRecipeIndex, field, "the value must be one line")}
	case len(value) > recipeLineMaxLength:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRecipeIndex, field,
			"the value is %d bytes, beyond the limit of %d", len(value), recipeLineMaxLength)}
	}
	return nil
}

// missingRecipeSamples reports every recipe whose sample is not a directory in
// this worktree. A symlink is not followed: a sample is committed content.
func missingRecipeSamples(tree *worktree, file string, index *RecipeIndex) []diag.Diagnostic {
	var findings []diag.Diagnostic
	for i, recipe := range index.Recipes {
		if !validRecipeSample(recipe.Sample) {
			// validateRecipe already refused it; a path that may leave the
			// worktree is never stat'ed.
			continue
		}
		if tree.isDir(recipe.Sample) {
			continue
		}
		findings = append(findings, diag.Errorf(ErrorCodeRecipeSampleMissing, fmt.Sprintf("%s#recipes[%d].sample", file, i),
			"recipe %q names sample %s, which is not a directory in this worktree", recipe.Intention, recipe.Sample))
	}
	return findings
}
