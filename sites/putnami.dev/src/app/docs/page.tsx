import { Box, styled } from '@putnami/ui';
import { Link, page, useLoaderData } from '@putnami/web';
import { PageMeta } from '../../components/page-meta';
import { StatusDot, ToolGlyph } from '../../components/tool-glyph';
import { frameworkStartList, pageCountLabel, toolList } from '../../lib/tools';
import type { DocsHubData } from './loader';
import HubSearchIsland from './search-trigger.island';

const START_CARDS = [
  {
    href: '/docs/getting-started',
    code: 'WS',
    title: 'Workspace start',
    desc: 'Install the CLI, create a workspace, and run your first project.',
  },
  ...frameworkStartList().map((tool) => ({
    href: tool.startHref ?? tool.href,
    code: tool.glyph,
    title: `${tool.short} start`,
    desc: tool.startBlurb ?? tool.blurb,
  })),
];

// The cross-cutting model pages, in reading order: the bet, the layers, the
// contracts that make the layers checkable, the loop that holds code to those
// contracts, and what all of it enables.
// They are not product surfaces — no glyph, no page count, no support subject —
// so they get their own row above the surface grid rather than a card inside it.
const MODEL_CARDS = [
  {
    href: '/docs/why',
    icon: <TargetIcon />,
    title: 'Why Putnami',
    desc: 'The bet, what it costs, and who it is not for.',
  },
  {
    href: '/docs/concepts',
    icon: <CompassIcon />,
    title: 'Concepts & principles',
    desc: 'The workspace model and the constraints Putnami refuses to break.',
  },
  {
    href: '/docs/protocols',
    icon: <LayersIcon />,
    title: 'Protocols',
    desc: 'The wire contracts every pillar reads, and how conformance is proven.',
  },
  {
    href: '/docs/concepts/spec-driven-development',
    icon: <ShieldCheckIcon />,
    title: 'Spec-driven development',
    desc: 'Features, specs, and architecture contracts (ARC & DARC) — intent as data, checked by the gate.',
  },
  {
    href: '/docs/agents',
    icon: <BotIcon />,
    title: 'Agents',
    desc: 'Orientation, blast radius, and the contract that bounds what an agent can do.',
  },
];

