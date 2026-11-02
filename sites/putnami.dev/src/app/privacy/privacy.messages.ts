import { messages } from '@putnami/web';

/**
 * The privacy notice, one message per paragraph or list item. `<code>`,
 * `<em>`, `<mail>` and `<link>` are tags the page renders; a translation
 * keeps them and states the same facts: `test/privacy.test.ts` checks the
 * action names and the retention periods in every catalog.
 */
export default messages('privacy', {
  metaTitle: 'Privacy — Putnami',
  metaDescription:
    'What putnami.dev measures, how visitors are counted without cookies, how long it is kept, and how to object.',
  title: 'Privacy',
  lede: 'This site measures how it is used. It sets no cookie for that, sends nothing to a third party, and asks you for nothing. Here is exactly what happens.',

  controllerTitle: 'Who is responsible',
  controller:
    'Fabien Dumay is the controller for the audience measurement described here. For access, rectification, erasure, or objection, write to <mail>contact@putnami.com</mail>.',

  purposeTitle: 'Why we measure',
  purpose:
    'To see which documentation pages are read, which searches find nothing, and which platforms the CLI is downloaded for. The purpose is to operate and improve this site. We build no advertising profile, and we neither sell nor share this data.',

  collectTitle: 'What we collect',
  collectIntro: 'For each page you open, we record:',
  collectRoute: 'the route and the path of the page;',
  collectReferrer:
    'the site you arrived from, and the five <code>utm_*</code> parameters of the address you arrived with;',
  collectVisible: 'how long the page was visible;',
  collectDevice:
    'your browser and operating-system family, your device type, your browser language, and a coarse window-size class.',
  actionsIntro: 'Beyond page views, three actions are recorded, and only these three:',
  actionSearch:
    '<code>docs_search_open</code> — you opened a documentation page from the search palette, with the number of results the search returned;',
  actionCopy: '<code>doc_copy_markdown</code> — you copied a page as Markdown, and from where;',
  actionDownload:
    '<code>cli_download</code> — a CLI artifact was resolved for download through this site, with its platform, architecture, and release channel.',

  neverTitle: 'What we never collect',
  never:
    'Not your IP address. Not your full browser identification string. Not what you typed in the search box, nor the contents of any other field. No page title, no token, no e-mail address. This site has no accounts, so nothing here is ever attached to an identity.',

  countedTitle: 'How you are counted',
  counted:
    "No measurement cookie is placed. To count you once rather than many times, the server computes a short code from your network address, your browser family, and a secret key that changes every day at midnight UTC. Your network address is never written down: it exists for the instant the code is computed. Because the key changes daily, today's code cannot be linked to yesterday's.",

  browserTitle: 'What is kept in your browser',
  browserIntro: 'Three things, all first-party, none readable by another site:',
  browserCsrf:
    '<code>_csrf</code>, a cookie that protects form submissions from being forged. It is strictly necessary, carries no identity, and disappears when you close the browser.',
  browserVisit:
    'a visit identifier in <em>session storage</em>, so several pages read in a row count as one visit. It is erased when the tab closes and is never linked to a previous visit.',
  browserQueue:
    'a small queue of measurement events in <em>local storage</em>, held only until they are sent. It exists so a page you close on a flaky connection is not counted twice.',

  basisTitle: 'Lawful basis',
  basis:
    'Measuring our own site, on our own servers, without cross-site tracking, is carried out in our legitimate interest in operating it, and falls within the audience-measurement exemption. We therefore do not ask for your consent, and the site works identically whether or not you send an opt-out signal.',

  signalsTitle: 'Opt-out signals',
  signals:
    'If your browser sends <code>Sec-GPC: 1</code> (Global Privacy Control) or <code>DNT: 1</code> (Do Not Track), no measurement cookie is ever placed for you. Since this site places none in the first place, the signals change nothing here — they are honoured all the same.',

  retentionTitle: 'How long we keep it',
  retention: 'Individual events are deleted after 90 days. Daily totals, which identify nobody, are kept for 760 days.',

  storageTitle: 'Where it is stored',
  storage:
    "In this site's own database, which we operate. No third party receives it. Nothing is sent to an analytics service, and the site loads no third-party measurement script.",

  rightsTitle: 'Your rights',
  rights:
    "You may ask for a copy of the data held about you, ask for it to be corrected or erased, or object to this processing. Write to <mail>contact@putnami.com</mail>. Bear in mind what the sections above describe: outside the day it was collected, we hold nothing that can be traced back to you — the daily code cannot be reversed, and the key that produced it is gone. Clearing this site's browser storage removes the visit identifier and the pending queue at any time.",
  complaint:
    "You may also complain to your national supervisory authority — in France, the CNIL (Commission Nationale de l'Informatique et des Libertés).",

  howTitle: 'How this works',
  how: 'The measurement is <link>a Putnami framework package</link>, running inside this site. What it records, where it lands, and the decisions behind it are documented on that page.',
});
