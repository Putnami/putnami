package language

import (
	"strings"
	"testing"
)

func TestLanguageSignalsAndActionableLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text     string
		offender bool
	}{
		{"Nous corrigons le code", true},
		{"corrige le parseur des tableaux", true},
		{"école café", true},
		{"An English naïve example", false},
		{"Des Moines", false},
		{"The RGPD and CNIL review `nous avec`\n```\nnous avec\n```", false},
		{"Use --avec on the command line", false},
	}
	for _, tc := range cases {
		if got := EvaluateLanguage("unit", tc.text) != nil; got != tc.offender {
			t.Errorf("%q offender=%t, want %t", tc.text, got, tc.offender)
		}
	}
	if hit := EvaluateLanguage("commit", "Explain the reason\nNous corrigeons le code"); hit == nil || len(hit.Lines) == 0 || !strings.HasPrefix(hit.Lines[0], "2:") {
		t.Fatalf("diagnostic lost actionable line: %+v", hit)
	}
}
