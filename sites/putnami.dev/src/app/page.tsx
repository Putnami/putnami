import { Box, Button, Flex, Grid, Heading, Link, styled, Text } from '@putnami/ui';
import { page } from '@putnami/web';
import { PageMeta } from '../components/page-meta';
import { ToolGlyph } from '../components/tool-glyph';
import { pageCountLabel, toolList } from '../lib/tools';
import HeroTerminalIsland from './hero-terminal.island';

const DESCRIPTION =
  'A workspace model, framework primitives for Go and TypeScript, and a layer of wire protocols underneath them. Your build, your CI, your infrastructure and your agents read the same contracts.';

/**
 * Each row is a wire protocol: you declare intent next to the code, Putnami
 * derives a document, and a named consumer reads it. The agent-context row is
 * marked `derived` because it is the one input nobody writes — it falls out of
 * the rows above it, which is the whole argument of the page.
 */
const CHAIN = [
  {
    declare: 'putnami.json',
    derives: 'the workspace graph',
    reads: 'the CLI, every extension, every task',
  },
  {
    declare: 'an HTTP handler',
    derives: 'a route inventory',
    reads: 'your edge, as a generated default-deny allowlist',
  },
  {
    declare: 'an infra need, next to the code',
    derives: 'one merged manifest, with provenance',
    reads: 'your deploy target',
  },
  {
    declare: 'putnami.ci.json',
    derives: 'a validated job graph',
    reads: 'any execution plane — it reads the document, it never widens it',
  },
  {
    declare: 'a migration, a repository',
    derives: 'a bundle digest',
    reads: 'Go and TypeScript adapters, the test provisioner',
  },
  {
    declare: '— nothing. it’s derived',
    derives: 'the agent context',
    reads: 'your agent, over MCP',
    derived: true,
  },
];

const PRINCIPLES = [
  {
    title: 'Observable by construction',
    desc: 'Every runtime emits structured signals by default. Local, preview and production expose the same shape.',
  },
  {
    title: 'Performance is a constraint',
    desc: 'Defaults must survive production load. Regressions are detectable in CI. Abstractions expose their cost.',
  },
  {
    title: 'Deterministic and reviewable',
    desc: 'Git defines system intent. Deployments are pure functions of versioned state. Drift is detectable.',
  },
  {
    title: 'Security is foundational',
    desc: 'Secure defaults are enforced. The system fails closed. Unsafe paths require explicit acknowledgment.',
  },
  {
    title: 'Data ownership is non-negotiable',
    desc: 'Data lives in stores you control. Schemas are versioned, portable, and part of the contract.',
  },
  {
    title: 'Automation is a first-class user',
    desc: 'The CLI is the primary interface. Output is deterministic and machine-readable, so humans and machines follow the same rules.',
  },
];

const SURFACES = [
  {
    key: 'putnami.dev',
    name: 'Putnami',
    desc: 'Structural precision. The workspace model, the framework primitives, the CLI, and the protocol layer underneath them. Complete locally, no account required. FSL-1.1-MIT.',
  },
  {
    key: 'putnami.cloud',
    name: 'Putnami Cloud',
    desc: 'Operational depth on the same documents: release provenance, runtime correlation, incident context, rollout protection. It reads your route inventory to build an allowlist, your infra manifest to provision, your CI document to execute. It gets no private dialect.',
  },
  {
    key: 'intelligence',
    name: 'Putnami Intelligence',
    desc: 'What ships today: agent orientation over MCP, a versioned workspace index, and spec-driven development — features, specs and architecture recorded next to the code. The review-and-audit loop, with freshness and receipts on every finding, is where this is going. It stands alone: useful with neither Putnami frameworks nor Putnami Cloud.',
  },
];

