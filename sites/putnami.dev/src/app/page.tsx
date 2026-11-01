import { Box, Button, Flex, Grid, Heading, Link, styled, Text } from '@putnami/ui';
import { page, useT } from '@putnami/web';
import { PageMeta } from '../components/page-meta';
import { ToolGlyph } from '../components/tool-glyph';
import { toolList } from '../lib/tools';
import HeroTerminalIsland from './hero-terminal.island';
import home from './home.messages';

type HomeKey = keyof (typeof home)['definitions'];

/**
 * Each row is a wire protocol: you declare intent next to the code, Putnami
 * derives a document, and a named consumer reads it. The agent-context row is
 * marked `derived` because it is the one input nobody writes — it falls out of
 * the rows above it, which is the whole argument of the page. A file name is
 * code, not copy, so it is not translated.
 */
const CHAIN: { declare: HomeKey | { code: string }; derives: HomeKey; reads: HomeKey; derived?: boolean }[] = [
  { declare: { code: 'putnami.json' }, derives: 'chain1Derives', reads: 'chain1Reads' },
  { declare: 'chain2Declare', derives: 'chain2Derives', reads: 'chain2Reads' },
  { declare: 'chain3Declare', derives: 'chain3Derives', reads: 'chain3Reads' },
  { declare: { code: 'putnami.ci.json' }, derives: 'chain4Derives', reads: 'chain4Reads' },
  { declare: 'chain5Declare', derives: 'chain5Derives', reads: 'chain5Reads' },
  { declare: 'chain6Declare', derives: 'chain6Derives', reads: 'chain6Reads', derived: true },
];

const PRINCIPLES: { title: HomeKey; desc: HomeKey }[] = [
  { title: 'principle1Title', desc: 'principle1Desc' },
  { title: 'principle2Title', desc: 'principle2Desc' },
  { title: 'principle3Title', desc: 'principle3Desc' },
  { title: 'principle4Title', desc: 'principle4Desc' },
  { title: 'principle5Title', desc: 'principle5Desc' },
  { title: 'principle6Title', desc: 'principle6Desc' },
];

const PAINS: { title: HomeKey; desc: HomeKey }[] = [
  { title: 'pain1Title', desc: 'pain1Desc' },
  { title: 'pain2Title', desc: 'pain2Desc' },
  { title: 'pain3Title', desc: 'pain3Desc' },
  { title: 'pain4Title', desc: 'pain4Desc' },
];

// Product names are not translated.
const SURFACES: { key: string; name: string; desc: HomeKey }[] = [
  { key: 'putnami.dev', name: 'Putnami', desc: 'surfaceDev' },
  { key: 'putnami.cloud', name: 'Putnami Cloud', desc: 'surfaceCloud' },
  { key: 'intelligence', name: 'Putnami Intelligence', desc: 'surfaceIntelligence' },
];

