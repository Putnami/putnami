/**
 * The generated `/docs/languages` page: every language and monorepo tool
 * Putnami runs, each linked to its reference, each with the status the reviewed
 * catalog gives its extension.
 *
 * The lists come from `../tools` and every status from the catalog, so the page
 * restates neither: a subject the catalog does not classify renders as having
 * no reviewed status, and a newly reviewed entry appears on the next build.
 * Client-safe: a pure function of the catalog it is handed.
 */
import { LANGUAGES, MONOREPO_TOOLS, type RunTarget } from '../tools';
import { type SupportCatalog, statusLabel, statusOf } from './catalog';

/** What the status cell says for a subject the catalog does not classify. */
export const UNCLASSIFIED_LABEL = 'No reviewed status';

function rows(catalog: SupportCatalog, targets: readonly RunTarget[]): string[] {
  return targets.map((target) => {
    const status = statusOf(catalog, target.supportSubject);
    return `| [${target.name}](${target.href}) | \`${target.extension}\` | ${status ? statusLabel(status) : UNCLASSIFIED_LABEL} |`;
  });
}

/**
 * Render the published `/docs/languages` page from the reviewed catalog.
 *
 * The output is a pure function of the catalog and the two lists, so the same
 * catalog always produces the same page and the generate step stays cacheable.
 */
export function renderLanguagesPage(catalog: SupportCatalog): string {
  const lines: string[] = [
    '# Languages and monorepo tools',
    '',
    'Putnami runs a workspace through extensions: one per language, and one per',
    'monorepo tool a repository already uses. This page lists each of them with',
    'the reference that documents it. The status column is read from the',
    'reviewed support catalog every time the site is built; what each status',
    'commits to, and what a missing one means, is on',
    '[Support status](/docs/support).',
    '',
    '## Languages',
    '',
    'TypeScript, Go and Python run on full extensions: Putnami scaffolds their',
    'projects from its templates and runs them with its own jobs. Each other',
    'language runs on a stack extension, which runs the tools the repository',
    'already uses and reads the repository the way `putnami observe` does.',
    '',
    '| Language | Extension | Status |',
    '| --- | --- | --- |',
    ...rows(catalog, LANGUAGES),
    '',
    '## Monorepo tools',
    '',
    'A monorepo tool integration turns each project of a repository built with',
    'that tool into a Putnami project. Putnami selects what a change affects and',
    'gives the verdict; who runs each task and whose cache answers depends on the',
    'tool, and on the mode the repository adopts it in.',
    '[Adopt Putnami on an existing monorepo](/docs/how-to/adopt-putnami-on-an-existing-monorepo)',
    'walks each tool through `putnami init --from <tool>`.',
    '',
    '| Tool | Extension | Status |',
    '| --- | --- | --- |',
    ...rows(catalog, MONOREPO_TOOLS),
    '',
  ];
  return lines.join('\n');
}