// Pre-rendered at build (SSG) and revalidated on the cloud runtime — by TTL and
// when the `docs` tag is revalidated (the shared navbar reflects the docs tree).
// The page ships zero base JS; only the hero terminal hydrates (when visible).
export default page()
  .static({ revalidate: { seconds: 3600, tags: ['docs'] } })
  .render(() => {
    return (
      <Box as='main' pt='var(--space-2xl)' pb='var(--space-3xl)'>
        <PageMeta
          title='Putnami — Declare the system once'
          description={DESCRIPTION}
          url='/'
          jsonLd={{
            '@context': 'https://schema.org',
            '@type': 'WebSite',
            name: 'Putnami',
            url: 'https://putnami.dev',
            description: DESCRIPTION,
            publisher: {
              '@type': 'Organization',
              name: 'Putnami',
              url: 'https://putnami.dev',
              logo: {
                '@type': 'ImageObject',
                url: 'https://putnami.dev/assets/putnami-logo.png',
              },
            },
          }}
        />

        {/* Hero */}
        <Box as='section' textAlign='center' mb='var(--space-3xl)' px='var(--space-md)'>
          <HeroPill>
            <span className='pill-dot' />
            Workspace · Protocols · Agent-operable
          </HeroPill>
          <Heading level={1} size={['2xl', '3xl', '4xl']} mb='md' style={{ letterSpacing: '-0.02em' }}>
            Declare the system once. Humans review it. Agents operate it.
          </Heading>
          <Text as='p' size='lg' color='secondary' mx='auto' mb='lg' maxWidth='640px' style={{ lineHeight: 1.6 }}>
            Putnami is a workspace model, framework primitives for Go and TypeScript, and a layer of wire protocols
            underneath them. Your build, your CI, your infrastructure and your agents all read the same contracts — from
            your laptop to production.
          </Text>
          <Flex justify='center' align='center' wrap='wrap' gap='lg'>
            <Link to='/docs/getting-started'>
              <Button size='lg'>Get started</Button>
            </Link>
            <SecondaryLink to='/docs/why'>Why Putnami →</SecondaryLink>
          </Flex>
        </Box>

        {/* Quick start visual */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='760px' mx='auto' px='var(--space-md)'>
          <HeroTerminalIsland />
        </Box>

        {/* The problem */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>Why this exists</SectionLabel>
          <SectionHeading level={2} mb='md'>
            Your system is real. Your description of it isn’t.
          </SectionHeading>
          <Grid columns={[1, 2]} gap='md'>
            <PainCard
              title='The system only exists at runtime'
              desc='Config in five dialects, infrastructure in another repo, CI in provider YAML. Nothing in git states what the system is — only fragments of how to rebuild it, if you already know.'
            />
            <PainCard
              title='Every boundary is a convention'
              desc='The CLI knows what the framework emits because the same person wrote both. Add a language, a second consumer, or an agent, and the convention lives only in someone’s head.'
            />
            <PainCard
              title='Drift isn’t unfixed. It’s invisible.'
              desc='Local, CI and production diverge quietly, because nothing in the system compares them. You find out at the worst possible moment.'
            />
            <PainCard
              title='Agents can edit. They can’t operate.'
              desc='An agent can write code against your repo. Operating your system — knowing the blast radius, reading the state, undoing a mistake — needs a contract, and you don’t have one to hand it.'
            />
          </Grid>
        </Box>

        {/* The chain */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>How it works</SectionLabel>
          <SectionHeading level={2} mb='md'>
            One declaration. Everything downstream reads it.
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            You write intent next to the code it describes. Putnami derives the rest, deterministically, and publishes
            it as a contract anyone can parse — including the tools you didn’t write.
          </Text>
          <ChainTable>
            <table>
              <thead>
                <tr>
                  <th>You declare</th>
                  <th>Putnami derives</th>
                  <th>Who reads it</th>
                </tr>
              </thead>
              <tbody>
                {CHAIN.map((row) => (
                  <tr key={row.derives} className={row.derived ? 'derived' : undefined}>
                    <td data-label='You declare'>{row.declare}</td>
                    <td data-label='Putnami derives'>{row.derives}</td>
                    <td data-label='Who reads it'>{row.reads}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ChainTable>
          <Text as='p' color='secondary' mt='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            <strong>Your branch is already an environment.</strong> Git carries the intent — what changed, by whom,
            against what — and every document above is derived from it, so one workspace structure, one set of commands
            and one set of contracts hold from your laptop through CI to whatever runs it. Managed per-branch previews
            and deploys are where this is going; nothing in the chain requires an account.
          </Text>
          <Caption as='p' mt='md'>
            Every row above is a wire protocol: a published schema, a corpus of valid and invalid fixtures, and a strict
            parser in each language that implements it.{' '}
            <SecondaryLink to='/docs/protocols'>Read the protocols →</SecondaryLink>
          </Caption>
        </Box>

        {/* Agent-operable */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>Built to be operated</SectionLabel>
          <SectionHeading level={2} mb='sm'>
            An agent doesn’t need to be taught your repository.
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            It reads the same contracts your build reads.
          </Text>
          <Grid columns={[1, 2]} gap='md'>
            <SurfaceCard title='putnami context pack'>
              A deterministic per-project orientation document: identity and dependency graph, composition roots,
              capability and contract references, representative source ranges, tests, docs, provenance. Aggregated{' '}
              <strong>by reference</strong> — paths, digests and ranges, never file contents — behind a fail-closed
              publish-safety gate. Served read-only over MCP.
            </SurfaceCard>
            <SurfaceCard title='--impacted'>
              The blast radius of a change, before anything acts on it. An agent that knows what it is about to affect
              is an agent you can let run.
            </SurfaceCard>
            <SurfaceCard title='--output=json · jsonl'>
              Exit codes, output modes and machine documents are a versioned contract, not an implementation detail that
              moves under a parser. The same corpus validates the Go and the TypeScript CLI.
            </SurfaceCard>
            <SurfaceCard title='putnami doctor'>
              Production-readiness findings under a deployment profile, a frozen check-code taxonomy, and a remediation
              baked into every check. “What’s wrong and what do I do about it” is data, not prose.
            </SurfaceCard>
          </Grid>
          <Caption as='p' mt='md'>
            One JSONL event stream per job, so the agent watching a build and the human reading the log are parsing the
            same lines.
          </Caption>
        </Box>

        {/* Protocols */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>Why you can check any of this</SectionLabel>
          <SectionHeading level={2} mb='md'>
            One package per boundary. No shared assumptions.
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            The frameworks, the tooling and the platform don’t integrate through shared code. They integrate through
            protocols: a published schema, a corpus of valid and invalid fixtures, a strict parser in every language
            that implements it, and at least one real consumer.
          </Text>
          <Pullquote>A protocol without consumers is a spec, not a contract.</Pullquote>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            The repository publishes a conformance matrix — who implements or consumes each contract, and how it’s
            verified. It has three states, and the middle one is published on purpose: <strong>tested</strong>,{' '}
            <strong>aligned by hand</strong> (drift possible), and <strong>not a consumer</strong>. A matrix that only
            showed green would be marketing.
          </Text>
          <Flex align='center' wrap='wrap' gap='lg'>
            <SecondaryLink to='/docs/protocols'>Read the protocols →</SecondaryLink>
            <SecondaryLink to='/docs/support'>Support status →</SecondaryLink>
          </Flex>
        </Box>

        {/* Principles */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>Principles</SectionLabel>
          <SectionHeading level={2} mb='md'>
            Constraints, not aspirations. Violations are bugs.
          </SectionHeading>
          <Grid columns={[1, 2]} gap='md'>
            {PRINCIPLES.map((principle) => (
              <PrincipleItem key={principle.title} title={principle.title} desc={principle.desc} />
            ))}
          </Grid>
          <Box mt='md'>
            <SecondaryLink to='/docs/principles'>Read the full principles →</SecondaryLink>
          </Box>
        </Box>

        {/* Three surfaces */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>The shape of the product</SectionLabel>
          <SectionHeading level={2} mb='md'>
            Three surfaces. One declaration.
          </SectionHeading>
          <Grid columns={[1, 3]} gap='md'>
            {SURFACES.map((surface) => (
              <Card key={surface.key}>
                <SurfaceKey>{surface.key}</SurfaceKey>
                <Heading level={3} size='md' mb='sm'>
                  {surface.name}
                </Heading>
                <Text color='secondary' size='sm' style={{ lineHeight: 1.6 }}>
                  {surface.desc}
                </Text>
              </Card>
            ))}
          </Grid>
          <Caption as='p' mt='md'>
            Adopting putnami.dev sharpens what Intelligence knows about your structure. Adopting Cloud deepens what it
            knows about your operations. Neither is a prerequisite — and Cloud integrates through the same published
            contracts as any third-party adapter. Its advantage is zero-configuration correlation, not a private
            backchannel.
          </Caption>
        </Box>

        {/* One docs model, every surface — ties the home into the docs IA */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <Box textAlign='center' mb='lg'>
            <SectionLabel as='span'>One docs model, every surface</SectionLabel>
            <SectionHeading level={2} mb='sm'>
              Tooling first. Language depth when you need it.
            </SectionHeading>
          </Box>
          <ToolStrip>
            {toolList().map((tool) => (
              <Link key={tool.id} to={tool.href} className='tool-cell'>
                <ToolGlyph tool={tool} size={40} />
                <span className='tool-name'>{tool.short}</span>
                <span className='tool-pages'>{pageCountLabel(tool.pages)}</span>
              </Link>
            ))}
          </ToolStrip>
          <Box textAlign='center' mt='md'>
            <Link to='/docs' style={{ color: 'var(--color-primary)', fontWeight: 500, textDecoration: 'none' }}>
              Open the documentation hub →
            </Link>
          </Box>
        </Box>

        {/* Final CTA */}
        <Box as='section' textAlign='center' maxWidth='760px' mx='auto' px='var(--space-md)' pt='var(--space-xl)'>
          <Heading level={2} mb='sm'>
            Start with a workspace.
          </Heading>
          <Text as='p' color='secondary' mb='md'>
            Everything above runs on your machine, without an account. The platform is there when operating it stops
            being your job.
          </Text>
          <Flex justify='center' align='center' wrap='wrap' gap='lg'>
            <Link to='/docs/getting-started'>
              <Button size='lg'>Get started</Button>
            </Link>
            <SecondaryLink to='https://github.com/putnami/putnami'>View on GitHub</SecondaryLink>
          </Flex>
        </Box>
      </Box>
    );
  });

const SectionLabel = styled(Text)`
  font-size: var(--fs-sm);
  font-weight: 500;
  color: var(--color-primary);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin-bottom: var(--space-sm);
`;

const SecondaryLink = styled(Link)`
  color: var(--color-text);
  text-decoration: underline;
  text-decoration-color: var(--color-border);
  text-underline-offset: 3px;
  font-weight: 500;
  transition: text-decoration-color var(--transition-fast);

  &:hover {
    text-decoration-color: var(--color-text);
  }
`;

const SectionHeading = styled(Heading)`
  margin-top: 0;
  margin-bottom: 1em;
`;

const Caption = styled(Text)`
  font-size: var(--fs-sm);
  color: var(--color-text-muted);
  line-height: 1.6;
`;

const HeroPill = styled.div`
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 5px 12px;
  margin-bottom: var(--space-lg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-xl);
  font-size: 0.8rem;
  color: var(--color-text-muted);

  .pill-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--color-primary);
  }
`;

const ToolStrip = styled.div`
  display: grid;
  grid-template-columns: repeat(5, 1fr);
  gap: var(--space-sm);

  @media (max-width: 640px) {
    grid-template-columns: repeat(2, 1fr);

    /* An odd cell count would strand the last tool beside an empty slot. */
    .tool-cell:nth-child(odd):last-child {
      grid-column: 1 / -1;
    }
  }

  .tool-cell {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
    padding: var(--space-md) var(--space-sm);
    background: var(--color-surface);
    border: 1px solid var(--color-border);
    border-radius: var(--radius-md);
    text-decoration: none;
    transition: all var(--transition-fast);
  }

  .tool-cell:hover {
    border-color: var(--color-text-dim);
    box-shadow: var(--shadow-sm);
  }

  .tool-name {
    font-size: 0.8rem;
    font-weight: 600;
    color: var(--color-text);
  }

  .tool-pages {
    font-size: 0.7rem;
    font-family: var(--font-mono);
    color: var(--color-text-dim);
  }
`;

/**
 * The derived row is tinted rather than annotated: it is the only row whose
 * first cell has no author, and the page loses its point if that reads as one
 * more line in a list.
 */
const ChainTable = styled.div`
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  overflow: hidden;

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--fs-sm);
  }

  th {
    text-align: left;
    padding: var(--space-sm) var(--space-md);
    background: var(--color-surface-hover);
    font-size: 0.7rem;
    letter-spacing: 0.07em;
    text-transform: uppercase;
    color: var(--color-text-muted);
    font-weight: 600;
  }

  td {
    padding: var(--space-sm) var(--space-md);
    border-top: 1px solid var(--color-border);
    vertical-align: top;
    color: var(--color-text-muted);
    line-height: 1.5;
  }

  td:first-of-type {
    color: var(--color-text);
    font-family: var(--font-mono);
    font-size: 0.83rem;
  }

  tr.derived td {
    background: color-mix(in srgb, var(--color-primary) 8%, transparent);
  }

  tr.derived td:first-of-type {
    color: var(--color-primary);
    font-family: inherit;
    font-style: italic;
    font-size: var(--fs-sm);
  }

  /* Three text-heavy columns cannot share a phone screen; each row becomes a
     stacked card with its column header repeated from the data-label. */
  @media (max-width: 640px) {
    thead {
      display: none;
    }

    table,
    tbody,
    tr,
    td {
      display: block;
      width: 100%;
    }

    tr {
      padding: var(--space-sm) var(--space-md);
      border-top: 1px solid var(--color-border);
    }

    tbody tr:first-of-type {
      border-top: none;
    }

    td,
    td:first-of-type {
      padding: 0;
      border-top: none;
    }

    td + td {
      margin-top: 10px;
    }

    td::before {
      content: attr(data-label);
      display: block;
      margin-bottom: 1px;
      font-family: var(--font-sans);
      font-size: 0.65rem;
      font-style: normal;
      font-weight: 600;
      letter-spacing: 0.07em;
      text-transform: uppercase;
      color: var(--color-text-dim);
    }

    tr.derived,
    tr.derived td {
      background: none;
    }

    tr.derived {
      background: color-mix(in srgb, var(--color-primary) 8%, transparent);
    }
  }
`;

const Pullquote = styled.blockquote`
  margin: 0 0 var(--space-md);
  padding: var(--space-sm) var(--space-md);
  border-left: 3px solid var(--color-primary);
  background: color-mix(in srgb, var(--color-primary) 8%, transparent);
  border-radius: 0 var(--radius-md) var(--radius-md) 0;
  font-size: var(--fs-md);
  font-weight: 500;
  color: var(--color-text);
`;

const Card = styled(Box)`
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  padding: var(--space-md);
`;

const SurfaceKey = styled.span`
  display: block;
  margin-bottom: var(--space-xs);
  font-family: var(--font-mono);
  font-size: 0.72rem;
  color: var(--color-primary);
`;

const CardCode = styled.code`
  font-family: var(--font-mono);
  font-size: 0.9em;
`;

function SurfaceCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card>
      <Heading level={3} size='md' mb='sm'>
        <CardCode>{title}</CardCode>
      </Heading>
      <Text color='secondary' size='sm' style={{ lineHeight: 1.6 }}>
        {children}
      </Text>
    </Card>
  );
}

function PainCard({ title, desc }: { title: string; desc: string }) {
  return (
    <Pain>
      <Heading level={3} size='md' mb='sm'>
        {title}
      </Heading>
      <Text color='secondary' size='sm' style={{ lineHeight: 1.6 }}>
        {desc}
      </Text>
    </Pain>
  );
}

const Pain = styled(Box)`
  background: transparent;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  padding: var(--space-md);
`;

function PrincipleItem({ title, desc }: { title: string; desc: string }) {
  return (
    <Box>
      <Text as='p' size='md' mb='xs'>
        <strong>{title}.</strong>{' '}
        <Text as='span' color='secondary'>
          {desc}
        </Text>
      </Text>
    </Box>
  );
}
