package cli

import (
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// These tests prove each help renderer on a small catalog the test builds, so
// they do not change when a command of the CLI does. The CLI's own catalog is
// committed once, as command-surface.json (TestGoldenCommandSurfaceDocument),
// and TestEveryHelpSurfaceCoversEveryCatalogCommand proves that every renderer
// reaches each of its commands.
//
// A test here replaces the catalog for its package, so none of them calls
// t.Parallel.

// renderVersion replaces the linker-stamped CLI version while a renderer test
// runs, so the expected bytes do not depend on how the test binary was built.
const renderVersion = "0.0.0-test"

// useRenderCatalog makes renderCatalog the catalog and pins the CLI version
// until the test ends.
func useRenderCatalog(t *testing.T) {
	t.Helper()
	commands, globals := renderCatalog()
	t.Cleanup(commandmeta.ReplaceForTest(commands, globals))
	original := Version
	Version = renderVersion
	t.Cleanup(func() { Version = original })
}

// renderCatalog is a catalog of six paths and five global flags that holds
// every case a help surface renders differently:
//
//   - zeta: a listed command with a required positional, a value flag with an
//     enum, a boolean flag with a short alias, an apostrophe and a pipe in its
//     prose, examples, and related commands, one of them a job command;
//   - alpha: a bare root no listing names, whose default subcommand is list;
//   - alpha list: a listed subcommand under a Label;
//   - alpha show: an unlisted subcommand with a positional;
//   - beta: an alternate spelling of alpha list (DetailFrom), listed in a
//     second category;
//   - hidden: a documented command no listing names.
//
// The global flags hold two categories, an unlisted flag (--version), a value
// flag with values, and a flag only extension commands read (--plan).
func renderCatalog() ([]commandmeta.Command, []commandmeta.GlobalFlag) {
	commands := []commandmeta.Command{
		{
			Path:        "zeta",
			Kind:        commandmeta.KindStructured,
			Category:    "Tools",
			Summary:     "Run zeta on a target",
			Description: "Run zeta on the project's target; a | in prose stays as written",
			Usage:       "putnami zeta <target> [--mode <fast|safe>] [-f]",
			Positionals: []commandmeta.Positional{{Name: "target", Required: true}},
			Flags: []commandmeta.Flag{
				{Long: "--mode", Type: commandmeta.FlagValue, ValueName: "<mode>", Description: "Run mode, fast or safe", Values: []string{"fast", "safe"}},
				{Long: "--force", Short: "-f", Description: "Skip the project's checks."},
			},
			Examples: []string{"putnami zeta app --mode fast", "putnami zeta app -f"},
			Related:  []string{"alpha list", "build"},
		},
		{
			Path:       "alpha",
			Kind:       commandmeta.KindStructured,
			Summary:    "Manage alphas",
			DefaultSub: "list",
		},
		{
			Path:        "alpha list",
			Kind:        commandmeta.KindStructured,
			Category:    "Tools",
			Label:       "alpha [list]",
			Summary:     "List alphas",
			Description: "List every alpha",
			Usage:       "putnami alpha list",
		},
		{
			Path:        "alpha show",
			Kind:        commandmeta.KindStructured,
			Summary:     "Show one alpha",
			Description: "Show one alpha.",
			Usage:       "putnami alpha show <name>",
			Positionals: []commandmeta.Positional{{Name: "name", Required: true}},
		},
		{
			Path:       "beta",
			Kind:       commandmeta.KindStructured,
			Category:   "Other",
			Summary:    "List alphas under another name",
			DetailFrom: "alpha list",
		},
		{
			Path:        "hidden",
			Kind:        commandmeta.KindStructured,
			Summary:     "Run the hidden command",
			Description: "Run a command no listing names",
			Usage:       "putnami hidden",
		},
	}
	globals := []commandmeta.GlobalFlag{
		{Long: "--verbose", Short: "-v", Description: "Show more", Category: "Common"},
		{Long: "--plan", Description: "Show the plan", Category: "Common", JobOnly: true},
		{Long: "--help", Short: "-h", Description: "Show help", Category: "Common"},
		{Long: "--output", Type: commandmeta.FlagValue, ValueName: "<format>", Description: "Output format, text or json", Category: "Output", Values: []string{"text", "json"}},
		{Long: "--version", Short: "-V", Description: "Show version"},
	}
	return commands, globals
}

// renderScope is the help scope of a workspace whose one extension declares
// the job commands build and deploy.
func renderScope() *helpScope {
	return helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{
		{Name: "@acme/lang", Commands: map[string]string{"build": "Build the project.", "deploy": "Deploy the project."}},
	}}, nil)
}

