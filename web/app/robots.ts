import type { MetadataRoute } from 'next';
import { CRAWLER_DISALLOW, siteOrigin } from '@/lib/brand';

// Every route except `/` is behind AuthGate and answers a session check to
// anyone without a cookie, so an indexed one would be a page of nothing under a
// URL that promises a product. Disallow them by name rather than relying on the
// gate: a crawler that indexes /chat before the gate resolves has already spent
// the impression.
export default function robots(): MetadataRoute.Robots {
  return {
    rules: { userAgent: '*', allow: '/', disallow: [...CRAWLER_DISALLOW] },
    sitemap: `${siteOrigin()}/sitemap.xml`,
    host: siteOrigin(),
  };
}
