import { Glob } from 'bun';
import { fileExists, getWorkspaceRoot, joinPath } from '@putnami/utils';
import type { TocItem, TopicDoc } from '@putnami/ui';
import type { Tokens } from 'marked';
import { marked } from 'marked';
import { createSlugger, MARKED_OPTIONS, parseInlineSync, toPlainTextFromHtml } from './markdown-utils';

export interface PackageInfo {
  name: string;
  readme: string;
  topics: string[];
  hasDoc: boolean;
}

const PROJECT_ROOT_PATTERNS = ['tooling/*', 'platform/*', 'typescript/framework/*', 'go/framework/*'];

const EXTENSION_ROOTS = {
  go: 'go/extension',
  python: 'python/extension',
  typescript: 'typescript/extension',
} as const;

interface DocsPaths {
  name: string;
  aliases: Set<string>;
  docPath: string;
  readmePath: string;
}

let docsPathsCache: DocsPaths[] | undefined;

function addAlias(aliases: Set<string>, value: string | undefined) {
  if (!value) {
    return;
  }

  aliases.add(value);
  if (value.startsWith('@')) {
    aliases.add(value.split('/').at(-1) ?? value);
  }
}

async function readJsonName(path: string): Promise<string | undefined> {
  if (!fileExists(path)) {
    return undefined;
  }

  try {
    const value = JSON.parse(await Bun.file(path).text());
    return typeof value.name === 'string' && value.name.length > 0 ? value.name : undefined;
  } catch {
    return undefined;
  }
}

async function getProjectName(projectRoot: string): Promise<string> {
  const packageName = await readJsonName(joinPath(projectRoot, 'package.json'));
  if (packageName) {
    return packageName;
  }

  const manifestName = await readJsonName(joinPath(projectRoot, 'putnami.extension.json'));
  if (manifestName) {
    return manifestName;
  }

  return projectRoot.split('/').filter(Boolean).at(-1) ?? projectRoot;
}

async function discoverDocsPaths(): Promise<DocsPaths[]> {
  if (docsPathsCache) {
    return docsPathsCache;
  }

  const workspaceRoot = getWorkspaceRoot();
  const projectRoots = new Set<string>();

  for (const pattern of PROJECT_ROOT_PATTERNS) {
    for (const entry of new Glob(pattern).scanSync(workspaceRoot)) {
      projectRoots.add(joinPath(workspaceRoot, entry));
    }
  }

  for (const projectRoot of Object.values(EXTENSION_ROOTS)) {
    projectRoots.add(joinPath(workspaceRoot, projectRoot));
  }

  const docsPaths: DocsPaths[] = [];
  for (const projectRoot of projectRoots) {
    const docPath = joinPath(projectRoot, 'doc');
    const readmePath = joinPath(projectRoot, 'README.md');
    if (!fileExists(docPath) && !fileExists(readmePath)) {
      continue;
    }

    const aliases = new Set<string>();
    const projectName = await getProjectName(projectRoot);
    addAlias(aliases, projectName);
    addAlias(aliases, projectRoot.split('/').filter(Boolean).at(-1));

    const parentName = projectRoot.split('/').filter(Boolean).at(-2);
    if (parentName && projectRoot.endsWith('/extension')) {
      addAlias(aliases, parentName);
    }

    docsPaths.push({
      name: projectName,
      aliases,
      docPath,
      readmePath,
    });
  }

  docsPathsCache = docsPaths.sort((a, b) => a.name.localeCompare(b.name));
  return docsPathsCache;
}

/**
 * Resolve paths for documentation and readme.
 */
async function resolveDocsPaths(packageName: string): Promise<DocsPaths | null> {
  const docsPaths = await discoverDocsPaths();
  return docsPaths.find((entry) => entry.aliases.has(packageName)) ?? null;
}

/**
 * Load package documentation from the repo-owned project roots.
 */
export async function loadPackageDocs(packageName: string): Promise<PackageInfo> {
  const paths = await resolveDocsPaths(packageName);

  let readme = '';
  const topics: string[] = [];

  if (paths) {
    if (fileExists(paths.readmePath)) {
      readme = await Bun.file(paths.readmePath).text();
    }

    if (fileExists(paths.docPath)) {
      for (const file of new Glob('*.md').scanSync(paths.docPath)) {
        topics.push(file.replace('.md', ''));
      }
    }
  }

  return {
    name: paths?.name ?? packageName,
    readme,
    topics: topics.sort(),
    hasDoc: topics.length > 0,
  };
}

/**
 * Load a specific topic document.
 */
export async function loadTopicDoc(packageName: string, topic: string): Promise<TopicDoc> {
  const paths = await resolveDocsPaths(packageName);

  if (!paths) {
    throw new Response('Documentation section not found', { status: 404 });
  }

  const docFilePath = joinPath(paths.docPath, `${topic}.md`);

  if (!fileExists(docFilePath)) {
    throw new Response('Documentation file not found', { status: 404 });
  }

  const content = await Bun.file(docFilePath).text();
  const toc = await extractToc(content);

  return { content, toc };
}

/**
 * Extract table of contents from markdown content.
 */
export async function extractToc(content: string): Promise<TocItem[]> {
  const toc: TocItem[] = [];
  const slugger = createSlugger();
  const tokens = marked.lexer(content, MARKED_OPTIONS);

  for (const token of tokens) {
    if (token.type !== 'heading') {
      continue;
    }

    const heading = token as Tokens.Heading;
    const rendered = parseInlineSync(heading.text);
    toc.push({
      level: heading.depth,
      id: slugger.slug(heading.text, rendered),
      text: toPlainTextFromHtml(rendered),
    });
  }

  return toc;
}

/**
 * Get all available packages with documentation.
 */
export async function getAllPackages(): Promise<PackageInfo[]> {
  const docsPaths = await discoverDocsPaths();
  const packages: PackageInfo[] = [];

  for (const entry of docsPaths) {
    let readme = '';
    const topics: string[] = [];

    if (fileExists(entry.readmePath)) {
      readme = await Bun.file(entry.readmePath).text();
    }

    if (fileExists(entry.docPath)) {
      for (const file of new Glob('*.md').scanSync(entry.docPath)) {
        topics.push(file.replace('.md', ''));
      }
    }

    if (!readme && topics.length === 0) {
      continue;
    }

    packages.push({
      name: entry.name,
      readme,
      topics: topics.sort(),
      hasDoc: topics.length > 0,
    });
  }

  return packages.sort((a, b) => a.name.localeCompare(b.name));
}
