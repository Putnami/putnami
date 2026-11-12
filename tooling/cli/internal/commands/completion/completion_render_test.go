package completion

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// These tests prove each completion generator on a small catalog the test
// builds, so they do not change when a command of the CLI does. The CLI's own
// catalog is committed once, as tooling/cli/command-surface.json, and
// TestCompletionProjectsEveryCatalogPath proves that completion reaches each
// of its commands.
//
// A test here replaces the catalog for its package, so none of them calls
// t.Parallel.

// useRenderCatalog makes renderCatalog the catalog until the test ends.
func useRenderCatalog(t *testing.T) {
	t.Helper()
	commands, globals := renderCatalog()
	t.Cleanup(commandmeta.ReplaceForTest(commands, globals))
}

// renderCatalog is a catalog of six paths and five global flags that holds
// every case a completion script renders differently:
//
//   - zeta: a root with a value flag whose values are an enum, one of them a
//     word the shell must quote, a boolean flag with a short alias, and an
//     apostrophe in its summary and flag description;
//   - alpha: a root no listing names, with the subcommands list and show;
//   - alpha list: a subcommand with a flag;
//   - beta: an alternate spelling of alpha list (DetailFrom), which completes
//     its flags;
//   - hidden: a root no listing names, which completion still offers.
//
// The global flags hold an unlisted flag (--version), a value flag with
// values, and a flag only extension commands read (--plan).
func renderCatalog() ([]commandmeta.Command, []commandmeta.GlobalFlag) {
	commands := []commandmeta.Command{
		{
			Path:        "zeta",
			Kind:        commandmeta.KindStructured,
			Category:    "Tools",
			Summary:     "Run the project's zeta",
			Description: "Run zeta",
			Usage:       "putnami zeta [--mode <mode>] [-f]",
			Flags: []commandmeta.Flag{
				{Long: "--mode", Type: commandmeta.FlagValue, ValueName: "<mode>", Description: "Run mode", Values: []string{"fast", "two words"}},
				{Long: "--force", Short: "-f", Description: "Skip the project's checks"},
			},
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
			Summary:     "List alphas",
			Description: "List every alpha",
			Usage:       "putnami alpha list [--all-of <kind>]",
			Flags: []commandmeta.Flag{
				{Long: "--all-of", Type: commandmeta.FlagValue, ValueName: "<kind>", Description: "Kind to list"},
			},
		},
		{
			Path:        "alpha show",
			Kind:        commandmeta.KindStructured,
			Summary:     "Show one alpha",
			Description: "Show one alpha",
			Usage:       "putnami alpha show <name>",
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
		{Long: "--output", Type: commandmeta.FlagValue, ValueName: "<format>", Description: "Output format: text | json", Category: "Output", Values: []string{"text", "json"}},
		{Long: "--version", Short: "-V", Description: "Show version"},
	}
	return commands, globals
}

// TestStructuredCommandHelpReadsTheCatalog proves the help record a structured
// path answers with: the catalog entry's prose, usage and flags in declaration
// order, the record of the path an alternate spelling names (beta), and no
// record for a path the catalog does not declare. A caller that sorts the
// flags it gets does not change what the next caller reads.
func TestStructuredCommandHelpReadsTheCatalog(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "each-help-renderer-is-proven-on-a-small-catalog")
	useRenderCatalog(t)

	zeta, ok := StructuredCommandHelp("zeta", "")
	if !ok {
		t.Fatal("StructuredCommandHelp(zeta) found no record")
	}
	want := StructuredCommandInfo{
		Description: "Run zeta",
		Usage:       "putnami zeta [--mode <mode>] [-f]",
		Flags: []StructuredFlagInfo{
			{Long: "--mode", Description: "Run mode", ValueName: "<mode>"},
			{Long: "--force", Short: "-f", Description: "Skip the project's checks"},
		},
	}
	if !reflect.DeepEqual(zeta, want) {
		t.Errorf("StructuredCommandHelp(zeta) = %+v, want %+v", zeta, want)
	}

	zeta.Flags[0], zeta.Flags[1] = zeta.Flags[1], zeta.Flags[0]
	if again, _ := StructuredCommandHelp("zeta", ""); !reflect.DeepEqual(again, want) {
		t.Errorf("a caller reordering its flags changed the next record: %+v", again)
	}

	list, okList := StructuredCommandHelp("alpha", "list")
	beta, okBeta := StructuredCommandHelp("beta", "")
	if !okList || !okBeta || !reflect.DeepEqual(beta, list) {
		t.Errorf("StructuredCommandHelp(beta) = %+v, %t, want the record of alpha list, %+v", beta, okBeta, list)
	}
	if _, ok := StructuredCommandHelp("alpha", "missing"); ok {
		t.Error("StructuredCommandHelp(alpha missing) found a record for a path the catalog does not declare")
	}
}