// assertRendered fails with the first differing line when got is not want.
func assertRendered(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n%s\ngot:\n%s", what, firstGoldenDiff(want, got), got)
	}
}

// renderedSection returns the part of text from the line start up to, not
// including, the line end.
func renderedSection(t *testing.T, text, start, end string) string {
	t.Helper()
	from := strings.Index(text, start)
	if from < 0 {
		t.Fatalf("the rendering has no %q:\n%s", start, text)
	}
	to := strings.Index(text[from:], end)
	if to < 0 {
		t.Fatalf("the rendering has no %q after %q:\n%s", end, start, text)
	}
	return text[from : from+to]
}

var wantTopLevelHelp = strings.Join([]string{
	"putnami 0.0.0-test — polyglot build orchestrator",
	"",
	"Usage:",
	"  putnami <command[,command...]> [options]",
	"",
	"Build (from extensions):",
	"  build, deploy",
	"  Multi-command: putnami <command>,<command> --impacted",
	"",
	"Tools:",
	"  zeta                         Run zeta on a target",
	"  alpha [list]                 List alphas",
	"",
	"Other:",
	"  beta                         List alphas under another name",
	"",
	"Common:",
	"  -v, --verbose                Show more",
	"  --plan                       Show the plan",
	"  -h, --help                   Show help",
	"",
	"Output:",
	"  --output <format>            Output format, text or json",
	"",
	"Default Selection:",
	"  no target            Feature branch: impacted vs trunk; main/master: local marker, then remote cache marker when active, otherwise all",
	"",
	"Positional:",
	"  .                    Current directory scope (project or subtree)",
	"",
	"Help:",
	"  putnami help <command>   Command help",
	"  putnami help --man       Man page format",
	"  putnami help --markdown  Markdown format",
	"",
	"",
}, "\n")

// The top-level help lists the job commands, then each catalog category in
// first-appearance order with its rows in catalog order under their Label,
// then each listed global-flag category. A command and a global flag no
// category names are not listed.
func testTopLevelHelpRendersTheCatalog(t *testing.T) {
	useRenderCatalog(t)

	assertRendered(t, "top-level help of a workspace", renderHelp(renderScope()), wantTopLevelHelp)

	// Outside a workspace no extension is loaded: help says so, and drops the
	// flag only extension commands read.
	want := strings.Replace(wantTopLevelHelp,
		"  build, deploy\n  Multi-command: putnami <command>,<command> --impacted\n",
		"  No extension is loaded. Name a language extension in\n  putnami.workspace.json, then run: putnami extensions install\n", 1)
	want = strings.Replace(want, "  --plan                       Show the plan\n", "", 1)
	assertRendered(t, "top-level help outside a workspace", renderHelp(nil), want)
}

var wantZetaHelp = strings.Join([]string{
	"putnami zeta",
	"",
	"Run zeta on the project's target; a | in prose stays as written",
	"",
	"Usage:",
	"  putnami zeta <target> [--mode <fast|safe>] [-f]",
	"",
	"Command Flags:",
	"  --mode <mode>                Run mode, fast or safe",
	"  --force                      Skip the project's checks.",
	"",
	"Examples:",
	"  putnami zeta app --mode fast",
	"  putnami zeta app -f",
	"",
	"Global Options:",
	"  Common: --verbose, --help",
	"  Output: --output",
	"",
	"Related Commands:",
	"  putnami alpha list",
	"  putnami build",
	"",
	"",
}, "\n")

var wantAlphaHelp = strings.Join([]string{
	"putnami alpha",
	"",
	"Subcommands:",
	"  list  List alphas (default)",
	"  show  Show one alpha",
	"",
	"Global Options:",
	"  Common: --verbose, --help",
	"  Output: --output",
	"",
	"",
}, "\n")

