import { Box, styled } from '@putnami/ui';
import type { ComponentProps } from 'react';
import { type NavItem, toUrlPath } from '../lib/docs/navigation';
import { pageCountLabel, toolList } from '../lib/tools';
import { ToolGlyph } from './tool-glyph';

interface SidebarProps extends ComponentProps<typeof Box> {
  navItems: NavItem[];
  /**
   * The active doc path used to highlight the current link. Passed explicitly
   * (rather than read from a router) so the sidebar is router-free: the static
   * docs layout bakes it from the build-time location, and the mobile-menu
   * island supplies `window.location.pathname` after hydration.
   */
  currentPath?: string;
}

interface NavScope {
  id: string;
  name: string;
  href: string;
  subtitle: string;
  toolId?: string;
  rootItem?: NavItem;
  items: NavItem[];
}

const START_SCOPE_NAMES = new Set(['Getting Started', 'Concepts', 'How To', 'Principles']);
const DEFAULT_OPEN_GROUPS = new Set([
  'How To / Guides',
  'Extension',
  'Framework / Capabilities',
  'Lifecycle / Capabilities',
  'Workspace Model',
  'Extension Capabilities',
  'Frameworks',
]);

const GUIDE_NAMES_BY_SCOPE: Record<string, string[]> = {
  start: [
    'Build A Web App',
    'Build An Api Service',
    'Share Code Between Projects',
    'Configure Your App',
    'Add Persistence',
    'Add Authentication',
    'Add Background Jobs',
    'Develop With Ai',
    'Structure Business Logic With Di',
    'Upgrade Putnami',
  ],
  tooling: ['Share Code Between Projects', 'Develop With Ai', 'Upgrade Putnami'],
  ts: [
    'Build A Web App',
    'Build An Api Service',
    'Share Code Between Projects',
    'Configure Your App',
    'Structure Business Logic With Di',
    'Add Persistence',
    'Add Authentication',
  ],
  go: [
    'Build An Api Service',
    'Configure Your App',
    'Structure Business Logic With Di',
    'Add Persistence',
    'Develop With Ai',
  ],
  py: ['Build An Api Service', 'Share Code Between Projects', 'Develop With Ai', 'Upgrade Putnami'],
};

function normalizePath(path: string): string {
  if (!path || path === '#') return '#';

  let decoded = path;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    // If the browser hands us a malformed path, compare the raw value.
  }

  return decoded.replace(/\/+$/, '') || '/';
}

function isOnOrUnderPath(itemPath: string, currentPath: string): boolean {
  const item = normalizePath(itemPath);
  const current = normalizePath(currentPath);
  return item !== '#' && (current === item || current.startsWith(`${item}/`));
}

/** Whether the active path lives anywhere inside this item's subtree. */
function subtreeHasActive(item: NavItem, currentPath: string): boolean {
  if (isOnOrUnderPath(toUrlPath(item.contentPath), currentPath)) return true;
  return (item.children ?? []).some((child) => subtreeHasActive(child, currentPath));
}

/** Count the navigable leaf pages under an item — shown as a quantity cue. */
function countPages(item: NavItem): number {
  const children = item.children ?? [];
  if (children.length === 0) return 1;
  return children.reduce((sum, child) => sum + countPages(child), 0);
}

function findRoot(navItems: NavItem[], name: string): NavItem | undefined {
  return navItems.find((item) => item.name === name);
}

function childNamed(item: NavItem | undefined, name: string): NavItem | undefined {
  const wanted = name.toLowerCase();
  return item?.children?.find((child) => child.name.toLowerCase() === wanted);
}

function childrenNamed(item: NavItem | undefined, names: string[]): NavItem[] {
  return names.flatMap((name) => {
    const child = childNamed(item, name);
    return child ? [child] : [];
  });
}

function itemsForScope(rootItem: NavItem): NavItem[] {
  return rootItem.children?.length ? rootItem.children : [rootItem];
}

function countScopePages(scope: NavScope): number {
  if (scope.rootItem) return countPages(scope.rootItem);
  return scope.items.reduce((sum, item) => sum + countPages(item), 0);
}