// The docs "front door": a hub of tool cards + a start-here path, not a wall of
// links. You choose context (a tool, or the cross-cutting concepts) before you
// see depth — the scoped sidebar only appears once you're inside a section.
export default page()
  .static({ revalidate: { seconds: 3600, tags: ['docs'] } })
  .render(() => {
    const { toolStatus } = useLoaderData<DocsHubData>();
    return (
      <Box as='main'>
        <PageMeta
          title='Documentation — Putnami'
          description='One site, every Putnami surface. Start with the workspace model, then choose Tooling, TypeScript, Go, experimental Python, or the Platform.'
          url='/docs'
        />

        <Hub>
          {/* Hero */}
          <header className='hub-hero'>
            <span className='kicker'>Documentation</span>
            <h1>One site. Pick the surface you are working in.</h1>
            <p className='lede'>
              Putnami documents a polyglot system in one place. Start with the shared model, then narrow the docs to
              Tooling, TypeScript, Go, experimental Python, or the Platform.
            </p>
            <HubSearchIsland />
          </header>

          {/* New here? Start with a concrete path. */}
          <section className='start-path' aria-label='Getting started'>
            <div className='start-head'>
              <BoltIcon />
              <span className='start-title'>New to Putnami? Pick a first path.</span>
              <Link to='/docs/getting-started' className='start-link'>
                global quickstart →
              </Link>
            </div>
            <div className='start-steps'>
              {START_CARDS.map((step) => (
                <Link key={step.href} to={step.href} className='start-step'>
                  <span className='step-num'>{step.code}</span>
                  <span>
                    <span className='step-title'>{step.title}</span>
                    <span className='step-desc'>{step.desc}</span>
                  </span>
                </Link>
              ))}
            </div>
          </section>

          {/* The cross-cutting model: read once, applies to every surface below. */}
          <section aria-label='The system model'>
            <div className='section-row'>
              <h2>Understand the system</h2>
              <span className='section-hint'>read once; applies everywhere</span>
            </div>
            <div className='model-grid'>
              {MODEL_CARDS.map((card) => (
                <Link key={card.href} to={card.href} className='model-card'>
                  <span className='concepts-glyph'>{card.icon}</span>
                  <span className='mc-name'>{card.title}</span>
                  <span className='mc-desc'>{card.desc}</span>
                </Link>
              ))}
            </div>
          </section>

          {/* Choose a product surface */}
          <section aria-label='Product surfaces'>
            <div className='section-row'>
              <h2>Choose your surface</h2>
              <span className='section-hint'>the sidebar narrows after this choice</span>
            </div>
            <div className='tool-grid'>
              {toolList().map((tool) => (
                <Link key={tool.id} to={tool.href} className='tool-card'>
                  <div className='tc-head'>
                    <ToolGlyph tool={tool} size={42} />
                    <div className='tc-titles'>
                      <div className='tc-name'>{tool.name}</div>
                      <div className='tc-status'>
                        <StatusDot status={toolStatus[tool.id]} />
                      </div>
                    </div>
                    <ArrowUpRight />
                  </div>
                  <p className='tc-blurb'>{tool.blurb}</p>
                  <div className='tc-foot'>
                    <span className='tc-pages'>{pageCountLabel(tool.pages)}</span>
                    <span className='tc-go'>Open docs →</span>
                  </div>
                </Link>
              ))}
            </div>
          </section>

          {/* Agent strip */}
          <section className='agent-strip' aria-label='For agents'>
            <div className='agent-copy'>
              <div className='agent-head'>
                <BotIcon />
                <span>Readable by agents, by design</span>
              </div>
              <p>
                Stable URL prefixes per surface. Every page offers <strong>Copy as Markdown</strong> and a
                machine-readable <code>llms.txt</code>. The structure agents read is the structure you read.
              </p>
              <p>
                The system itself is contracted the same way — orientation, blast radius, and what a tool is allowed to
                mutate. <Link to='/docs/agents'>See how agents operate a workspace →</Link>
              </p>
              <p>
                Python documentation is experimental: the extension requires an explicit workspace opt-in, is never
                enabled by default, and carries no Go or TypeScript parity promise.
              </p>
            </div>
            <pre className='code-block agent-urls'>{`putnami.dev/docs/tooling-&-workspace/...
putnami.dev/docs/frameworks/typescript/...
putnami.dev/docs/frameworks/go/...
putnami.dev/docs/frameworks/python/...
putnami.dev/llms.txt`}</pre>
          </section>

          {/* What each badge above actually promises. */}
          <section className='support-note' aria-label='Support status'>
            <p>
              The badge on each card is that surface&apos;s reviewed public support status. What every status commits
              to, and the full package and protocol catalog, is on <Link to='/docs/support'>Support status</Link>.
            </p>
          </section>
        </Hub>
      </Box>
    );
  });

function BoltIcon() {
  return (
    <svg
      width='16'
      height='16'
      viewBox='0 0 24 24'
      fill='none'
      stroke='var(--color-primary)'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='m12 3-1.9 5.8a2 2 0 0 1-1.287 1.288L3 12l5.8 1.9a2 2 0 0 1 1.288 1.287L12 21l1.9-5.8a2 2 0 0 1 1.287-1.288L21 12l-5.8-1.9a2 2 0 0 1-1.288-1.287Z' />
    </svg>
  );
}

function CompassIcon() {
  return (
    <svg
      width='20'
      height='20'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <circle cx='12' cy='12' r='10' />
      <polygon points='16.24 7.76 14.12 14.12 7.76 16.24 9.88 9.88 16.24 7.76' />
    </svg>
  );
}

function TargetIcon() {
  return (
    <svg
      width='20'
      height='20'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <circle cx='12' cy='12' r='9' />
      <circle cx='12' cy='12' r='4.5' />
      <circle cx='12' cy='12' r='0.8' fill='currentColor' />
    </svg>
  );
}

