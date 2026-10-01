'use client';

import {
  Box,
  Button,
  Container,
  Drawer,
  DrawerBody,
  DrawerCloseButton,
  DrawerFooter,
  DrawerHeader,
  Flex,
  styled,
  ThemeToggle,
  Tooltip,
} from '@putnami/ui';
import { useState } from 'react';
import type { NavItem } from '../lib/docs/navigation';
import { SITE_MAX_WIDTH } from '../theme';
import { frameworkStartList, TOOLS, toolList } from '../lib/tools';
import { PutnamiLogo } from './putnami-logo';
import { Sidebar } from './sidebar';
import { ToolGlyph } from './tool-glyph';

const START_LINKS = [
  { label: 'Docs hub', href: '/docs', desc: 'Choose a path' },
  { label: 'Workspace start', href: '/docs/getting-started', desc: 'Install, create, run' },
];

// The cross-cutting model pages: version-independent and surface-independent.
// They answer "what is this system" rather than "how do I do X in language Y",
// so they sit apart from both the start paths and the per-surface entries.
const SYSTEM_LINKS = [
  { label: 'Why Putnami', href: '/docs/why' },
  { label: 'Concepts', href: '/docs/concepts' },
  { label: 'Principles', href: '/docs/principles' },
  { label: 'Protocols', href: '/docs/protocols' },
  { label: 'Agents', href: '/docs/agents' },
];

const GUIDE_LINKS = [
  { label: 'Build a web app', href: '/docs/how-to/build-a-web-app' },
  { label: 'Build an API service', href: '/docs/how-to/build-an-api-service' },
  { label: 'Share code', href: '/docs/how-to/share-code-between-projects' },
  { label: 'Configure your app', href: '/docs/how-to/configure-your-app' },
  { label: 'Add persistence', href: '/docs/how-to/add-persistence' },
  { label: 'Develop with AI', href: '/docs/how-to/develop-with-ai' },
];

