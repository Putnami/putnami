import { Box, styled } from '@putnami/ui';
import type { ReactNode } from 'react';
import { Link, page, useT } from '@putnami/web';
import { PageMeta } from '../../components/page-meta';
import privacy from './privacy.messages';

/**
 * The site's privacy notice.
 *
 * It exists because the site measures its own audience. The measurement is
 * cookieless and exempt from consent, but never from information: a visitor
 * has to be able to read what is recorded, how long it is kept, and how to
 * object, without a banner and without asking.
 *
 * Every sentence in `privacy.messages.ts` describes what `@putnami/analytics`
 * actually does with the configuration in `conf/`. `test/privacy.test.ts`
 * pins the declared action vocabulary against those messages and each
 * catalog, so a new tracked action cannot ship without appearing on this page.
 */
const code = (chunks: ReactNode) => <code>{chunks}</code>;
const em = (chunks: ReactNode) => <em>{chunks}</em>;
const mail = (chunks: ReactNode) => <a href='mailto:contact@putnami.com'>{chunks}</a>;
const link = (chunks: ReactNode) => <Link to='/docs/frameworks/typescript/analytics'>{chunks}</Link>;

function Privacy() {
  const t = useT(privacy);
  return (
    <Box as='main'>
      <PageMeta title={t.metaTitle()} description={t.metaDescription()} url='/privacy' />
      <Prose>
        <h1>{t.title()}</h1>
        <p className='lede'>{t.lede()}</p>

        <h2>{t.controllerTitle()}</h2>
        <p>{t.controller({ mail })}</p>

        <h2>{t.purposeTitle()}</h2>
        <p>{t.purpose()}</p>

        <h2>{t.collectTitle()}</h2>
        <p>{t.collectIntro()}</p>
        <ul>
          <li>{t.collectRoute()}</li>
          <li>{t.collectReferrer({ code })}</li>
          <li>{t.collectVisible()}</li>
          <li>{t.collectDevice()}</li>
        </ul>
        <p>{t.actionsIntro()}</p>
        <ul>
          <li>{t.actionSearch({ code })}</li>
          <li>{t.actionCopy({ code })}</li>
          <li>{t.actionDownload({ code })}</li>
        </ul>

        <h2>{t.neverTitle()}</h2>
        <p>{t.never()}</p>

        <h2>{t.countedTitle()}</h2>
        <p>{t.counted()}</p>

        <h2>{t.browserTitle()}</h2>
        <p>{t.browserIntro()}</p>
        <ul>
          <li>{t.browserCsrf({ code })}</li>
          <li>{t.browserVisit({ em })}</li>
          <li>{t.browserQueue({ em })}</li>
        </ul>

        <h2>{t.basisTitle()}</h2>
        <p>{t.basis()}</p>

        <h2>{t.signalsTitle()}</h2>
        <p>{t.signals({ code })}</p>

        <h2>{t.retentionTitle()}</h2>
        <p>{t.retention()}</p>

        <h2>{t.storageTitle()}</h2>
        <p>{t.storage()}</p>

        <h2>{t.rightsTitle()}</h2>
        <p>{t.rights({ mail })}</p>
        <p>{t.complaint()}</p>

        <h2>{t.howTitle()}</h2>
        <p>{t.how({ link })}</p>
      </Prose>
    </Box>
  );
}

export default page()
  .static()
  .render(() => <Privacy />);

const Prose = styled.article`
  max-width: 46rem;
  margin: 0 auto;
  padding: var(--space-3xl) var(--space-lg);
  color: var(--color-text);
  line-height: 1.7;

  h1 {
    font-size: 2.5rem;
    margin-bottom: var(--space-lg);
  }

  h2 {
    font-size: 1.25rem;
    margin-top: var(--space-2xl);
    margin-bottom: var(--space-sm);
  }

  p,
  ul {
    margin-bottom: var(--space-md);
  }

  ul {
    padding-left: var(--space-lg);
  }

  li {
    margin-bottom: var(--space-xs);
  }

  .lede {
    font-size: 1.125rem;
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: 0.9em;
    background: var(--color-surface);
    border-radius: 4px;
    padding: 0.1em 0.35em;
  }

  a {
    color: var(--color-primary);
    text-decoration: none;

    &:hover {
      text-decoration: underline;
    }
  }
`;
