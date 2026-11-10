package lifecycle

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// initLanguagesJSON is the language index `putnami init` reads before it has
// installed anything: a copy of the workspace adapter of every language
// extension this repository ships, generated from their manifests and held to
// them by TestInitLanguagesFollowTheExtensionManifests (ADR 0086).
//
//go:embed init_languages.json
var initLanguagesJSON []byte

// initLanguage is one language init declares: the extension that runs it, the
// file patterns that mark one of its projects, and the directories its scan
// skips, as that extension's workspace adapter states them.
type initLanguage struct {
	Language  string   `json:"language"`
	Extension string   `json:"extension"`
	Markers   []string `json:"markers"`
	Excludes  []string `json:"excludes,omitempty"`
}

// initLanguageIndex is the shape of init_languages.json.
type initLanguageIndex struct {
	Comment   string         `json:"$comment,omitempty"`
	Languages []initLanguage `json:"languages"`
}

// initLanguages are the languages of the embedded index, sorted by language.
var initLanguages = decodeInitLanguages(initLanguagesJSON)

func decodeInitLanguages(data []byte) []initLanguage {
	var index initLanguageIndex
	if err := json.Unmarshal(data, &index); err != nil {
		panic(fmt.Sprintf("init_languages.json: %v", err))
	}
	return index.Languages
}

// initDefaultLanguage is the language init declares when nothing chose one.
const initDefaultLanguage = "typescript"

// initLanguageShorthands are the short names --extension accepts for two
// languages. No other language has one: a language is named by its id.
var initLanguageShorthands = map[string]string{"ts": "typescript", "py": "python"}

// starterNPMRegistries maps the @putnami npm scope to the Putnami package
// registry, which serves the TypeScript framework packages the TypeScript
// templates depend on.
var starterNPMRegistries = map[string]json.RawMessage{
	"npm": json.RawMessage(`{"scopes":{"@putnami":"https://npm.putnami.dev"}}`),
}

// initStarters are the starters of the languages that ship one, by language:
// the template --project renders and the registries its dependencies install
// from. Which template greets a new user is init's choice, not language
// knowledge, so it lives here; TestInitStartersNameTheirExtension holds each
// one to its language's extension and template.
var initStarters = map[string]initExtensionConfig{
	"typescript": {packageName: "@putnami/typescript", template: "typescript-web", registries: starterNPMRegistries},
	"go":         {packageName: "@putnami/go", template: "go-server"},
	"python":     {packageName: "@putnami/python", template: "python-server"},
}

// findInitLanguage returns the language of the index a --extension item names:
// its language id, or one of the shorthands.
func findInitLanguage(name string) (initLanguage, bool) {
	if language, ok := initLanguageShorthands[name]; ok {
		name = language
	}
	for _, language := range initLanguages {
		if language.Language == name {
			return language, true
		}
	}
	return initLanguage{}, false
}