// Pre-rendered at build (SSG) and revalidated on the cloud runtime — by TTL and
// when the `docs` tag is revalidated (the shared navbar reflects the docs tree).
// The page ships zero base JS; only the hero terminal hydrates (when visible).
export default page()
  .static({ revalidate: { seconds: 3600, tags: ['docs'] } })
  .render(() => {
    const t = useT(home);
    const text = (key: HomeKey) => (t[key] as () => string)();
    return (
      <Box as='main' pt='var(--space-2xl)' pb='var(--space-3xl)'>
        <PageMeta
          title={t.metaTitle()}
          description={t.description()}
          url='/'
          jsonLd={{
            '@context': 'https://schema.org',
            '@type': 'WebSite',
            name: 'Putnami',
            url: 'https://putnami.dev',
            description: t.description(),
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
            {t.heroPill()}
          </HeroPill>
          <Heading level={1} size={['2xl', '3xl', '4xl']} mb='md' style={{ letterSpacing: '-0.02em' }}>
            {t.heroTitle()}
          </Heading>
          <Text as='p' size='lg' color='secondary' mx='auto' mb='lg' maxWidth='640px' style={{ lineHeight: 1.6 }}>
            {t.heroLede()}
          </Text>
          <Flex justify='center' align='center' wrap='wrap' gap='lg'>
            <Link to='/docs/getting-started'>
              <Button size='lg'>{t.getStarted()}</Button>
            </Link>
            <SecondaryLink to='/docs/why'>{t.whyPutnami()}</SecondaryLink>
          </Flex>
        </Box>

        {/* Quick start visual */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='760px' mx='auto' px='var(--space-md)'>
          <HeroTerminalIsland />
        </Box>

        {/* The problem */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.problemLabel()}</SectionLabel>
          <SectionHeading level={2} mb='md'>
            {t.problemTitle()}
          </SectionHeading>
          <Grid columns={[1, 2]} gap='md'>
            {PAINS.map((pain) => (
              <PainCard key={pain.title} title={text(pain.title)} desc={text(pain.desc)} />
            ))}
          </Grid>
        </Box>

        {/* The chain */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.chainLabel()}</SectionLabel>
          <SectionHeading level={2} mb='md'>
            {t.chainTitle()}
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            {t.chainLede()}
          </Text>
          <ChainTable>
            <table>
              <thead>
                <tr>
                  <th>{t.chainDeclare()}</th>
                  <th>{t.chainDerives()}</th>
                  <th>{t.chainReads()}</th>
                </tr>
              </thead>
              <tbody>
                {CHAIN.map((row) => (
                  <tr key={row.derives} className={row.derived ? 'derived' : undefined}>
                    <td data-label={t.chainDeclare()}>
                      {typeof row.declare === 'string' ? text(row.declare) : row.declare.code}
                    </td>
                    <td data-label={t.chainDerives()}>{text(row.derives)}</td>
                    <td data-label={t.chainReads()}>{text(row.reads)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ChainTable>
          <Text as='p' color='secondary' mt='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            <strong>{t.branchLead()}</strong> {t.branchBody()}
          </Text>
          <Caption as='p' mt='md'>
            {t.chainCaption()} <SecondaryLink to='/docs/protocols'>{t.readProtocols()}</SecondaryLink>
          </Caption>
        </Box>

        {/* Agent-operable */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.operateLabel()}</SectionLabel>
          <SectionHeading level={2} mb='sm'>
            {t.operateTitle()}
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            {t.operateLede()}
          </Text>
          <Grid columns={[1, 2]} gap='md'>
            <SurfaceCard title='putnami context pack'>
              {t.contextPackBefore()} <strong>{t.contextPackStrong()}</strong> {t.contextPackAfter()}
            </SurfaceCard>
            <SurfaceCard title='--impacted'>{t.impacted()}</SurfaceCard>
            <SurfaceCard title='--output=json · jsonl'>{t.outputModes()}</SurfaceCard>
            <SurfaceCard title='putnami doctor'>{t.doctor()}</SurfaceCard>
          </Grid>
          <Caption as='p' mt='md'>
            {t.operateCaption()}
          </Caption>
        </Box>

        {/* Protocols */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.protocolsLabel()}</SectionLabel>
          <SectionHeading level={2} mb='md'>
            {t.protocolsTitle()}
          </SectionHeading>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            {t.protocolsLede()}
          </Text>
          <Pullquote>{t.protocolsQuote()}</Pullquote>
          <Text as='p' color='secondary' mb='md' maxWidth='680px' style={{ lineHeight: 1.6 }}>
            {t.matrixBefore()} <strong>{t.matrixTested()}</strong>, <strong>{t.matrixAligned()}</strong>{' '}
            {t.matrixAlignedNote()} <strong>{t.matrixNotConsumer()}</strong>
            {t.matrixAfter()}
          </Text>
          <Flex align='center' wrap='wrap' gap='lg'>
            <SecondaryLink to='/docs/protocols'>{t.readProtocols()}</SecondaryLink>
            <SecondaryLink to='/docs/support'>{t.supportStatus()}</SecondaryLink>
          </Flex>
        </Box>

        {/* Principles */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.principlesLabel()}</SectionLabel>
          <SectionHeading level={2} mb='md'>
            {t.principlesTitle()}
          </SectionHeading>
          <Grid columns={[1, 2]} gap='md'>
            {PRINCIPLES.map((principle) => (
              <PrincipleItem key={principle.title} title={text(principle.title)} desc={text(principle.desc)} />
            ))}
          </Grid>
          <Box mt='md'>
            <SecondaryLink to='/docs/principles'>{t.readPrinciples()}</SecondaryLink>
          </Box>
        </Box>

        {/* Three surfaces */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <SectionLabel>{t.surfacesLabel()}</SectionLabel>
          <SectionHeading level={2} mb='md'>
            {t.surfacesTitle()}
          </SectionHeading>
          <Grid columns={[1, 3]} gap='md'>
            {SURFACES.map((surface) => (
              <Card key={surface.key}>
                <SurfaceKey>{surface.key}</SurfaceKey>
                <Heading level={3} size='md' mb='sm'>
                  {surface.name}
                </Heading>
                <Text color='secondary' size='sm' style={{ lineHeight: 1.6 }}>
                  {text(surface.desc)}
                </Text>
              </Card>
            ))}
          </Grid>
          <Caption as='p' mt='md'>
            {t.surfacesCaption()}
          </Caption>
        </Box>

        {/* One docs model, every surface — ties the home into the docs IA */}
        <Box as='section' mb='var(--space-3xl)' maxWidth='1000px' mx='auto' px='var(--space-md)'>
          <Box textAlign='center' mb='lg'>
            <SectionLabel as='span'>{t.docsLabel()}</SectionLabel>
            <SectionHeading level={2} mb='sm'>
              {t.docsTitle()}
            </SectionHeading>
          </Box>
          <ToolStrip>
            {toolList().map((tool) => (
              <Link key={tool.id} to={tool.href} className='tool-cell'>
                <ToolGlyph tool={tool} size={40} />
                <span className='tool-name'>{tool.short}</span>
                <span className='tool-pages'>{t.pageCount({ count: tool.pages })}</span>
              </Link>
            ))}
          </ToolStrip>
          <Box textAlign='center' mt='md'>
            <Link to='/docs' style={{ color: 'var(--color-primary)', fontWeight: 500, textDecoration: 'none' }}>
              {t.openDocs()}
            </Link>
          </Box>
        </Box>

        {/* Final CTA */}
        <Box as='section' textAlign='center' maxWidth='760px' mx='auto' px='var(--space-md)' pt='var(--space-xl)'>
          <Heading level={2} mb='sm'>
            {t.ctaTitle()}
          </Heading>
          <Text as='p' color='secondary' mb='md'>
            {t.ctaLede()}
          </Text>
          <Flex justify='center' align='center' wrap='wrap' gap='lg'>
            <Link to='/docs/getting-started'>
              <Button size='lg'>{t.getStarted()}</Button>
            </Link>
            <SecondaryLink to='https://github.com/putnami/putnami'>{t.viewOnGithub()}</SecondaryLink>
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
