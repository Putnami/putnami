package agentctx

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// recipePlaceholder is replaced with the line that names the service recipe
// of the workspace's languages, or with nothing.
const recipePlaceholder = "{{RECIPE}}"

// serviceRecipe is the public page that builds an HTTP API with Postgres in
// one language, from the template to a running image. The page lives in
// sites/putnami.dev/doc/03-how-to, where the documentation examples check
// its code blocks.
type serviceRecipe struct {
	extension string
	language  string
	url       string
}

// serviceRecipes are the recipes, in the order the line names them.
var serviceRecipes = []serviceRecipe{
	{extension: "@putnami/go", language: "Go", url: "https://putnami.dev/docs/how-to/build-a-postgres-api-in-go"},
	{extension: "@putnami/typescript", language: "TypeScript", url: "https://putnami.dev/docs/how-to/build-a-postgres-api-in-typescript"},
}

// recipeGuidanceForWorkspace is the recipe line for the language extensions
// the workspace loads. A workspace that loads neither renders no line, so its
// block stays the block it had before the line existed.
func recipeGuidanceForWorkspace(wsRoot string) string {
	return recipeGuidance(loadedExtensions(wsRoot))
}

// recipeGuidance is the recipe line for the loaded extensions.
func recipeGuidance(extensions []*extension.ExtensionDescription) string {
	loaded := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		if ext != nil {
			loaded[ext.Name] = true
		}
	}
	return recipeLine(loaded)
}

// recipeLine renders the line for the loaded extensions, by name.
func recipeLine(loaded map[string]bool) string {
	var links []string
	for _, recipe := range serviceRecipes {
		if loaded[recipe.extension] {
			links = append(links, recipe.url+" ("+recipe.language+")")
		}
	}
	if len(links) == 0 {
		return ""
	}
	return "- To build an HTTP API with Postgres, follow " + strings.Join(links, " or ") +
		": the template, the framework docs, the database, migrations, tests and the Docker image.\n"
}

// applyRecipe substitutes the recipe line into a guidance template.
func applyRecipe(template string, recipe string) string {
	return strings.ReplaceAll(template, recipePlaceholder, recipe)
}