function scopeHasActive(scope: NavScope, currentPath: string): boolean {
  if (isOnOrUnderPath(scope.href, currentPath)) return true;
  if (scope.rootItem && subtreeHasActive(scope.rootItem, currentPath)) return true;
  return scope.items.some((item) => subtreeHasActive(item, currentPath));
}

function navGroup(name: string, order: number, children: NavItem[], contentPath?: string): NavItem {
  return { name, order, contentPath, children };
}

function navLeaf(name: string, order: number, contentPath: string | undefined): NavItem | undefined {
  if (!contentPath) return undefined;
  return { name, order, contentPath };
}

function guideGroup(navItems: NavItem[], scopeId: string): NavItem | undefined {
  const howTo = findRoot(navItems, 'How To');
  if (!howTo) return undefined;

  const guideNames = GUIDE_NAMES_BY_SCOPE[scopeId] ?? GUIDE_NAMES_BY_SCOPE['start'];
  const children = childrenNamed(howTo, guideNames);
  return navGroup('How To / Guides', 1, children.length > 0 ? children : (howTo.children ?? []), howTo.contentPath);
}

function compact<T>(items: Array<T | undefined>): T[] {
  return items.filter((item): item is T => Boolean(item));
}

function buildStartItems(navItems: NavItem[]): NavItem[] {
  return compact([
    findRoot(navItems, 'Getting Started'),
    guideGroup(navItems, 'start'),
    findRoot(navItems, 'Frameworks'),
    findRoot(navItems, 'Tooling & Workspace'),
    findRoot(navItems, 'Concepts'),
    findRoot(navItems, 'Principles'),
  ]);
}

function buildToolingItems(scope: NavScope, navItems: NavItem[]): NavItem[] {
  const root = scope.rootItem;
  const toolingChildren = root?.children ?? [];
  const frameworks = findRoot(navItems, 'Frameworks');

  return compact([
    navLeaf('Getting Started', 0, root?.contentPath),
    guideGroup(navItems, 'tooling'),
    navGroup('Workspace Model', 2, childrenNamed(root, ['Workspace', 'Cli', 'Jobs & Caching'])),
    navGroup('Extension Capabilities', 3, childrenNamed(root, ['Extensions', 'Templates', 'Error Handling'])),
    frameworks ? navGroup('Frameworks', 4, frameworks.children ?? [], frameworks.contentPath) : undefined,
    ...toolingChildren.filter(
      (item) =>
        !['Workspace', 'Cli', 'Jobs & Caching', 'Extensions', 'Templates', 'Error Handling'].includes(item.name),
    ),
  ]);
}

function frameworkCapabilities(scope: NavScope): NavItem | undefined {
  const root = scope.rootItem;
  if (!root) return undefined;

  if (scope.id === 'ts') {
    return navGroup(
      'Framework / Capabilities',
      4,
      compact([
        childNamed(root, 'Overview'),
        navGroup(
          'Web Experience',
          1,
          childrenNamed(root, ['Web', 'React Routing', 'Forms And Actions', 'Static Files']),
        ),
        navGroup(
          'APIs & Runtime',
          2,
          childrenNamed(root, [
            'Api',
            'Errors And Responses',
            'Http And Middleware',
            'Websockets',
            'Plugins And Lifecycle',
          ]),
        ),
        navGroup(
          'Application Services',
          3,
          childrenNamed(root, ['Configuration', 'Logging', 'Dependency Injection', 'Sessions', 'Auth']),
        ),
        navGroup(
          'Data & Messaging',
          4,
          childrenNamed(root, ['Persistence', 'Document', 'Events', 'Storage', 'Caching']),
        ),
        navGroup(
          'Operations & Contracts',
          5,
          childrenNamed(root, [
            'Testing',
            'Health Checks',
            'Telemetry',
            'Proto Grpc',
            'Smart Client',
            'Schema',
            'Platform Endpoints',
          ]),
        ),
      ]),
      root.contentPath,
    );
  }

  if (scope.id === 'go') {
    return navGroup(
      'Framework / Capabilities',
      4,
      compact([
        childNamed(root, 'Overview'),
        navGroup(
          'Runtime',
          1,
          childrenNamed(root, ['Http', 'Plugins And Lifecycle', 'Grpc', 'Service Clients', 'Platform Endpoints']),
        ),
        navGroup(
          'Application Services',
          2,
          childrenNamed(root, ['Dependency Injection', 'Configuration', 'Security', 'Logging']),
        ),
        navGroup('Data & Messaging', 3, childrenNamed(root, ['Persistence', 'Events', 'Storage', 'Caching'])),
        navGroup(
          'Operations & Contracts',
          4,
          childrenNamed(root, ['Errors', 'Telemetry', 'Validation', 'Openapi', 'Testing']),
        ),
      ]),
      root.contentPath,
    );
  }

  return navGroup('Lifecycle / Capabilities', 4, [], root.contentPath);
}

