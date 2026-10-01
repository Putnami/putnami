import { HttpResponse, endpoint } from '@putnami/application';

import { getDocsPaths } from '../../lib/docs/navigation.server';

const BASE_URL = 'https://putnami.dev';

function escapeXml(str: string): string {
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export default endpoint(async () => {
  // Use the same path source as the SSG pre-render (page().static({ paths })),
  // so the sitemap can never advertise a doc URL that wasn't actually generated
  // (or miss one that was).
  const docPaths = await getDocsPaths();

  const urls = [
    { loc: '/', priority: '1.0', changefreq: 'weekly' },
    { loc: '/docs', priority: '0.9', changefreq: 'weekly' },
    { loc: '/privacy', priority: '0.3', changefreq: 'yearly' },
    ...docPaths.map((path) => ({ loc: `/docs/${path}`, priority: '0.7', changefreq: 'monthly' })),
  ];

  const xml = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${urls
  .map(
    (u) => `  <url>
    <loc>${escapeXml(`${BASE_URL}${u.loc}`)}</loc>
    <changefreq>${u.changefreq}</changefreq>
    <priority>${u.priority}</priority>
  </url>`,
  )
  .join('\n')}
</urlset>`;

  return new HttpResponse(xml, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' },
  });
});