// renderScripts renders the script a shell gets on renderCatalog, outside a
// workspace and in a workspace whose extension declares the job command build.
func renderScripts(t *testing.T, shell string) (outside, inWorkspace string) {
	t.Helper()
	useRenderCatalog(t)
	generate := completionGenerators[shell]
	var buf bytes.Buffer
	generate(&buf, "", &wsproto.Config{}, nil)
	outside = buf.String()
	buf.Reset()
	generate(&buf, writeExtensionWorkspace(t, "build"), &wsproto.Config{}, nil)
	return outside, buf.String()
}

// assertBlocks fails unless each block's lines appear in the script whole and
// in a row, so a block pins both the rendering and the order of its lines.
func assertBlocks(t *testing.T, name, script string, blocks ...[]string) {
	t.Helper()
	for _, block := range blocks {
		if !strings.Contains("\n"+script, "\n"+strings.Join(block, "\n")+"\n") {
			t.Errorf("%s does not hold these lines in a row:\n%s", name, strings.Join(block, "\n"))
		}
	}
}

// assertNoLine fails when a line of the script contains fragment.
func assertNoLine(t *testing.T, name, script, fragment string) {
	t.Helper()
	for line := range strings.SplitSeq(script, "\n") {
		if strings.Contains(line, fragment) {
			t.Errorf("%s holds %q, which it must not offer: %s", name, fragment, line)
		}
	}
}

func TestBashCompletionRendersTheCatalog(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "each-help-renderer-is-proven-on-a-small-catalog")
	outside, inWorkspace := renderScripts(t, "bash")

	// The lists both renderings share: the subcommands of a root, the paths a
	// subcommand word extends, the roots (hidden too), the flags of each path in
	// sorted order (beta completes the flags of alpha list), and the values of
	// each value flag, quoted where a value is not a plain word.
	for name, script := range map[string]string{"bash outside a workspace": outside, "bash in a workspace": inWorkspace} {
		assertBlocks(t, name, script,
			[]string{"        'alpha') _putnami_words=(list show) ;;"},
			[]string{
				"        'alpha') return 0 ;;",
				"        'alpha list') return 0 ;;",
				"        'alpha show') return 0 ;;",
				"        'beta') return 0 ;;",
				"        'zeta') return 0 ;;",
				"        *) return 1 ;;",
			},
			[]string{
				"        'alpha') return 0 ;;",
				"        'beta') return 0 ;;",
				"        'hidden') return 0 ;;",
				"        'zeta') return 0 ;;",
				"        *) return 1 ;;",
			},
			[]string{
				"        'alpha list') _putnami_words=(--all-of) ;;",
				"        'beta') _putnami_words=(--all-of) ;;",
				"        'zeta') _putnami_words=(--force --mode) ;;",
			},
			[]string{"        '--output') _putnami_words=(text json); return 0 ;;"},
			[]string{
				"        'zeta --mode')",
				"            structured_values=(fast 'two words')",
				"            value_slot=1",
			},
		)
	}

	// Outside a workspace no extension loads, so neither a job command nor
	// --plan, the flag only job commands read, is offered. The unlisted
	// --version is.
	assertBlocks(t, "bash outside a workspace", outside, []string{
		"    local -a commands=(alpha beta hidden zeta)",
		"    local -a structured=(alpha beta hidden zeta)",
		"    local -a global_flags=(--verbose --help --output --version)",
		"    local -a job_global_flags=()",
	})
	assertBlocks(t, "bash in a workspace", inWorkspace, []string{
		"    local -a commands=(alpha beta build hidden zeta)",
		"    local -a structured=(alpha beta hidden zeta)",
		"    local -a global_flags=(--verbose --help --output --version)",
		"    local -a job_global_flags=(--plan)",
	})
}