// The help of a core command renders its description, usage line, flags with
// their value names, examples, the global flags it accepts, and the related
// commands the scope lists. A root lists its subcommands in catalog order and
// marks its default one. An alternate spelling renders the help it shares.
func testCommandHelpRendersTheCatalogEntry(t *testing.T) {
	useRenderCatalog(t)

	zeta := captureStructuredHelp(t, func() { printSubcommandHelp("zeta", "", renderScope()) })
	assertRendered(t, "help of zeta in a workspace", zeta, wantZetaHelp)

	// Outside a workspace no extension declares build, so help does not
	// suggest it.
	zeta = captureStructuredHelp(t, func() { printSubcommandHelp("zeta", "", nil) })
	assertRendered(t, "help of zeta outside a workspace", zeta, strings.Replace(wantZetaHelp, "  putnami build\n", "", 1))

	alpha := captureStructuredHelp(t, func() { printSubcommandHelp("alpha", "", nil) })
	assertRendered(t, "help of the root alpha", alpha, wantAlphaHelp)

	list := captureStructuredHelp(t, func() { printSubcommandHelp("alpha", "list", nil) })
	beta := captureStructuredHelp(t, func() { printSubcommandHelp("beta", "", nil) })
	if want := strings.Replace(list, "putnami alpha list\n\n", "putnami beta\n\n", 1); beta != want {
		t.Errorf("the alternate spelling beta does not render the help of alpha list:\n%s", firstGoldenDiff(want, beta))
	}
}

// The help of a job command lists the flags its extension declares, sorted,
// with their short alias and default, then the global flags and the related
// commands its verb names.
func testJobCommandHelpMergesExtensionFlags(t *testing.T) {
	useRenderCatalog(t)

	flags := map[string]extension.FlagDefinition{
		"environment":  {Type: "string", Description: "Deployment environment"},
		"confirm":      {Type: "boolean", Short: "-c", Description: "Confirm deployment"},
		"replicas":     {Type: "number", Description: "Replica count", Default: 3},
		"undocumented": {Type: "boolean"},
	}
	dev := &extension.ExtensionDescription{Name: "@acme/dev", Verbs: []extension.VerbDefinition{
		{Name: "build", Description: "Build the project."},
		{Name: "package", Description: "Package the project."},
		{Name: "publish", Description: "Publish the project."},
		{Name: "deploy", Description: "Deploy the project.", Related: []string{"build", "package", "publish"}},
	}}
	lang := &extension.ExtensionDescription{Name: "@acme/lang", Commands: map[string]string{
		"build": "", "package": "", "publish": "", "deploy": "",
	}}
	scope := helpScopeOf(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{lang, dev}}, nil)

	got := captureStructuredHelp(t, func() { printCommandHelp("deploy", flags, scope) })
	assertRendered(t, "help of the job command deploy", got, strings.Join([]string{
		"putnami deploy",
		"",
		"Command Flags:",
		"  -c, --confirm  Confirm deployment",
		"  --environment  Deployment environment",
		"  --replicas  Replica count (default: 3)",
		"  --undocumented  undocumented",
		"",
		"Global Options:",
		"  Common: --verbose, --plan, --help",
		"  Output: --output",
		"",
		"Related Commands:",
		"  putnami build",
		"  putnami package",
		"  putnami publish",
		"",
		"",
	}, "\n"))
}

var wantManJobsAndFlags = strings.Join([]string{
	".SH JOB COMMANDS",
	".TP",
	".B build",
	"Build the project.",
	".TP",
	".B deploy",
	"Deploy the project.",
	".SH COMMON OPTIONS",
	".TP",
	".BR -v \" , \" --verbose",
	"Show more",
	".TP",
	".B --plan",
	"Show the plan",
	".TP",
	".BR -h \" , \" --help",
	"Show help",
	".SH OUTPUT OPTIONS",
	".TP",
	".B --output \" <format>\"",
	"Output format, text or json",
	"",
}, "\n")

var wantManCommands = strings.Join([]string{
	".SH STRUCTURED COMMANDS",
	".SS Tools",
	".TP",
	".B zeta",
	"Run zeta on a target.",
	".TP",
	".B alpha [list]",
	"List alphas.",
	".SS Other",
	".TP",
	".B beta",
	"List alphas under another name.",
	".SH COMMAND REFERENCE",
	".SS zeta",
	"Run zeta on the project's target; a | in prose stays as written.",
	".PP",
	".B Usage:",
	".br",
	"putnami zeta <target> [--mode <fast|safe>] [-f]",
	".TP",
	".B --mode",
	"Run mode, fast or safe.",
	".TP",
	".B --force",
	"Skip the project's checks.",
	".SS alpha list",
	"List every alpha.",
	".PP",
	".B Usage:",
	".br",
	"putnami alpha list",
	".SS alpha show",
	"Show one alpha.",
	".PP",
	".B Usage:",
	".br",
	"putnami alpha show <name>",
	".SS hidden",
	"Run a command no listing names.",
	".PP",
	".B Usage:",
	".br",
	"putnami hidden",
	"",
}, "\n")