function buildFrameworkItems(scope: NavScope, navItems: NavItem[]): NavItem[] {
  const root = scope.rootItem;
  const tooling = findRoot(navItems, 'Tooling & Workspace');
  const extension = childNamed(root, 'Extension');
  const capabilities = frameworkCapabilities(scope);

  if (scope.id === 'py') {
    const lifecycle = capabilities
      ? {
          ...capabilities,
          children: compact([
            navLeaf('Templates', 1, childNamed(tooling, 'Templates')?.contentPath),
            navLeaf('Jobs & Caching', 2, childNamed(tooling, 'Jobs & Caching')?.contentPath),
          ]),
        }
      : undefined;

    return compact([childNamed(root, 'Getting Started'), guideGroup(navItems, 'py'), extension, lifecycle]);
  }

  return compact([childNamed(root, 'Getting Started'), guideGroup(navItems, scope.id), extension, capabilities]);
}

function itemsForSidebarScope(scope: NavScope, navItems: NavItem[]): NavItem[] {
  if (scope.id === 'start') return buildStartItems(navItems);
  if (scope.id === 'tooling') return buildToolingItems(scope, navItems);
  if (scope.id === 'ts' || scope.id === 'go' || scope.id === 'py') return buildFrameworkItems(scope, navItems);
  return scope.items;
}

function buildDocScopes(navItems: NavItem[]): NavScope[] {
  const scopes: NavScope[] = [];

  const startItems = navItems.filter((item) => START_SCOPE_NAMES.has(item.name));
  if (startItems.length > 0) {
    scopes.push({
      id: 'start',
      name: 'Start & guides',
      href: toUrlPath(startItems[0]?.contentPath),
      subtitle: 'orientation',
      items: startItems,
    });
  }

  const tooling = findRoot(navItems, 'Tooling & Workspace');
  const frameworks = findRoot(navItems, 'Frameworks');
  const platform = findRoot(navItems, 'Platform');

  for (const tool of toolList()) {
    if (tool.id === 'tooling') {
      if (!tooling) continue;
      scopes.push({
        id: tool.id,
        name: tool.short,
        href: tool.href,
        subtitle: 'workspace docs',
        toolId: tool.id,
        rootItem: tooling,
        items: itemsForScope(tooling),
      });
      continue;
    }

    if (tool.id === 'cloud') {
      if (!platform) continue;
      scopes.push({
        id: tool.id,
        name: tool.short,
        href: tool.href,
        subtitle: 'managed platform',
        toolId: tool.id,
        rootItem: platform,
        items: itemsForScope(platform),
      });
      continue;
    }

    const framework = childNamed(frameworks, tool.short);
    if (!framework) continue;
    scopes.push({
      id: tool.id,
      name: tool.short,
      href: tool.href,
      subtitle: 'language docs',
      toolId: tool.id,
      rootItem: framework,
      items: itemsForScope(framework),
    });
  }

  return scopes;
}

/**
 * The scoped docs sidebar — the "un-blob".
 *
 * Rather than presenting the entire docs catalog as one flat tree, the sidebar
 * scopes to the section the reader is in: a switcher jumps between top-level
 * sections, and the active section's tree is shown with the larger groups
 * collapsed by default (progressive disclosure). Falls back to listing every
 * section when the path matches none (e.g. an unknown URL).
 */