// Router-free shell: the navbar hydrates as an isolated island root with no
// React Router context, so links are plain anchors (full-page navigation, which
// is correct for a zero-base-JS static site) and the active doc path is read
// from the live location rather than a router hook.
export function Navbar({ navItems }: { navItems?: NavItem[] }) {
  const [isMobileMenuOpen, setIsMobileMenuOpen] = useState(false);
  const mobileMenuId = 'mobile-nav-drawer';
  const currentPath = typeof window !== 'undefined' ? window.location.pathname : '';

  return (
    <StyledNav>
      <Container maxWidth={SITE_MAX_WIDTH}>
        <Flex as='nav' aria-label='Navigation' alignItems='center' style={{ height: '64px' }}>
          <Flex alignItems='center' gap='sm'>
            <a
              href='/'
              style={{ display: 'flex', alignItems: 'center', color: 'var(--color-text)', textDecoration: 'none' }}
              onClick={() => setIsMobileMenuOpen(false)}
            >
              <PutnamiLogo height={28} />
            </a>
          </Flex>
          <Flex flex='1' alignItems='center' justifyContent='flex-end' gap='md'>
            {/* Desktop Navigation — needs ~950px for its full item row, so it
                only appears from the lg breakpoint; below that the drawer
                carries the same links. */}
            <Flex display={['none', 'none', 'none', 'flex']} alignItems='center' gap='md'>
              <StyledNavLink href='/docs/why'>Why</StyledNavLink>
              <NavDropdown>
                <summary>
                  Docs
                  <ChevronDownIcon />
                </summary>
                <div className='docs-menu-panel'>
                  <div className='menu-section'>
                    <span className='menu-heading'>Start</span>
                    {START_LINKS.map((link) => (
                      <a key={link.href} href={link.href} className='menu-link'>
                        <span className='menu-title'>{link.label}</span>
                        <span className='menu-desc'>{link.desc}</span>
                      </a>
                    ))}
                    {frameworkStartList().map((tool) => (
                      <a key={tool.id} href={tool.startHref ?? tool.href} className='menu-link'>
                        <span className='menu-title'>{tool.short} start</span>
                        <span className='menu-desc'>{tool.startBlurb}</span>
                      </a>
                    ))}
                  </div>
                  <div className='menu-section'>
                    <span className='menu-heading'>Choose a surface</span>
                    {toolList().map((tool) => (
                      <a key={tool.id} href={tool.href} className='menu-link surface-row'>
                        <ToolGlyph tool={tool} size={28} />
                        <span className='menu-text'>
                          <span className='menu-title'>{tool.short}</span>
                          <span className='menu-desc'>{tool.name}</span>
                        </span>
                      </a>
                    ))}
                  </div>
                  <div className='menu-section'>
                    <span className='menu-heading'>Common guides</span>
                    {GUIDE_LINKS.map((link) => (
                      <a key={link.href} href={link.href} className='menu-link compact'>
                        {link.label}
                      </a>
                    ))}
                    <span className='menu-heading menu-heading-stacked'>The system model</span>
                    {SYSTEM_LINKS.map((link) => (
                      <a key={link.href} href={link.href} className='menu-link compact'>
                        {link.label}
                      </a>
                    ))}
                  </div>
                </div>
              </NavDropdown>
              <StyledNavLink href='/docs/protocols'>Protocols</StyledNavLink>
              <StyledNavLink href='/docs/agents'>Agents</StyledNavLink>
              <StyledNavLink href={TOOLS['tooling'].href}>{TOOLS['tooling'].short}</StyledNavLink>
              {/* Languages share one trigger. Python is listed inside — a listing
                  may present it (its blurb states experimental), but an anchor
                  beside TypeScript and Go would imply a parity the workspace does
                  not offer: `python-is-browseable-not-anchored` in the spec. */}
              <NavDropdown>
                <summary>
                  Frameworks
                  <ChevronDownIcon />
                </summary>
                <div className='fw-menu-panel'>
                  {frameworkStartList().map((tool) => (
                    <a key={tool.id} href={tool.href} className='menu-link surface-row'>
                      <ToolGlyph tool={tool} size={28} />
                      <span className='menu-text'>
                        <span className='menu-title'>{tool.short}</span>
                        <span className='menu-desc'>{tool.blurb}</span>
                      </span>
                    </a>
                  ))}
                  <a href='/docs/frameworks' className='menu-link compact'>
                    All frameworks →
                  </a>
                </div>
              </NavDropdown>
              <StyledNavLink href={TOOLS['cloud'].href}>{TOOLS['cloud'].short}</StyledNavLink>
              <SearchButton
                type='button'
                onClick={() => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true }))}
                aria-label='Search documentation'
              >
                <SearchNavIcon />
                <span>Search</span>
                <SearchKbd>
                  <kbd>⌘</kbd>
                  <kbd>K</kbd>
                </SearchKbd>
              </SearchButton>
              <ThemeToggle />
              <Tooltip label='View on GitHub'>
                <a
                  href='https://github.com/putnami/putnami'
                  target='_blank'
                  rel='noopener noreferrer'
                  aria-label='GitHub'
                  style={{ display: 'flex', color: 'inherit' }}
                >
                  <GitHubIcon />
                </a>
              </Tooltip>
            </Flex>

            {/* Mobile Navigation Trigger */}
            <Flex display={['flex', 'flex', 'flex', 'none']} alignItems='center' gap='sm'>
              <Button
                variant='ghost'
                size='sm'
                type='button'
                aria-label='Search'
                onClick={() => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true }))}
              >
                <SearchNavIcon />
              </Button>
              <Button
                variant='ghost'
                size='sm'
                type='button'
                aria-label='Open Menu'
                aria-expanded={isMobileMenuOpen}
                aria-controls={mobileMenuId}
                onClick={() => setIsMobileMenuOpen(true)}
              >
                <MenuIcon />
              </Button>
            </Flex>
          </Flex>
        </Flex>
      </Container>

      <Drawer isOpen={isMobileMenuOpen} onClose={() => setIsMobileMenuOpen(false)} placement='right' size='xs'>
        <DrawerHeader>
          <Flex justifyContent='space-between' alignItems='center' width='100%'>
            <span style={{ fontWeight: 700 }}>Menu</span>
            <DrawerCloseButton onClose={() => setIsMobileMenuOpen(false)} />
          </Flex>
        </DrawerHeader>
        <DrawerBody>
          <div id={mobileMenuId}>
            <MobileTopLinks>
              <StyledMobileNavLink href='/docs' onClick={() => setIsMobileMenuOpen(false)}>
                Docs
              </StyledMobileNavLink>
              <StyledMobileNavLink href='/docs/why' onClick={() => setIsMobileMenuOpen(false)}>
                Why Putnami
              </StyledMobileNavLink>
              <StyledMobileNavLink href='/docs/protocols' onClick={() => setIsMobileMenuOpen(false)}>
                Protocols
              </StyledMobileNavLink>
              <StyledMobileNavLink href='/docs/agents' onClick={() => setIsMobileMenuOpen(false)}>
                Agents
              </StyledMobileNavLink>
              <StyledMobileNavLink href={TOOLS['tooling'].href} onClick={() => setIsMobileMenuOpen(false)}>
                {TOOLS['tooling'].short}
              </StyledMobileNavLink>
              <StyledMobileNavLink href='/docs/frameworks' onClick={() => setIsMobileMenuOpen(false)}>
                Frameworks
              </StyledMobileNavLink>
              <StyledMobileNavLink href={TOOLS['cloud'].href} onClick={() => setIsMobileMenuOpen(false)}>
                {TOOLS['cloud'].short}
              </StyledMobileNavLink>
            </MobileTopLinks>
            {(navItems?.length ?? 0) > 0 && (
              <Box
                onClick={(e: React.MouseEvent) => {
                  if ((e.target as HTMLElement).closest('a')) {
                    setIsMobileMenuOpen(false);
                  }
                }}
              >
                <Sidebar navItems={navItems ?? []} currentPath={currentPath} width='100%' p='0' position='static' />
              </Box>
            )}
          </div>
        </DrawerBody>
        <DrawerFooter>
          <Flex width='100%' justifyContent='space-between' alignItems='center'>
            <StyledMobileNavLink href='/docs' onClick={() => setIsMobileMenuOpen(false)} style={{ padding: 0 }}>
              Docs
            </StyledMobileNavLink>
            <Flex alignItems='center' gap='md'>
              <a
                href='https://github.com/putnami/putnami'
                target='_blank'
                rel='noopener noreferrer'
                aria-label='GitHub'
                style={{ display: 'flex', color: 'inherit' }}
              >
                <GitHubIcon />
              </a>
              <ThemeToggle />
            </Flex>
          </Flex>
        </DrawerFooter>
      </Drawer>
    </StyledNav>
  );
}

