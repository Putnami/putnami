package language

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Offender is one text unit with French signals and actionable source lines.
type Offender struct {
	Label   string
	Signals []string
	Lines   []string
}

var (
	inlineCode  = regexp.MustCompile("`[^`]*`")
	optionWord  = regexp.MustCompile(`(^|\s)--?[[:alnum:]][[:alnum:]-]*`)
	properNouns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)Règlement\s+Général\s+sur\s+la\s+Protection\s+des\s+Données`),
		regexp.MustCompile(`(?i)Commission\s+Nationale\s+de\s+l['’]Informatique\s+et\s+des\s+Libertés`),
		regexp.MustCompile(`\bRGPD\b`), regexp.MustCompile(`\bCNIL\b`),
	}
	strongWords = map[string]bool{"nous": true, "avec": true, "pour que": true, "il faut": true,
		"ainsi": true, "donc": true, "c est": true, "parce que": true, "il y a": true}
	commonWords = map[string]bool{}
)

func init() {
	for _, word := range strings.Fields("le la les des une du aux est sont dans sur pas mais aussi cette ces qui que ajoute supprime corrige fichier fichiers") {
		commonWords[word] = true
		commonWords[strings.ToUpper(word[:1])+word[1:]] = true
	}
}

func maskMatch(text string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(text, func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '\n' {
				return r
			}
			return ' '
		}, s)
	})
}

// EvaluateLanguage applies the repository's bounded French-signal heuristic.
// It does not claim to identify every language.
func EvaluateLanguage(label, input string) *Offender {
	lines := strings.Split(input, "\n")
	codeFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			codeFence = !codeFence
			lines[i] = ""
			continue
		}
		if codeFence {
			lines[i] = ""
			continue
		}
		line = inlineCode.ReplaceAllString(line, "")
		lines[i] = optionWord.ReplaceAllString(line, " ")
	}
	clean := strings.Join(lines, "\n")
	for _, re := range properNouns {
		clean = maskMatch(clean, re)
	}
	lines = strings.Split(clean, "\n")
	strong, common, accent := map[string]bool{}, map[string]bool{}, map[string]bool{}
	lineNumbers := map[int]bool{}
	for i, line := range lines {
		words := strings.FieldsFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) })
		for j, word := range words {
			lower := strings.ToLower(word)
			found := false
			for n := 1; n <= 3 && j+n <= len(words); n++ {
				phrase := make([]string, n)
				for k := 0; k < n; k++ {
					phrase[k] = strings.ToLower(words[j+k])
				}
				joined := strings.Join(phrase, " ")
				if strongWords[joined] {
					strong[joined] = true
					found = true
				}
			}
			if commonWords[word] {
				common[lower] = true
				found = true
			}
			if strings.ContainsAny(word, "éèêàçùœÉÈÊÀÇÙŒ") {
				accent[word] = true
				found = true
			}
			if found {
				lineNumbers[i+1] = true
			}
		}
	}
	if len(strong) == 0 && len(accent) < 2 && len(common) < 2 {
		return nil
	}
	if len(common) < 2 {
		common = map[string]bool{}
	}
	signals := make([]string, 0, len(strong)+len(common)+len(accent))
	for word := range strong {
		signals = append(signals, word)
	}
	for word := range common {
		signals = append(signals, word)
	}
	for word := range accent {
		signals = append(signals, word)
	}
	sort.Strings(signals)
	if len(signals) > 10 {
		extra := len(signals) - 10
		signals = append(signals[:10], fmt.Sprintf("(+%d more)", extra))
	}
	numbers := make([]int, 0, len(lineNumbers))
	for n := range lineNumbers {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	if len(numbers) > 5 {
		numbers = numbers[:5]
	}
	details := make([]string, 0, len(numbers))
	for _, n := range numbers {
		detail := fmt.Sprintf("%d:%s", n, lines[n-1])
		if len(detail) > 200 {
			detail = detail[:200]
		}
		details = append(details, detail)
	}
	return &Offender{Label: label, Signals: signals, Lines: details}
}