// The man page escapes its own dashes, renders a short alias and a value name
// as roff arguments, ends each catalog sentence with one period, lists every
// category row, and gives each documented command one reference section: an
// alternate spelling shares the section of the path it follows.
func testManPageRendersTheCatalog(t *testing.T) {
	useRenderCatalog(t)

	man := captureStructuredHelp(t, func() { printHelpMan(os.Stdout, renderScope()) })
	if want := ".TH PUTNAMI 1 \"\" \"putnami 0.0.0-test\" \"User Commands\"\n.SH NAME\nputnami \\- polyglot build orchestrator\n"; !strings.HasPrefix(man, want) {
		t.Errorf("the man page does not start with %q:\n%s", want, man)
	}
	assertRendered(t, "man page job commands and options",
		renderedSection(t, man, ".SH JOB COMMANDS\n", ".SH DEFAULT PROJECT SELECTION\n"), wantManJobsAndFlags)
	assertRendered(t, "man page commands",
		renderedSection(t, man, ".SH STRUCTURED COMMANDS\n", ".SH EXIT CODES\n"), wantManCommands)
	if strings.Contains(man, ".SH ALIASES") {
		t.Errorf("the man page of a scope without an alias has an ALIASES section:\n%s", man)
	}
}

var wantMarkdownJobsAndFlags = strings.Join([]string{
	"## Build Commands",
	"",
	"| Command | Description |",
	"|---------|-------------|",
	"| `putnami build` | Build the project |",
	"| `putnami deploy` | Deploy the project |",
	"",
	"Multi-command: `putnami <command>,<command> --impacted`",
	"",
	"## Common Options",
	"",
	"| Flag | Description |",
	"|------|-------------|",
	"| `--verbose`, `-v` | Show more |",
	"| `--plan` | Show the plan |",
	"| `--help`, `-h` | Show help |",
	"",
	"## Output Options",
	"",
	"| Flag | Description |",
	"|------|-------------|",
	"| `--output` <format> | Output format, text or json |",
	"",
	"",
}, "\n")

var wantMarkdownCommands = strings.Join([]string{
	"## Tools",
	"",
	"| Command | Description |",
	"|---------|-------------|",
	"| `putnami zeta` | Run zeta on a target |",
	"| `putnami alpha [list]` | List alphas |",
	"",
	"## Other",
	"",
	"| Command | Description |",
	"|---------|-------------|",
	"| `putnami beta` | List alphas under another name |",
	"",
	"## Command Reference",
	"",
	"### `putnami zeta`",
	"",
	"Run zeta on the project's target; a | in prose stays as written",
	"",
	"**Usage**",
	"",
	"```",
	"putnami zeta <target> [--mode <fast|safe>] [-f]",
	"```",
	"",
	"**Flags**",
	"",
	"| Flag | Description |",
	"|------|-------------|",
	"| `--mode` <mode> | Run mode, fast or safe |",
	"| `--force` | Skip the project's checks. |",
	"",
	"**Examples**",
	"",
	"- `putnami zeta app --mode fast`",
	"- `putnami zeta app -f`",
	"",
	"### `putnami alpha list`",
	"",
	"List every alpha",
	"",
	"**Usage**",
	"",
	"```",
	"putnami alpha list",
	"```",
	"",
	"### `putnami alpha show`",
	"",
	"Show one alpha.",
	"",
	"**Usage**",
	"",
	"```",
	"putnami alpha show <name>",
	"```",
	"",
	"### `putnami hidden`",
	"",
	"Run a command no listing names",
	"",
	"**Usage**",
	"",
	"```",
	"putnami hidden",
	"```",
	"",
	"",
}, "\n")