function LayersIcon() {
  return (
    <svg
      width='20'
      height='20'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <polygon points='12 2 22 8.5 12 15 2 8.5 12 2' />
      <polyline points='2 15.5 12 22 22 15.5' />
    </svg>
  );
}

function ShieldCheckIcon() {
  return (
    <svg
      width='20'
      height='20'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M12 2 4 5.5v5.7c0 5 3.4 8.6 8 10.3 4.6-1.7 8-5.3 8-10.3V5.5L12 2' />
      <polyline points='8.5 12 11 14.5 15.5 9.5' />
    </svg>
  );
}

function ArrowUpRight() {
  return (
    <svg
      className='tc-arrow'
      width='17'
      height='17'
      viewBox='0 0 24 24'
      fill='none'
      stroke='currentColor'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M7 7h10v10' />
      <path d='M7 17 17 7' />
    </svg>
  );
}

function BotIcon() {
  return (
    <svg
      width='17'
      height='17'
      viewBox='0 0 24 24'
      fill='none'
      stroke='var(--color-primary)'
      strokeWidth='2'
      strokeLinecap='round'
      strokeLinejoin='round'
      aria-hidden='true'
    >
      <path d='M12 8V4H8' />
      <rect width='16' height='12' x='4' y='8' rx='2' />
      <path d='M2 14h2' />
      <path d='M20 14h2' />
      <path d='M15 13v2' />
      <path d='M9 13v2' />
    </svg>
  );
}

