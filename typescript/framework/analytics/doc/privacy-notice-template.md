# Privacy notice template

Paste this into your application's privacy page and replace every
`{{ placeholder }}`. It covers the Article 13 items of the RGPD (Règlement
Général sur la Protection des Données, the European General Data Protection
Regulation) for the default `cookieless` mode. Have your own counsel read it:
this is a starting point, not legal advice.

The wording below matches what `@putnami/analytics` does with its defaults. If
you changed `mode`, `countryHeader`, `retentionRawDays`, or
`retentionAggregateDays`, change the corresponding sentence.

---

## Audience measurement

### Who is responsible

{{ Controller legal name }}, {{ registered address }}, is the controller for
the audience measurement described here. For access, rectification, erasure, or
objection, contact {{ privacy@example.com }}.

### Why we measure

We measure how this site is used so we can see which pages are read, which
features are used, and where something is failing. The purpose is to operate
and improve this site. We do not build advertising profiles, and we do not sell
or share this data.

### What we collect

For each page you open and each action you take on this site, we record: the
page route and path, the referring site, the five `utm_*` campaign parameters
of the address you arrived with, how long the page was visible, the browser and
operating-system family, the device type, the browser language, and a coarse
window-size class.

We do **not** record your IP address, your full browser identification string,
your search terms, page titles, form contents, or the contents of any field you
type.

### How you are counted

We do not place a measurement cookie. To count you once instead of many times,
our server computes a short code from your network address, your browser
family, and a secret key that changes every day at midnight UTC. Your network
address is not kept: it exists only for the instant the code is computed.
Because the key changes every day, the code that represents you today cannot be
linked to the one from yesterday.

If you are signed in, your account identifier is recorded alongside the event so
we can answer a request about your own data.

### What is kept in your browser

Two first-party keys, neither readable by another site: the identifier of your
current visit, in session storage, erased when you close the tab; and the
measurement events not yet sent to us, in local storage, cleared as they leave.
They exist so that several pages read in a row count as one visit and a page
closed on a flaky connection is not counted twice. Clearing this site's browser
storage removes both.

### Lawful basis

Audience measurement of our own site, on our own servers, without cross-site
tracking, is carried out in our legitimate interest in operating this site, and
falls within the exemption for audience-measurement analytics. We therefore do
not ask for your consent for it, and this site works identically whether or not
you send an opt-out signal.

### Opt-out signals

If your browser sends `Sec-GPC: 1` (Global Privacy Control) or `DNT: 1` (Do Not
Track), we never place any measurement cookie for you.

### How long we keep it

Individual events are deleted after {{ 90 }} days. Daily totals, which identify
nobody, are kept for {{ 760 }} days.

### Where it is stored, and who can see it

Everything described here is stored in our own database, at {{ hosting region
}}. No third party receives it, and it is not transferred outside
{{ jurisdiction }}.

### Your rights

You may ask us for a copy of the data we hold about you, ask us to correct or
erase it, or object to this processing. Write to {{ privacy@example.com }}.

If you are signed in, we retrieve your data by your account identifier. If you
are not signed in, we hold nothing that can be traced back to you: the daily
code described above cannot be reversed, and the key that produced it is gone.

You may also complain to your national supervisory authority — in France, the
CNIL (Commission Nationale de l'Informatique et des Libertés).

---

## If you run `mode: 'identified'`

Replace *How you are counted* and *Lawful basis* with:

> **How you are counted.** With your consent, we store a first-party cookie
> named `{{ _pa }}` on your device, containing a random identifier and nothing
> else. It lasts {{ 390 }} days. It lets us recognise a returning visit to this
> site. It is not readable by any other site and is never shared.
>
> **Lawful basis.** We place this cookie only after you consent, and you may
> withdraw consent at any time from {{ the cookie settings link }}, which
> deletes the cookie.