const StyledNav = styled.header`
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  z-index: 100;
  background: var(--color-bg);
  @supports (background: color-mix(in srgb, red 50%, blue)) {
    background: color-mix(in srgb, var(--color-bg) 85%, transparent);
  }
  backdrop-filter: blur(12px);
  border-bottom: 1px solid var(--color-border);
`;

const StyledNavLink = styled.a`
  color: var(--color-text-muted);
  text-decoration: none;
  font-size: 0.95rem;
  font-weight: 500;
  padding: var(--space-xs) var(--space-sm);
  border-radius: var(--radius-md);
  transition: all var(--transition-fast);

  &:hover {
    color: var(--color-text);
    background: var(--color-surface-hover);
  }
`;

const NavDropdown = styled.details`
  position: relative;

  & > summary {
    list-style: none;
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--color-text-muted);
    text-decoration: none;
    font-size: 0.95rem;
    font-weight: 500;
    padding: var(--space-xs) var(--space-sm);
    border-radius: var(--radius-md);
    cursor: pointer;
    transition: all var(--transition-fast);
  }

  & > summary::-webkit-details-marker {
    display: none;
  }

  & > summary:hover {
    color: var(--color-text);
    background: var(--color-surface-hover);
  }

  &[open] > summary {
    color: var(--color-text);
    background: var(--color-surface-hover);
  }

  &[open] .docs-chevron {
    transform: rotate(180deg);
  }

  .docs-menu-panel {
    position: fixed;
    top: 58px;
    left: 50%;
    z-index: 120;
    width: min(720px, calc(100vw - 32px));
    transform: translateX(-50%);
    display: grid;
    grid-template-columns: 1fr 1.05fr 1.1fr;
    gap: 12px;
    padding: 14px;
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-xl);
  }

  /* Compact variant anchored to its trigger — used by the Frameworks menu. */
  .fw-menu-panel {
    position: absolute;
    top: calc(100% + 10px);
    left: 50%;
    z-index: 120;
    width: 300px;
    transform: translateX(-50%);
    padding: 8px;
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-xl);
  }

  .fw-menu-panel .menu-link.compact {
    padding: 8px;
  }

  .menu-section {
    min-width: 0;
  }

  .menu-heading-stacked {
    padding-top: 12px;
  }

  .menu-heading {
    display: block;
    padding: 0 8px 6px;
    font-size: 0.68rem;
    font-weight: 600;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--color-text-dim);
  }

  .menu-link {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 8px;
    border-radius: var(--radius-md);
    color: var(--color-text);
    text-decoration: none;
  }

  .menu-link:hover {
    background: var(--color-surface-hover);
  }

  .menu-link.surface-row {
    flex-direction: row;
    align-items: center;
    gap: 10px;
  }

  .menu-link.compact {
    display: block;
    font-size: 0.82rem;
    color: var(--color-text-muted);
  }

  .menu-link.compact:hover {
    color: var(--color-text);
  }

  .menu-text {
    min-width: 0;
  }

  .menu-title {
    display: block;
    font-size: 0.84rem;
    font-weight: 600;
    line-height: 1.25;
  }

  .menu-desc {
    display: block;
    margin-top: 2px;
    font-size: 0.74rem;
    color: var(--color-text-muted);
    line-height: 1.35;
  }
`;