const Hub = styled.div`
  max-width: 1200px;

  .hub-hero {
    margin-bottom: var(--space-2xl);
  }

  .kicker {
    display: block;
    font-size: var(--fs-sm);
    font-weight: 500;
    color: var(--color-primary);
    text-transform: uppercase;
    letter-spacing: 0.06em;
    margin-bottom: 10px;
  }

  .hub-hero h1 {
    font-size: var(--fs-4xl);
    letter-spacing: -0.02em;
    margin: 0 0 12px;
    max-width: 680px;
  }

  .lede {
    font-size: var(--fs-lg);
    color: var(--color-text-muted);
    max-width: 600px;
    line-height: 1.6;
    margin-bottom: var(--space-lg);
  }

  /* Start path */
  .start-path {
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    overflow: hidden;
    background: var(--color-surface);
  }

  .start-head {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--color-border);
  }

  .start-title {
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .start-link {
    margin-left: auto;
    font-size: 0.82rem;
    color: var(--color-primary);
    text-decoration: none;
    font-weight: 500;
  }

  .start-steps {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .start-step {
    display: flex;
    gap: 12px;
    padding: 16px;
    border-top: 1px solid var(--color-border);
    color: inherit;
    text-decoration: none;
    transition: background var(--transition-fast);
  }

  .start-step:nth-child(-n + 2) {
    border-top: none;
  }

  .start-step:nth-child(odd) {
    border-right: 1px solid var(--color-border);
  }

  .start-step:hover {
    background: var(--color-bg);
  }

  .step-num {
    width: 30px;
    height: 30px;
    flex: 0 0 30px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-md);
    background: var(--color-bg);
    border: 1px solid var(--color-border);
    color: var(--color-primary);
    font-family: var(--font-mono);
    font-weight: 600;
  }

  .step-title {
    display: block;
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .step-desc {
    display: block;
    font-size: 0.8rem;
    color: var(--color-text-muted);
    margin-top: 2px;
  }

  /* Tool grid */
  section {
    margin-top: var(--space-2xl);
  }

  .section-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    margin-bottom: var(--space-md);
  }

  .section-row h2 {
    font-size: var(--fs-xl);
    margin: 0;
  }

  .section-hint {
    font-size: 0.8rem;
    color: var(--color-text-dim);
  }

  .tool-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: var(--space-md);
  }

  .model-grid {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: var(--space-sm);
  }

  .model-card {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: var(--space-md);
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-md);
    text-decoration: none;
    transition: all var(--transition-fast);
  }

  .model-card:hover {
    border-color: var(--color-text-dim);
    box-shadow: var(--shadow-sm);
  }

  .mc-name {
    font-size: 0.92rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .mc-desc {
    font-size: 0.82rem;
    line-height: 1.5;
    color: var(--color-text-muted);
  }

  .tool-card {
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 18px;
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    text-decoration: none;
    transition: all var(--transition-fast);
  }

  .tool-card:hover {
    border-color: var(--color-text-dim);
    box-shadow: var(--shadow-sm);
  }

  .tool-card:hover .tc-arrow {
    color: var(--color-primary);
    transform: translate(2px, -2px);
  }

  .concepts-card {
    background: var(--color-bg);
    border-style: dashed;
  }

  .tc-head {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .tc-titles {
    flex: 1;
    min-width: 0;
  }

  .tc-name {
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--color-text);
    line-height: 1.2;
  }

  .tc-status,
  .tc-cross {
    margin-top: 4px;
  }

  .tc-cross {
    font-size: 0.75rem;
    color: var(--color-text-dim);
  }

  .concepts-glyph {
    width: 42px;
    height: 42px;
    flex: 0 0 42px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-md);
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    color: var(--color-text-muted);
  }

  .tc-arrow {
    color: var(--color-text-dim);
    transition: all var(--transition-fast);
  }

  .tc-blurb {
    font-size: 0.82rem;
    color: var(--color-text-muted);
    line-height: 1.55;
    margin: 0;
    flex: 1;
  }

  .tc-foot {
    display: flex;
    align-items: center;
    gap: 8px;
    padding-top: 10px;
    border-top: 1px solid var(--color-border);
  }

  .tc-pages {
    font-size: 0.75rem;
    color: var(--color-text-dim);
    font-family: var(--font-mono);
  }

  .tc-go {
    margin-left: auto;
    font-size: 0.8rem;
    color: var(--color-text-muted);
    font-weight: 500;
  }

  .tc-go.primary {
    margin-left: 0;
    color: var(--color-primary);
  }

  .tool-card:hover .tc-go {
    color: var(--color-primary);
  }

  /* Agent strip */
  .agent-strip {
    display: flex;
    align-items: center;
    gap: var(--space-lg);
    padding: var(--space-lg);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-lg);
    background: var(--color-surface);
  }

  .agent-copy {
    flex: 1;
    min-width: 0;
  }

  .agent-head {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 6px;
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .agent-copy p {
    font-size: 0.82rem;
    color: var(--color-text-muted);
    line-height: 1.55;
    margin: 0;
    max-width: 520px;
  }

  .agent-copy code {
    font-size: 0.78rem;
  }

  .agent-urls {
    margin: 0;
    font-size: 0.72rem;
    flex: 0 0 auto;
    max-width: 340px;
  }

  .support-note p {
    margin: 0;
    font-size: 0.82rem;
    color: var(--color-text-muted);
    line-height: 1.55;
  }

  .support-note a {
    color: var(--color-primary);
  }

  @media (max-width: 1024px) {
    .model-grid {
      grid-template-columns: repeat(3, 1fr);
    }
  }

  @media (max-width: 640px) {
    .hub-hero h1 {
      font-size: var(--fs-3xl);
    }

    /* Heading and hint fight for one line on phones; stack them. */
    .section-row {
      flex-direction: column;
      align-items: flex-start;
      gap: 2px;
    }
  }

  @media (max-width: 768px) {
    .tool-grid {
      grid-template-columns: 1fr;
    }
    .model-grid {
      grid-template-columns: 1fr;
    }
    .start-steps {
      grid-template-columns: 1fr;
    }
    .start-step:nth-child(-n + 2) {
      border-top: 1px solid var(--color-border);
    }
    .start-step:first-child {
      border-top: none;
    }
    .start-step {
      border-right: none;
    }
    .agent-strip {
      flex-direction: column;
      align-items: flex-start;
    }
    .agent-urls {
      max-width: 100%;
      width: 100%;
    }
  }
`;
