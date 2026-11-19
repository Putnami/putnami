#!/usr/bin/env bun
/**
 * `putnami i18n sync` and `putnami i18n translate`: keep a localized
 * project's translation catalogs in step with its messages.
 *
 * Sync:
 *
 * Adds an entry for each new message, removes the entries of deleted ones, and
 * marks stale every translation whose default text changed, keeping its text
 * for the translator. Each namespace has its own file,
 * `locales/<locale>/<namespace>.json`: sync writes the files that differ,
 * deletes the files of namespaces no declaration holds, and touches nothing
 * else; the build only reads them.
 *
 * Translate writes `.putnami/i18n/translate.json`, the missing and stale
 * entries with their context, for an agent to fill; `--apply` checks the
 * filled file and writes its answers as drafts. The command calls no model.
 *
 * Validate shows a person the draft and stale translations of one catalog
 * at a time as a table, asks once whether to validate them all, review them
 * one by one or skip the catalog, and locks the ones they accept with their
 * git identity and the day. `--locale` and the repeatable `--namespace`
 * narrow it to those catalogs. It needs an interactive terminal and refuses
 * to run inside an agent session.
 *
 * Usage: bun run bin/i18n.ts sync|translate|validate [--putnamiContext <path>]
 */

import { existsSync, mkdirSync, readdirSync, readFileSync, rmdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { readProjectConfigFile } from '@putnami/utils';
import { formatDiagnostic, type I18nBuild, resolveI18nBuild } from '../src/ssr/i18n/build';
import { type Glossary, readGlossary } from '../src/ssr/i18n/glossary';
import {
  applyTranslationWork,
  carryOverAnswers,
  countWork,
  translationWork,
  WORK_FILE,
} from '../src/ssr/i18n/translate';
import {
  AGENT_SESSION_VARIABLES,
  type ValidationCandidate,
  type ValidationSelection,
  validateEntries,
  validationCandidates,
} from '../src/ssr/i18n/validate';

/** Remove the directories a deletion emptied, up to the locale's directory. */
function pruneEmpty(dir: string, stop: string): void {
  for (let current = dir; current !== stop && current.startsWith(stop); current = dirname(current)) {
    if (readdirSync(current).length > 0) return;
    rmdirSync(current);
  }
}

/** The project's resolved localization, or `undefined` when it declares no i18n block. */
async function localization(projectRoot: string): Promise<I18nBuild | undefined> {
  const options = readProjectConfigFile(projectRoot)?.options?.['@putnami/web:generate'] as
    | { i18n?: unknown }
    | undefined;
  if (options?.i18n === undefined) return undefined;
  return resolveI18nBuild(projectRoot, options.i18n, false);
}

export async function sync(projectRoot: string, log: (line: string) => void): Promise<number> {
  const build = await localization(projectRoot);
  if (!build) {
    log('No i18n block in options["@putnami/web:generate"]; nothing to sync');
    return 0;
  }
  for (const catalog of build.catalogs) {
    const { added, removed, staled } = catalog.changes;
    log(
      `${relative(projectRoot, catalog.dir)}: ${catalog.outOfSync ? 'synced' : 'up to date'} (${added.length} added, ${removed.length} removed, ${staled.length} stale)`,
    );
    for (const { file, content } of catalog.writes) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, content);
      log(`  wrote ${relative(projectRoot, file)}`);
    }
    for (const file of catalog.deletions) {
      rmSync(file);
      pruneEmpty(dirname(file), catalog.dir);
      log(`  deleted ${relative(projectRoot, file)}: no messages declaration holds this namespace`);
    }
    for (const key of staled) log(`  stale ${key}: the default text changed; run \`putnami i18n translate\``);
  }
  const errors = build.diagnostics.filter((d) => d.level === 'error' && d.code !== 'missing');
  for (const error of errors) log(`error ${formatDiagnostic(error)}`);
  const missing = build.diagnostics.filter((d) => d.code === 'missing').length;
  if (missing > 0) log(`${missing} message(s) have no translation yet; run \`putnami i18n translate\``);
  return errors.length > 0 ? 1 : 0;
}