const StyledMobileNavLink = styled.a`
  display: block;
  color: var(--color-text);
  text-decoration: none;
  font-size: 1.1rem;
  font-weight: 500;
  padding: var(--space-sm);
  border-radius: var(--radius-md);
  transition: all var(--transition-fast);

  &:hover {
    background: var(--color-surface-hover);
  }
`;

const MobileTopLinks = styled.div`
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 6px;
  margin-bottom: var(--space-md);

  /* Plain child selector: component interpolation needs the emotion babel
     plugin, which this build does not run, so \`\${StyledMobileNavLink}\` would
     silently match nothing. */
  & > a {
    font-size: 0.92rem;
    padding: 10px var(--space-sm);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-md);
    background: var(--color-surface);
  }
`;

const SearchButton = styled.button`
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 10px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  color: var(--color-text-muted);
  font-size: 0.85rem;
  font-family: var(--font-sans);
  cursor: pointer;
  transition: all var(--transition-fast);
  white-space: nowrap;

  &:hover {
    border-color: var(--color-text-muted);
    color: var(--color-text);
  }
`;

const SearchKbd = styled.span`
  display: flex;
  align-items: center;
  gap: 2px;
  margin-left: 4px;

  kbd {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 18px;
    height: 18px;
    padding: 0 3px;
    font-size: 0.7rem;
    font-family: var(--font-sans);
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    border-radius: 3px;
  }
`;

function ChevronDownIcon() {
  return (
    <svg
      className='docs-chevron'
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
      <path d='m6 9 6 6 6-6' />
    </svg>
  );
}

function SearchNavIcon() {
  return (
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
      <circle cx='11' cy='11' r='8' />
      <line x1='21' y1='21' x2='16.65' y2='16.65' />
    </svg>
  );
}

function MenuIcon() {
  return (
    <svg
      width='24'
      height='24'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
    >
      <title>Menu</title>
      <line x1='3' y1='12' x2='21' y2='12' />
      <line x1='3' y1='6' x2='21' y2='6' />
      <line x1='3' y1='18' x2='21' y2='18' />
    </svg>
  );
}

function GitHubIcon() {
  return (
    <Box
      as='svg'
      width='20px'
      height='20px'
      viewBox='0 0 24 24'
      fill='currentColor'
      role='img'
      aria-hidden='true'
      style={{ display: 'block' }}
    >
      <title>GitHub</title>
      <path d='M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z' />
    </Box>
  );
}