// The Markdown reference puts every command and flag spelling in a code span,
// its usage line in a code block, lists every category row, and gives each
// documented command one reference section: an alternate spelling shares the
// section of the path it follows.
func testMarkdownReferenceRendersTheCatalog(t *testing.T) {
	useRenderCatalog(t)

	markdown := captureStructuredHelp(t, func() { printHelpMarkdown(os.Stdout, renderScope()) })
	if want := "# putnami\n\n> polyglot build orchestrator — v0.0.0-test\n\n"; !strings.HasPrefix(markdown, want) {
		t.Errorf("the Markdown reference does not start with %q:\n%s", want, markdown)
	}
	assertRendered(t, "Markdown job commands and options",
		renderedSection(t, markdown, "## Build Commands\n", "## Default Project Selection\n"), wantMarkdownJobsAndFlags)
	assertRendered(t, "Markdown commands",
		renderedSection(t, markdown, "## Tools\n", "## Exit Codes\n"), wantMarkdownCommands)
}

// Each help renderer renders a small catalog the test builds. The renderers
// read the catalog the package holds, which a subtest replaces, so they run
// one after another in one serial test.
func TestHelpRenderersRenderASmallCatalog(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "each-help-renderer-is-proven-on-a-small-catalog")
	t.Run("top-level help", testTopLevelHelpRendersTheCatalog)
	t.Run("command help", testCommandHelpRendersTheCatalogEntry)
	t.Run("job command help", testJobCommandHelpMergesExtensionFlags)
	t.Run("man page", testManPageRendersTheCatalog)
	t.Run("Markdown reference", testMarkdownReferenceRendersTheCatalog)
}

// Every help surface reaches each command of the CLI's own catalog: the
// top-level help, the man page and the Markdown reference list each category
// row, the man page and the Markdown reference give each documented command a
// section, and the help of each path renders its usage line and, for a root,
// its subcommands.
func TestEveryHelpSurfaceCoversEveryCatalogCommand(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "every-help-surface-covers-every-catalog-command")

	top := renderHelp(nil)
	man := captureStructuredHelp(t, func() { printHelpMan(os.Stdout, nil) })
	markdown := captureStructuredHelp(t, func() { printHelpMarkdown(os.Stdout, nil) })

	rows := 0
	for _, category := range commandmeta.Categories() {
		for _, command := range category.Commands {
			rows++
			name := command.DisplayName()
			if !strings.Contains(top, "\n  "+name+" ") {
				t.Errorf("the top-level help does not list %q", name)
			}
			if !strings.Contains(man, "\n.B "+name+"\n") {
				t.Errorf("the man page does not list %q", name)
			}
			if !strings.Contains(markdown, "| "+markdownCell("`putnami "+name+"`")+" |") {
				t.Errorf("the Markdown reference does not list %q", name)
			}
		}
	}
	if rows == 0 {
		t.Fatal("the catalog lists no command: the walk is wrong")
	}

	// Every table of the reference has two columns, so a row holds exactly
	// three unescaped pipes; a pipe a cell leaves unescaped adds a column.
	for _, line := range strings.Split(markdown, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if separators := strings.Count(strings.ReplaceAll(line, `\|`, ""), "|"); separators != 3 {
			t.Errorf("the Markdown reference row %q has %d cells, want 2", line, separators-1)
		}
	}

	for _, command := range commandmeta.ReferenceCommands() {
		if !strings.Contains(man, "\n.SS "+command.Path+"\n") {
			t.Errorf("the man page has no section for %q", command.Path)
		}
		if !strings.Contains(markdown, "\n### `putnami "+command.Path+"`\n") {
			t.Errorf("the Markdown reference has no section for %q", command.Path)
		}
	}

	for _, command := range commandmeta.Commands() {
		root, sub, _ := strings.Cut(command.Path, " ")
		help := captureStructuredHelp(t, func() { printSubcommandHelp(root, sub, nil) })
		if !strings.HasPrefix(help, "putnami "+command.Path+"\n") {
			t.Errorf("the help of %q does not start with its path:\n%s", command.Path, help)
		}
		if detail, ok := commandmeta.Detail(command.Path); ok && detail.Usage != "" &&
			!strings.Contains(help, "Usage:\n  "+detail.Usage+"\n") {
			t.Errorf("the help of %q does not render its usage line %q", command.Path, detail.Usage)
		}
		if sub != "" {
			continue
		}
		for _, child := range commandmeta.Subcommands(root) {
			if !strings.Contains(help, "\n  "+child) {
				t.Errorf("the help of %q does not list its subcommand %q", root, child)
			}
		}
	}
}