/**
 * Without `apply`, write the work file of every missing and stale entry, or
 * delete a leftover one when nothing is left to translate. With `apply`, write
 * the filled file's answers as drafts and delete it; one refused answer
 * refuses the file and writes nothing.
 */
export async function translate(projectRoot: string, apply: boolean, log: (line: string) => void): Promise<number> {
  const build = await localization(projectRoot);
  if (!build) {
    log('No i18n block in options["@putnami/web:generate"]; nothing to translate');
    return 0;
  }
  const workFile = join(projectRoot, WORK_FILE);
  if (apply) {
    if (!existsSync(workFile)) {
      log(`error ${WORK_FILE} does not exist; run \`putnami i18n translate\` first`);
      return 1;
    }
    let work: unknown;
    try {
      work = JSON.parse(readFileSync(workFile, 'utf8'));
    } catch (error) {
      log(`error ${WORK_FILE}: not valid JSON: ${(error as Error).message}`);
      return 1;
    }
    const result = applyTranslationWork(build, work);
    if (result.errors.length > 0) {
      for (const error of result.errors) log(`error ${error}`);
      log(`${result.errors.length} answer(s) refused; nothing written. Correct them in ${WORK_FILE} and apply again`);
      return 1;
    }
    for (const { file, content } of result.writes) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, content);
      log(`  wrote ${relative(projectRoot, file)}`);
    }
    rmSync(workFile);
    log(`${result.applied} draft(s) written; a person validates them with \`putnami i18n validate\``);
    return 0;
  }
  const glossaries: Record<string, Glossary> = {};
  for (const { locale } of build.catalogs) glossaries[locale] = readGlossary(projectRoot, locale);
  const work = translationWork(build, glossaries);
  const count = countWork(work);
  if (count === 0) {
    if (existsSync(workFile)) rmSync(workFile);
    log('Every published locale is translated; nothing to do');
    return 0;
  }
  const kept = existsSync(workFile) ? carryOverAnswers(work, readJson(workFile)) : 0;
  mkdirSync(dirname(workFile), { recursive: true });
  writeFileSync(workFile, `${JSON.stringify(work, null, 2)}\n`);
  log(
    `wrote ${WORK_FILE}: ${count} entr${count === 1 ? 'y' : 'ies'} to translate${kept > 0 ? `, ${kept} answered in the previous work file` : ''}. Fill each null text, then run \`putnami i18n translate --apply\``,
  );
  return 0;
}

/** Ask until the person answers yes, no or quit. */
async function decision(session: ValidationSession): Promise<'yes' | 'no' | 'quit'> {
  for (;;) {
    const reply = (await session.ask('Validate? [y]es, [n]o, [q]uit: ')).trim().toLowerCase();
    if (reply === 'y' || reply === 'yes') return 'yes';
    if (reply === 'n' || reply === 'no') return 'no';
    if (reply === 'q' || reply === 'quit') return 'quit';
    session.log('  answer y, n or q');
  }
}

/** Ask until the person answers all, one by one, skip or quit for a catalog of `count` translations. */
async function catalogDecision(session: ValidationSession, count: number): Promise<'all' | 'one' | 'skip' | 'quit'> {
  for (;;) {
    const subject = count === 1 ? 'this translation' : `all ${count}`;
    const reply = (await session.ask(`Validate ${subject}? [a]ll, [o]ne by one, [s]kip, [q]uit: `))
      .trim()
      .toLowerCase();
    if (reply === 'a' || reply === 'all') return 'all';
    if (reply === 'o' || reply === 'one' || reply === 'one by one') return 'one';
    if (reply === 's' || reply === 'skip') return 'skip';
    if (reply === 'q' || reply === 'quit') return 'quit';
    session.log('  answer a, o, s or q');
  }
}