export function Sidebar({ navItems, currentPath = '', ...props }: SidebarProps) {
  const scopes = buildDocScopes(navItems);
  const activeScope = scopes.find((scope) => scopeHasActive(scope, currentPath));
  const scopeItems = activeScope ? itemsForSidebarScope(activeScope, navItems) : [];
  const totalPages = activeScope
    ? countScopePages(activeScope)
    : scopes.reduce((sum, scope) => sum + countScopePages(scope), 0);

  return (
    <Box as='aside' width='260px' flexShrink={0} {...props}>
      {/* Scope switcher: which section am I in, and how do I jump elsewhere */}
      <Switcher>
        <summary>
          {activeScope?.toolId ? <ToolGlyph tool={activeScope.toolId} size={28} /> : <SectionGlyph />}
          <span className='scope-titles'>
            <span className='scope-name'>{activeScope ? activeScope.name : 'Documentation'}</span>
            <span className='scope-sub'>{activeScope ? activeScope.subtitle : 'browse all'}</span>
          </span>
          <ChevronDown />
        </summary>
        <div className='switch-menu' role='menu'>
          {scopes.map((scope) => (
            <a
              key={scope.id}
              href={scope.href}
              role='menuitem'
              data-active={activeScope === scope}
              className='switch-row'
            >
              <span>{scope.name}</span>
              <span className='switch-count'>{countScopePages(scope)}</span>
            </a>
          ))}
        </div>
      </Switcher>

      <PageCount>{pageCountLabel(totalPages)}</PageCount>

      <Box as='nav' aria-label={activeScope ? `${activeScope.name} navigation` : 'Documentation navigation'}>
        {activeScope
          ? scopeItems.map((item) => (
              <NavNode
                key={`${item.name}-${item.contentPath ?? item.order}`}
                item={item}
                currentPath={currentPath}
                depth={0}
              />
            ))
          : scopes.map((scope) => (
              <LeafLink key={scope.id} href={scope.href} data-active={false} style={{ paddingLeft: 10 }}>
                {scope.name}
              </LeafLink>
            ))}
      </Box>
    </Box>
  );
}

function NavNode({ item, currentPath, depth }: { item: NavItem; currentPath: string; depth: number }) {
  const itemPath = toUrlPath(item.contentPath);
  const isActive = itemPath !== '#' && currentPath === itemPath;
  const children = item.children ?? [];

  if (children.length === 0) {
    return (
      <LeafLink href={itemPath} data-active={isActive} style={{ paddingLeft: `${10 + depth * 12}px` }}>
        {item.name}
      </LeafLink>
    );
  }

  // Progressive disclosure: open groups that hold the active page, plus small
  // groups; collapse the large ones (the bulk of reference) by default.
  const pages = countPages(item);
  const open =
    subtreeHasActive(item, currentPath) || DEFAULT_OPEN_GROUPS.has(item.name) || pages <= (depth === 0 ? 6 : 4);

  return (
    <Group open={open} style={{ paddingLeft: depth > 0 ? '8px' : undefined }}>
      <summary>
        <ChevronRight className='grp-chevron' />
        {itemPath === '#' ? (
          <span className='grp-label'>{item.name}</span>
        ) : (
          <GroupLink href={itemPath} data-active={isActive} onClick={(e) => e.stopPropagation()}>
            {item.name}
          </GroupLink>
        )}
        <span className='grp-count'>{pages}</span>
      </summary>
      <div className='grp-children'>
        {children.map((child) => (
          <NavNode
            key={`${child.name}-${child.contentPath ?? child.order}`}
            item={child}
            currentPath={currentPath}
            depth={depth + 1}
          />
        ))}
      </div>
    </Group>
  );
}