func TestZshCompletionRendersTheCatalog(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "each-help-renderer-is-proven-on-a-small-catalog")
	outside, inWorkspace := renderScripts(t, "zsh")

	for name, script := range map[string]string{"zsh outside a workspace": outside, "zsh in a workspace": inWorkspace} {
		assertBlocks(t, name, script,
			[]string{
				"        'alpha')",
				"            reply=(",
				"                'list'",
				"                'show'",
				"            )",
			},
			[]string{
				"        'alpha') return 0 ;;",
				"        'alpha list') return 0 ;;",
				"        'alpha show') return 0 ;;",
				"        'beta') return 0 ;;",
				"        'zeta') return 0 ;;",
				"        *) return 1 ;;",
			},
			[]string{
				"        'alpha') return 0 ;;",
				"        'beta') return 0 ;;",
				"        'hidden') return 0 ;;",
				"        'zeta') return 0 ;;",
				"        *) return 1 ;;",
			},
			// A flag spec escapes the apostrophe of its description and lists
			// only the plain-word values, because "(a b)" splits on spaces.
			[]string{
				"        'alpha list')",
				"            reply=(",
				"                '--all-of[Kind to list]:kind:'",
				"            )",
				"            ;;",
				"        'beta')",
				"            reply=(",
				"                '--all-of[Kind to list]:kind:'",
				"            )",
				"            ;;",
				"        'zeta')",
				"            reply=(",
				`                '--force[Skip the project'\''s checks]'`,
				"                '--mode[Run mode]:mode:(fast)'",
				"            )",
			},
			[]string{"        '--output') reply=(text json); return 0 ;;"},
			// The word after the flag offers every value, the quoted one too.
			[]string{
				"                    'zeta --mode')",
				"                        cmd_values=(",
				"                            'fast'",
				"                            'two words'",
				"                        )",
			},
			[]string{
				"    structured_commands=(",
				"        'alpha:Manage alphas'",
				"        'beta:List alphas under another name'",
				"        'hidden:Run the hidden command'",
				`        'zeta:Run the project'\''s zeta'`,
				"    )",
			},
			[]string{
				"    global_flags=(",
				"        '--verbose[Show more]'",
				"        '--help[Show help]'",
				"        '--output[Output format: text | json]:format:(text json)'",
				"        '--version[Show version]'",
				"    )",
			},
		)
	}

	assertBlocks(t, "zsh outside a workspace", outside,
		[]string{"    commands=(", "    )"},
		[]string{"    job_global_flags=(", "    )"},
	)
	assertBlocks(t, "zsh in a workspace", inWorkspace,
		[]string{"    commands=(", "        'build:Run.'", "    )"},
		[]string{"    job_global_flags=(", "        '--plan[Show the plan]'", "    )"},
	)
}

func TestFishCompletionRendersTheCatalog(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-help", "each-help-renderer-is-proven-on-a-small-catalog")
	outside, inWorkspace := renderScripts(t, "fish")

	for name, script := range map[string]string{"fish outside a workspace": outside, "fish in a workspace": inWorkspace} {
		assertBlocks(t, name, script,
			[]string{"    contains -- $tokens[2] 'alpha' 'beta' 'hidden' 'zeta'"},
			[]string{
				"complete -c putnami -n '__putnami_use_command' -a 'alpha' -d 'Manage alphas'",
				"complete -c putnami -n '__putnami_use_command' -a 'beta' -d 'List alphas under another name'",
				"complete -c putnami -n '__putnami_use_command' -a 'hidden' -d 'Run the hidden command'",
				`complete -c putnami -n '__putnami_use_command' -a 'zeta' -d 'Run the project\'s zeta'`,
			},
			[]string{
				"complete -c putnami -n '__putnami_complete_children alpha' -a 'list'",
				"complete -c putnami -n '__putnami_complete_children alpha' -a 'show'",
			},
			[]string{
				"complete -c putnami -n '__putnami_help_word' -a 'alpha'",
				"complete -c putnami -n '__putnami_help_word' -a 'beta'",
				"complete -c putnami -n '__putnami_help_word' -a 'hidden'",
				"complete -c putnami -n '__putnami_help_word' -a 'zeta'",
				"complete -c putnami -n '__putnami_help_word alpha' -a 'list'",
				"complete -c putnami -n '__putnami_help_word alpha' -a 'show'",
			},
			[]string{
				"complete -c putnami -n '__putnami_seen_path alpha list' -l 'all-of' -d 'Kind to list' -x",
				"complete -c putnami -n '__putnami_seen_path beta' -l 'all-of' -d 'Kind to list' -x",
				`complete -c putnami -n '__putnami_seen_path zeta' -l 'force' -d 'Skip the project\'s checks'`,
				`complete -c putnami -n '__putnami_seen_path zeta' -l 'mode' -d 'Run mode' -a 'fast \'two words\'' -x`,
			},
		)
	}

	assertBlocks(t, "fish outside a workspace", outside, []string{
		"complete -c putnami -n '__putnami_no_leading_flag' -l 'verbose' -d 'Show more'",
		"complete -c putnami -n '__putnami_no_leading_flag' -l 'help' -d 'Show help'",
		"complete -c putnami -n '__putnami_no_leading_flag' -l 'output' -d 'Output format: text | json' -x -a 'text json'",
		"complete -c putnami -n '__putnami_no_leading_flag' -l 'version' -d 'Show version'",
	})
	assertNoLine(t, "fish outside a workspace", outside, "-a 'build'")
	assertBlocks(t, "fish in a workspace", inWorkspace,
		[]string{"complete -c putnami -n '__putnami_use_command' -a 'build' -d 'Run.'"},
		[]string{
			"complete -c putnami -n '__putnami_no_leading_flag' -l 'verbose' -d 'Show more'",
			"complete -c putnami -n '__putnami_job_flag' -l 'plan' -d 'Show the plan'",
			"complete -c putnami -n '__putnami_no_leading_flag' -l 'help' -d 'Show help'",
			"complete -c putnami -n '__putnami_no_leading_flag' -l 'output' -d 'Output format: text | json' -x -a 'text json'",
			"complete -c putnami -n '__putnami_no_leading_flag' -l 'version' -d 'Show version'",
		},
	)
}
