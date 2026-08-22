import type { MetadataRoute } from 'next';
import { INDEXABLE_PATHS, siteOrigin } from '@/lib/brand';

// One entry, and that is not an oversight: `/` is the only URL on this instance
// with content a stranger can read. Listing the gated routes would hand a
// crawler a dozen session checks; listing /pricing would advertise a hosted-only
// surface that does not exist on a self-hosted instance at all (lib/ia.ts).
export default function sitemap(): MetadataRoute.Sitemap {
  const origin = siteOrigin();
  return INDEXABLE_PATHS.map((path) => ({
    url: new URL(path, origin).toString(),
    changeFrequency: 'weekly' as const,
    priority: 1,
  }));
}