function SectionGlyph() {
  return (
    <span
      aria-hidden='true'
      style={{
        width: 28,
        height: 28,
        flex: '0 0 28px',
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        borderRadius: 'var(--radius-md)',
        background: 'var(--color-surface)',
        border: '1px solid var(--color-border)',
        color: 'var(--color-text-muted)',
      }}
    >
      <svg
        width='15'
        height='15'
        viewBox='0 0 24 24'
        fill='none'
        stroke='currentColor'
        strokeWidth='2'
        strokeLinecap='round'
        strokeLinejoin='round'
        aria-hidden='true'
      >
        <rect width='7' height='7' x='3' y='3' rx='1' />
        <rect width='7' height='7' x='14' y='3' rx='1' />
        <rect width='7' height='7' x='14' y='14' rx='1' />
        <rect width='7' height='7' x='3' y='14' rx='1' />
      </svg>
    </span>
  );
}

function ChevronDown() {
  return (
    <svg
      className='scope-chevron'
      width='15'
      height='15'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='m6 9 6 6 6-6' />
    </svg>
  );
}

function ChevronRight({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      width='14'
      height='14'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='m9 18 6-6-6-6' />
    </svg>
  );
}

const Switcher = styled.details`
  position: relative;

  & > summary {
    list-style: none;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px;
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-md);
    cursor: pointer;
    transition: border-color var(--transition-fast);
  }

  & > summary::-webkit-details-marker {
    display: none;
  }

  & > summary:hover {
    border-color: var(--color-text-dim);
  }

  .scope-titles {
    flex: 1;
    min-width: 0;
  }

  .scope-name {
    display: block;
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--color-text);
    line-height: 1.2;
  }

  .scope-sub {
    display: block;
    font-size: 0.72rem;
    color: var(--color-text-muted);
  }

  .scope-chevron {
    color: var(--color-text-dim);
    flex-shrink: 0;
  }

  &[open] .scope-chevron {
    transform: rotate(180deg);
  }

  .switch-menu {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    right: 0;
    z-index: 30;
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-xl);
    padding: 6px;
  }

  .switch-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 7px 10px;
    border-radius: var(--radius-md);
    font-size: 0.82rem;
    color: var(--color-text);
    text-decoration: none;
  }

  .switch-row:hover {
    background: var(--color-surface-hover);
  }

  .switch-row[data-active='true'] {
    color: var(--color-primary);
    font-weight: 600;
  }

  .switch-count {
    font-size: 0.7rem;
    font-family: var(--font-mono);
    color: var(--color-text-dim);
  }
`;

const PageCount = styled.div`
  margin: 10px 2px 16px;
  font-size: 0.72rem;
  color: var(--color-text-dim);
  font-family: var(--font-mono);
`;

const LeafLink = styled.a`
  display: block;
  padding: 5px 9px;
  border-radius: var(--radius-md);
  font-size: 0.82rem;
  line-height: 1.4;
  text-decoration: none;
  color: var(--color-text-muted);
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
    color: var(--color-text);
  }

  &[data-active='true'] {
    color: var(--color-primary);
    background: color-mix(in srgb, var(--color-primary) 10%, transparent);
    font-weight: 600;
  }
`;

const GroupLink = styled.a`
  flex: 1;
  font-size: 0.85rem;
  font-weight: 600;
  color: var(--color-text);
  text-decoration: none;

  &[data-active='true'] {
    color: var(--color-primary);
  }
`;

const Group = styled.details`
  margin-bottom: 2px;

  & > summary {
    list-style: none;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 8px;
    border-radius: var(--radius-md);
    cursor: pointer;
    transition: background var(--transition-fast);
  }

  & > summary::-webkit-details-marker {
    display: none;
  }

  & > summary:hover {
    background: var(--color-surface-hover);
  }

  .grp-chevron {
    color: var(--color-text-dim);
    flex-shrink: 0;
    transition: transform var(--transition-fast);
  }

  &[open] > summary .grp-chevron {
    transform: rotate(90deg);
  }

  .grp-label {
    flex: 1;
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .grp-count {
    font-size: 0.65rem;
    font-family: var(--font-mono);
    color: var(--color-text-muted);
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-sm);
    padding: 0 6px;
    line-height: 1.5;
  }

  .grp-children {
    margin-left: 14px;
    padding-left: 8px;
    border-left: 1px solid var(--color-border);
  }
`;
