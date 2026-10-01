import { Box, styled } from '@putnami/ui';
import { Link, page } from '@putnami/web';
import { PageMeta } from '../../components/page-meta';

/**
 * The site's privacy notice.
 *
 * It exists because the site measures its own audience. The measurement is
 * cookieless and exempt from consent, but never from information: a visitor
 * has to be able to read what is recorded, how long it is kept, and how to
 * object, without a banner and without asking.
 *
 * Every sentence here describes what `@putnami/analytics` actually does with
 * the configuration in `conf/`. `test/privacy.test.ts` pins the declared
 * action vocabulary against the list below, so a new tracked action cannot
 * ship without appearing on this page.
 */
export default page()
  .static()
  .render(() => (
    <Box as='main'>
      <PageMeta
        title='Privacy — Putnami'
        description='What putnami.dev measures, how visitors are counted without cookies, how long it is kept, and how to object.'
        url='/privacy'
      />
      <Prose>
        <h1>Privacy</h1>
        <p className='lede'>
          This site measures how it is used. It sets no cookie for that, sends nothing to a third party, and asks you
          for nothing. Here is exactly what happens.
        </p>

        <h2>Who is responsible</h2>
        <p>
          Fabien Dumay is the controller for the audience measurement described here. For access, rectification,
          erasure, or objection, write to <a href='mailto:contact@putnami.com'>contact@putnami.com</a>.
        </p>

        <h2>Why we measure</h2>
        <p>
          To see which documentation pages are read, which searches find nothing, and which platforms the CLI is
          downloaded for. The purpose is to operate and improve this site. We build no advertising profile, and we
          neither sell nor share this data.
        </p>

        <h2>What we collect</h2>
        <p>For each page you open, we record:</p>
        <ul>
          <li>the route and the path of the page;</li>
          <li>
            the site you arrived from, and the five <code>utm_*</code> parameters of the address you arrived with;
          </li>
          <li>how long the page was visible;</li>
          <li>
            your browser and operating-system family, your device type, your browser language, and a coarse window-size
            class.
          </li>
        </ul>
        <p>Beyond page views, three actions are recorded, and only these three:</p>
        <ul>
          <li>
            <code>docs_search_open</code> — you opened a documentation page from the search palette, with the number of
            results the search returned;
          </li>
          <li>
            <code>doc_copy_markdown</code> — you copied a page as Markdown, and from where;
          </li>
          <li>
            <code>cli_download</code> — a CLI artifact was resolved for download through this site, with its platform,
            architecture, and release channel.
          </li>
        </ul>

        <h2>What we never collect</h2>
        <p>
          Not your IP address. Not your full browser identification string. Not what you typed in the search box, nor
          the contents of any other field. No page title, no token, no e-mail address. This site has no accounts, so
          nothing here is ever attached to an identity.
        </p>

        <h2>How you are counted</h2>
        <p>
          No measurement cookie is placed. To count you once rather than many times, the server computes a short code
          from your network address, your browser family, and a secret key that changes every day at midnight UTC. Your
          network address is never written down: it exists for the instant the code is computed. Because the key changes
          daily, today's code cannot be linked to yesterday's.
        </p>

        <h2>What is kept in your browser</h2>
        <p>Three things, all first-party, none readable by another site:</p>
        <ul>
          <li>
            <code>_csrf</code>, a cookie that protects form submissions from being forged. It is strictly necessary,
            carries no identity, and disappears when you close the browser.
          </li>
          <li>
            a visit identifier in <em>session storage</em>, so several pages read in a row count as one visit. It is
            erased when the tab closes and is never linked to a previous visit.
          </li>
          <li>
            a small queue of measurement events in <em>local storage</em>, held only until they are sent. It exists so a
            page you close on a flaky connection is not counted twice.
          </li>
        </ul>

        <h2>Lawful basis</h2>
        <p>
          Measuring our own site, on our own servers, without cross-site tracking, is carried out in our legitimate
          interest in operating it, and falls within the audience-measurement exemption. We therefore do not ask for
          your consent, and the site works identically whether or not you send an opt-out signal.
        </p>

        <h2>Opt-out signals</h2>
        <p>
          If your browser sends <code>Sec-GPC: 1</code> (Global Privacy Control) or <code>DNT: 1</code> (Do Not Track),
          no measurement cookie is ever placed for you. Since this site places none in the first place, the signals
          change nothing here — they are honoured all the same.
        </p>

        <h2>How long we keep it</h2>
        <p>Individual events are deleted after 90 days. Daily totals, which identify nobody, are kept for 760 days.</p>

        <h2>Where it is stored</h2>
        <p>
          In this site's own database, which we operate. No third party receives it. Nothing is sent to an analytics
          service, and the site loads no third-party measurement script.
        </p>

        <h2>Your rights</h2>
        <p>
          You may ask for a copy of the data held about you, ask for it to be corrected or erased, or object to this
          processing. Write to <a href='mailto:contact@putnami.com'>contact@putnami.com</a>. Bear in mind what the
          sections above describe: outside the day it was collected, we hold nothing that can be traced back to you —
          the daily code cannot be reversed, and the key that produced it is gone. Clearing this site's browser storage
          removes the visit identifier and the pending queue at any time.
        </p>
        <p>
          You may also complain to your national supervisory authority — in France, the CNIL (Commission Nationale de
          l'Informatique et des Libertés).
        </p>

        <h2>How this works</h2>
        <p>
          The measurement is <Link to='/docs/frameworks/typescript/analytics'>a Putnami framework package</Link>,
          running inside this site. What it records, where it lands, and the decisions behind it are documented on that
          page.
        </p>
      </Prose>
    </Box>
  ));

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
