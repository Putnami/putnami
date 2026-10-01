package support

import (
	"strings"
	"testing"
)

const catalogLinkExample = "[`putnami.support.json`](../../putnami.support.json)"

func TestReadStatusDeclarationFindsTheSectionsOwnStatus(t *testing.T) {
	tests := []struct {
		name      string
		document  string
		wantFound bool
		wantValue Status
		wantLink  bool
	}{
		{
			name:      "no support section at all",
			document:  "# Wire\n\n## Vocabulary\n\nWords.\n",
			wantFound: false,
		},
		{
			name:      "a heading about supported backends is not a support section",
			document:  "# Wire\n\n## Supported backends\n\n`stable` is not the subject here.\n",
			wantFound: false,
		},
		{
			name:      "bullet convention",
			document:  "## Support\n\n- **Status:** `preview`, recorded in " + catalogLinkExample + ".\n",
			wantFound: true,
			wantValue: StatusPreview,
			wantLink:  true,
		},
		{
			name:      "table convention",
			document:  "## Support status, owner, and evidence\n\n| | |\n|---|---|\n| **Status** | `stable` — see " + catalogLinkExample + " |\n",
			wantFound: true,
			wantValue: StatusStable,
			wantLink:  true,
		},
		{
			name:      "a later mention does not override the declaration",
			document:  "## Ownership, support, and evidence\n\n**Support status** — `preview` in " + catalogLinkExample + ".\n\nWhy not `stable`: one implementation.\n",
			wantFound: true,
			wantValue: StatusPreview,
			wantLink:  true,
		},
		{
			name:      "a status named in prose is not written as inline code",
			document:  "## Support\n\nThis contract is stable, honestly.\n",
			wantFound: true,
		},
		{
			name:      "a nested subheading stays inside the section",
			document:  "## Support\n\n### Evidence\n\n`experimental`, per " + catalogLinkExample + ".\n\n## Next\n\n`stable`\n",
			wantFound: true,
			wantValue: StatusExperimental,
			wantLink:  true,
		},
		{
			name:      "the section ends at the next heading of the same level",
			document:  "## Support\n\nNo status here.\n\n## Evidence\n\n`stable` in " + catalogLinkExample + "\n",
			wantFound: true,
		},
		{
			name:      "naming the catalog in prose is not linking to it",
			document:  "## Support\n\n- **Status:** `stable`. Do not edit putnami.support.json by hand.\n",
			wantFound: true,
			wantValue: StatusStable,
			wantLink:  false,
		},
		{
			name:      "a fenced example never becomes the declaration",
			document:  "# Wire\n\n## Conventions\n\nDocument support like this:\n\n```markdown\n## Support\n\n- **Status:** `experimental`\n```\n\n## Support\n\n- **Status:** `stable`, recorded in " + catalogLinkExample + ".\n",
			wantFound: true,
			wantValue: StatusStable,
			wantLink:  true,
		},
		{
			name:      "a fenced example is not a section on its own",
			document:  "# Wire\n\n## Conventions\n\n```markdown\n## Support\n\n- **Status:** `experimental`\n```\n",
			wantFound: false,
		},
		{
			name:      "a tilde fence is stripped like a backtick fence",
			document:  "# Wire\n\n## Conventions\n\n~~~markdown\n## Support\n\n- **Status:** `experimental`\n~~~\n",
			wantFound: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			declaration := readStatusDeclaration(test.document)
			if declaration.section != test.wantFound {
				t.Fatalf("section = %v, want %v", declaration.section, test.wantFound)
			}
			if declaration.status != test.wantValue {
				t.Errorf("status = %q, want %q", declaration.status, test.wantValue)
			}
			if declaration.linksCatalog != test.wantLink {
				t.Errorf("linksCatalog = %v, want %v", declaration.linksCatalog, test.wantLink)
			}
		})
	}
}

func TestStatusDeclarationExplainsEveryDisagreement(t *testing.T) {
	tests := []struct {
		name     string
		document string
		reviewed Status
		want     string
	}{
		{
			name:     "no section",
			document: "# Wire\n",
			reviewed: StatusStable,
			want:     "no support section",
		},
		{
			name:     "no status in the section",
			document: "## Support\n\nSee the catalog.\n",
			reviewed: StatusStable,
			want:     "writes no status as inline code",
		},
		{
			name:     "drifted status",
			document: "## Support\n\n- **Status:** `stable`, per " + catalogLinkExample + ".\n",
			reviewed: StatusPreview,
			want:     "declares `stable` but the reviewed catalog says `preview`",
		},
		{
			name:     "no link to the authority",
			document: "## Support\n\n- **Status:** `preview`, see putnami.support.json.\n",
			reviewed: StatusPreview,
			want:     "without linking to " + CatalogFilename,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason := readStatusDeclaration(test.document).explain(test.reviewed)
			if !strings.Contains(reason, test.want) {
				t.Fatalf("explain(%q) = %q, want it to contain %q", test.reviewed, reason, test.want)
			}
		})
	}
}

func TestStatusDeclarationAgreesWithTheReviewedStatus(t *testing.T) {
	document := "## Support\n\n- **Status:** `stable`, recorded in " + catalogLinkExample + ".\n"
	if reason := readStatusDeclaration(document).explain(StatusStable); reason != "" {
		t.Fatalf("explain(stable) = %q, want empty", reason)
	}
}