// initLanguageNames lists the languages of the index, sorted.
func initLanguageNames() string {
	names := make([]string, 0, len(initLanguages))
	for _, language := range initLanguages {
		names = append(names, language.Language)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// initStarterLanguages lists the languages that ship a starter, sorted.
func initStarterLanguages() string {
	names := make([]string, 0, len(initStarters))
	for name := range initStarters {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// parseInitLanguages reads the value of --extension: one language or a
// comma-separated list of them, each declared once, in the order named.
func parseInitLanguages(value string) ([]initLanguage, error) {
	if strings.TrimSpace(value) == "" {
		return nil, cmderr.Usagef("--extension needs a language (choose: %s)", initLanguageNames())
	}
	var languages []initLanguage
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			return nil, cmderr.Usagef("--extension %q names an empty language: separate languages with one comma (choose: %s)", value, initLanguageNames())
		}
		language, ok := findInitLanguage(name)
		if !ok {
			return nil, cmderr.Usagef("unknown language %q for --extension (choose: %s; ts and py name typescript and python)", name, initLanguageNames())
		}
		if seen[language.Language] {
			continue
		}
		seen[language.Language] = true
		languages = append(languages, language)
	}
	return languages, nil
}

// initConfigFor is what init declares for a language: its starter when it
// ships one, else its extension alone.
func initConfigFor(language initLanguage) initExtensionConfig {
	if starter, ok := initStarters[language.Language]; ok {
		return starter
	}
	return initExtensionConfig{packageName: language.Extension}
}

// initLanguageList joins the ids of languages with commas, the form
// --extension takes.
func initLanguageList(languages []initLanguage) string {
	names := make([]string, len(languages))
	for i, language := range languages {
		names[i] = language.Language
	}
	return strings.Join(names, ",")
}

// initDetection is a language whose marker init found, and the first
// directory that holds one, relative to the directory init runs in.
type initDetection struct {
	language initLanguage
	dir      string
}

// detectInitLanguages walks root for the markers of languages
// (workspace.DetectMarkers) and returns the languages it found, in the order
// given.
func detectInitLanguages(root string, languages []initLanguage) []initDetection {
	scopes := make([]workspace.ProviderScope, 0, len(languages))
	matched := make([]initLanguage, 0, len(languages))
	for _, language := range languages {
		scope, ok := workspace.NewProviderScope(language.Extension, &extensionproto.WorkspaceAdapter{
			Markers:  language.Markers,
			Inputs:   language.Markers,
			Excludes: language.Excludes,
		})
		if !ok {
			continue
		}
		scopes = append(scopes, scope)
		matched = append(matched, language)
	}
	var detected []initDetection
	for i, dir := range workspace.DetectMarkers(root, scopes) {
		if dir != "" {
			detected = append(detected, initDetection{language: matched[i], dir: dir})
		}
	}
	return detected
}

// detectedLanguages are the languages of detections.
func detectedLanguages(detected []initDetection) []initLanguage {
	out := make([]initLanguage, len(detected))
	for i, detection := range detected {
		out[i] = detection.language
	}
	return out
}

// where names the directory of a detection the way init prints it.
func (d initDetection) where() string {
	if d.dir == "." {
		return "at the root"
	}
	return "in " + d.dir
}

// detectInitSources returns the tools --from accepts whose markers root
// holds, sorted, each with the first of its markers present.
func detectInitSources(root string) [][2]string {
	var found [][2]string
	for name, source := range initSources {
		for _, marker := range source.markers {
			if holdsAny(root, []string{marker}) {
				found = append(found, [2]string{name, marker})
				break
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i][0] < found[j][0] })
	return found
}

// proposeInitLanguages is init without --extension, --from or --project
// (D-067). A monorepo tool marker at the root fails with a usage error that
// points at init --from. Languages whose markers root holds are asked in a
// terminal, all preselected, and fail with a usage error that names the
// --extension command without one. No marker returns nil: the default stands.
func proposeInitLanguages(root string) ([]initLanguage, error) {
	if tools := detectInitSources(root); len(tools) > 0 {
		found := make([]string, len(tools))
		for i, tool := range tools {
			found[i] = fmt.Sprintf("%s (%s)", tool[1], tool[0])
		}
		return nil, protocolcli.WithNext(
			cmderr.Usagef("%s holds %s, the root of a monorepo tool's repository: adopt it with putnami init --from <tool>; init declares nothing on its own",
				root, strings.Join(found, " and ")),
			"putnami init --from "+tools[0][0],
		)
	}
	detected := detectInitLanguages(root, initLanguages)
	if len(detected) == 0 {
		return nil, nil
	}
	if !initIsTerminal() {
		found := make([]string, len(detected))
		for i, detection := range detected {
			found[i] = detection.language.Language + " " + detection.where()
		}
		return nil, protocolcli.WithNext(
			cmderr.Usagef("%s holds the markers of %s: without a terminal init cannot ask which to declare, so it declares nothing",
				root, strings.Join(found, ", ")),
			"putnami init --extension "+initLanguageList(detectedLanguages(detected)),
		)
	}
	return askInitLanguages(detected, initPromptInput, os.Stdout)
}

// askInitLanguages asks which detected languages to declare, every one
// preselected: an empty answer declares them all. An answer lists numbers of
// the list or languages of the index, separated by commas or spaces.
func askInitLanguages(detected []initDetection, in io.Reader, out io.Writer) ([]initLanguage, error) {
	all := detectedLanguages(detected)
	reader := bufio.NewReader(in)
	iox.Fprintf(out, "\n  This directory holds the markers of %d language(s):\n", len(detected))
	preselected := make([]string, len(detected))
	for i, detection := range detected {
		iox.Fprintf(out, "    %d) %-11s %-20s %s\n", i+1, detection.language.Language, detection.language.Extension, detection.where())
		preselected[i] = strconv.Itoa(i + 1)
	}
	iox.Fprintln(out, "  Declare which? Press Enter for all, or list numbers or languages.")
	for range 3 {
		iox.Fprintf(out, "  Languages [%s]: ", strings.Join(preselected, ","))
		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			if err != nil && err != io.EOF {
				return nil, err
			}
			return all, nil
		}
		if chosen, ok := initAnswerLanguages(answer, all); ok {
			return chosen, nil
		}
		if err != nil {
			break
		}
		iox.Fprintf(out, "  Answer with numbers from 1 to %d or languages (%s).\n", len(detected), initLanguageNames())
	}
	return nil, cmderr.Usagef("no language chosen: rerun with --extension %s", initLanguageList(all))
}

// initAnswerLanguages reads an answer to the language question: numbers of
// the detected list, or languages of the index, each declared once.
func initAnswerLanguages(answer string, detected []initLanguage) ([]initLanguage, bool) {
	fields := strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	var chosen []initLanguage
	seen := map[string]bool{}
	for _, field := range fields {
		var language initLanguage
		if n, err := strconv.Atoi(field); err == nil {
			if n < 1 || n > len(detected) {
				return nil, false
			}
			language = detected[n-1]
		} else {
			found, ok := findInitLanguage(strings.ToLower(field))
			if !ok {
				return nil, false
			}
			language = found
		}
		if !seen[language.Language] {
			seen[language.Language] = true
			chosen = append(chosen, language)
		}
	}
	return chosen, len(chosen) > 0
}