/** The calendar day of a date in the local time zone, as `YYYY-MM-DD`. */
export function localDay(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** The parsed JSON of a file, or undefined when it is not JSON. */
function readJson(file: string): unknown {
  try {
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    return undefined;
  }
}

/** What validation reads from the person's session, injected so a test can play the person. */
export interface ValidationSession {
  /** Whether stdin and stdout are a terminal. */
  interactive: boolean;
  env: Readonly<Record<string, string | undefined>>;
  /** The git `user.name` and `user.email`, when both are set. */
  identity(): { name: string; email: string } | undefined;
  /** Today, `YYYY-MM-DD`. */
  today(): string;
  ask(question: string): Promise<string>;
  log(line: string): void;
  /** The terminal's width in characters, when known; a catalog's table wraps its text to it. */
  columns?: number;
}

const catalogLabel = (c: ValidationCandidate) => `locales/${c.locale}/${c.namespace}.json`;

/** What a person reads about one translation before deciding. */
function presentation(candidate: ValidationCandidate, defaultLocale: string): string[] {
  const lines = [`${catalogLabel(candidate)} ${candidate.key} (${candidate.state})`];
  if (candidate.previousSource !== undefined)
    lines.push(`  ${defaultLocale}, translated from: ${candidate.previousSource}`);
  lines.push(`  ${defaultLocale}: ${candidate.source}`, `  ${candidate.locale}: ${candidate.text}`);
  return lines;
}

/** The candidates split by catalog, in order: candidates arrive with each catalog's translations adjacent. */
function byCatalog(candidates: readonly ValidationCandidate[]): ValidationCandidate[][] {
  const groups: ValidationCandidate[][] = [];
  for (const candidate of candidates) {
    const last = groups.at(-1);
    if (last && catalogLabel(last[0] as ValidationCandidate) === catalogLabel(candidate)) last.push(candidate);
    else groups.push([candidate]);
  }
  return groups;
}

/** The length of a text in code points, so a character outside the BMP counts once. */
const length = (text: string) => Array.from(text).length;

/** A text padded with spaces to `width` code points. */
const pad = (text: string, width: number) => text + ' '.repeat(Math.max(0, width - length(text)));

/**
 * A text cut into lines of at most `width` code points at its spaces; a word
 * longer than `width` is cut between code points.
 */
export function wrap(text: string, width: number): string[] {
  const lines: string[] = [];
  for (const paragraph of text.split('\n')) {
    let line = '';
    for (const word of paragraph.split(' ')) {
      let rest = Array.from(word);
      while (rest.length > width) {
        if (line !== '') lines.push(line);
        lines.push(rest.slice(0, width).join(''));
        rest = rest.slice(width);
        line = '';
      }
      const tail = rest.join('');
      if (line === '') line = tail;
      else if (length(line) + 1 + rest.length <= width) line = `${line} ${tail}`;
      else {
        lines.push(line);
        line = tail;
      }
    }
    lines.push(line);
  }
  return lines;
}

/**
 * The translations of one catalog as a table of key, default text and
 * translation, each cell wrapped to fit `columns`. A stale row adds the
 * default text its translation was made from.
 */
function catalogTable(candidates: readonly ValidationCandidate[], defaultLocale: string, columns: number): string[] {
  const first = candidates[0];
  if (!first) return [];
  const keyOf = (c: ValidationCandidate) => (c.state === 'stale' ? `${c.key} (stale)` : c.key);
  const keyWidth = Math.max(3, ...candidates.map((c) => length(keyOf(c))));
  const textWidth = Math.max(20, Math.floor((columns - keyWidth - 6) / 2));
  const row = (key: string, source: string[], text: string[]) => {
    const lines: string[] = [];
    for (let i = 0; i < Math.max(1, source.length, text.length); i++) {
      const cells = [pad(i === 0 ? key : '', keyWidth), pad(source[i] ?? '', textWidth), text[i] ?? ''];
      lines.push(`  ${cells.join('  ')}`.trimEnd());
    }
    return lines;
  };
  const lines = row('key', [defaultLocale], [first.locale]);
  lines.push(`  ${'-'.repeat(keyWidth)}  ${'-'.repeat(textWidth)}  ${'-'.repeat(textWidth)}`);
  for (const candidate of candidates) {
    const source = wrap(candidate.source, textWidth);
    if (candidate.previousSource !== undefined)
      source.push(...wrap(`(translated from: ${candidate.previousSource})`, textWidth));
    lines.push(...row(keyOf(candidate), source, wrap(candidate.text, textWidth)));
  }
  return lines;
}

/**
 * Show a person the draft and stale translations of each selected catalog as
 * a table, ask once per catalog whether to validate them all, review them one
 * by one or skip it, then lock the ones they accept. Refuses without an
 * interactive terminal, inside an agent session, or without a git identity,
 * before reading any catalog.
 */
export async function validate(
  projectRoot: string,
  selection: ValidationSelection,
  session: ValidationSession,
): Promise<number> {
  const { log } = session;
  if (!session.interactive) {
    log('error putnami i18n validate needs a person at an interactive terminal');
    return 1;
  }
  const agent = AGENT_SESSION_VARIABLES.find((name) => session.env[name]);
  if (agent) {
    log(
      `error putnami i18n validate refuses to run inside an agent session (${agent} is set): a person validates translations`,
    );
    return 1;
  }
  const identity = session.identity();
  if (!identity) {
    log('error set git user.name and user.email: validation records who validated');
    return 1;
  }
  const build = await localization(projectRoot);
  if (!build) {
    log('No i18n block in options["@putnami/web:generate"]; nothing to validate');
    return 0;
  }
  const { locale, namespaces = [] } = selection;
  if (locale !== undefined && !build.catalogs.some((c) => c.locale === locale)) {
    log(`error ${locale} is not a published locale other than the default one`);
    return 1;
  }
  const unknown = namespaces.filter((namespace) => !build.declared.has(namespace));
  if (unknown.length > 0) {
    log(
      `error ${unknown.join(', ')} ${unknown.length === 1 ? 'is not a namespace' : 'are not namespaces'} this project declares`,
    );
    return 1;
  }
  const candidates = validationCandidates(build, selection);
  if (candidates.length === 0) {
    log('No draft or stale translation to validate');
    return 0;
  }
  const { defaultLocale } = build.settings;
  const catalogs = byCatalog(candidates);
  const accepted: ValidationCandidate[] = [];
  review: for (const [index, group] of catalogs.entries()) {
    const label = catalogLabel(group[0] as ValidationCandidate);
    const valid = group.filter((candidate) => !candidate.defect);
    log(`[${index + 1}/${catalogs.length}] ${label}: ${valid.length} translation(s) to validate`);
    if (valid.length > 0) for (const line of catalogTable(valid, defaultLocale, session.columns ?? 100)) log(line);
    for (const candidate of group) {
      if (candidate.defect) log(`  skipped ${candidate.key}: ${candidate.defect}; correct it in ${label} first`);
    }
    if (valid.length === 0) continue;
    const reply = await catalogDecision(session, valid.length);
    if (reply === 'quit') break;
    if (reply === 'skip') continue;
    if (reply === 'all') {
      accepted.push(...valid);
      continue;
    }
    for (const [position, candidate] of valid.entries()) {
      log(`[${position + 1}/${valid.length}] ${presentation(candidate, defaultLocale).join('\n')}`);
      const answer = await decision(session);
      if (answer === 'quit') break review;
      if (answer === 'yes') accepted.push(candidate);
    }
  }
  const writes = validateEntries(build, accepted, `${identity.name} <${identity.email}>`, session.today());
  for (const { file, content } of writes) {
    writeFileSync(file, content);
    log(`  wrote ${relative(projectRoot, file)}`);
  }
  log(`${accepted.length} translation(s) validated by ${identity.name}`);
  return 0;
}

export interface JobContext {
  params?: Record<string, unknown>;
  selectedProjects?: { fullPath?: string }[];
  invocationDir?: string;
}

/** The job context the CLI passes as `--putnamiContext <path>`, or none. */
function jobContext(args: string[]): JobContext {
  const at = args.indexOf('--putnamiContext');
  const path = at >= 0 ? args[at + 1] : undefined;
  return path ? (JSON.parse(readFileSync(path, 'utf8')) as JobContext) : {};
}

/**
 * Every value of a repeatable flag, in order, from the job's arguments, which
 * keep each occurrence; its params keep the last one only. An occurrence
 * without a value, followed by another flag or by nothing, adds none.
 */
export function flagValues(args: readonly string[], context: JobContext, name: string): string[] {
  const values: string[] = [];
  for (let i = 0; i < args.length; i++) {
    const arg = args[i] as string;
    const next = args[i + 1];
    if (arg === `--${name}`) {
      if (next !== undefined && !next.startsWith('-')) values.push(args[++i] as string);
    } else if (arg.startsWith(`--${name}=`) && arg.length > name.length + 3) values.push(arg.slice(name.length + 3));
  }
  if (values.length > 0) return values;
  const param = context.params?.[name];
  if (Array.isArray(param)) return param.filter((value): value is string => typeof value === 'string');
  return typeof param === 'string' && param !== '' ? [param] : [];
}

function flag(context: JobContext, name: string): boolean {
  const value = context.params?.[name];
  return value === true || (typeof value === 'string' && ['1', 'true', 'yes', 'on'].includes(value.toLowerCase()));
}

function gitConfig(cwd: string, key: string): string | undefined {
  const result = Bun.spawnSync(['git', 'config', '--get', key], { cwd, stdout: 'pipe', stderr: 'ignore' });
  const value = result.stdout.toString().trim();
  return result.exitCode === 0 && value !== '' ? value : undefined;
}

/** Run the validation session on the one selected project, with the terminal and git as the person. */
async function validateSelected(
  args: readonly string[],
  context: JobContext,
  log: (line: string) => void,
): Promise<number> {
  const roots = context.selectedProjects?.flatMap((p) => (p.fullPath ? [p.fullPath] : [])) ?? [];
  if (roots.length !== 1) {
    log('error select one project: putnami i18n validate --projects <project>');
    return 2;
  }
  const root = roots[0] as string;
  const { createInterface } = await import('node:readline/promises');
  const prompt = createInterface({ input: process.stdin, output: process.stdout });
  const locale = context.params?.['locale'];
  try {
    const selection: ValidationSelection = { namespaces: flagValues(args, context, 'namespace') };
    if (typeof locale === 'string' && locale !== '') selection.locale = locale;
    return await validate(root, selection, {
      interactive: Boolean(process.stdin.isTTY && process.stdout.isTTY),
      columns: process.stdout.columns,
      env: process.env,
      identity: () => {
        const name = gitConfig(root, 'user.name');
        const email = gitConfig(root, 'user.email');
        return name && email ? { name, email } : undefined;
      },
      today: () => localDay(new Date()),
      ask: (question) => prompt.question(question),
      log,
    });
  } finally {
    prompt.close();
  }
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const [verb] = args;
  const context = jobContext(args);
  // The CLI skips a captured task's plain stdout lines and shows its stderr when it fails:
  // a refused entry reaches the person or agent that ran the command.
  // biome-ignore lint/suspicious/noConsole: CLI report output
  const report = (line: string) => console.error(line);
  // process.exitCode, not process.exit(): exit drops the lines still queued on a pipe.
  if (verb === 'sync') process.exitCode = await sync(process.cwd(), report);
  else if (verb === 'translate') process.exitCode = await translate(process.cwd(), flag(context, 'apply'), report);
  // biome-ignore lint/suspicious/noConsole: validate owns the terminal
  else if (verb === 'validate') process.exitCode = await validateSelected(args, context, (line) => console.log(line));
  else {
    // biome-ignore lint/suspicious/noConsole: CLI usage output
    console.error('Usage: putnami i18n sync|translate|validate');
    process.exitCode = 2;
  }
}
