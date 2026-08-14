'use client';

import Link from 'next/link';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { childSurfacesFor } from '@/lib/ia';

// Compact parent → child links so Teams / Marketplace / Monitor / Alerts /
// Cohorts stay one click from their Main or Explore parent without sitting
// as equal-weight nav peers.
export function RelatedSurfaces({ parentHref }: { parentHref: string }) {
  const surfaces = childSurfacesFor(parentHref);
  if (surfaces.length === 0) return null;
  return (
    <HStack gap={2} wrap="wrap" align="center">
      {surfaces.map((surface) => (
        <Link
          key={surface.href}
          href={surface.href}
          className="inline-flex min-h-11 items-center rounded-sm px-3 text-[12.5px] text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]"
        >
          {surface.label}
        </Link>
      ))}
    </HStack>
  );
}

export function RelatedSurfacesLabel({ parentHref }: { parentHref: string }) {
  const surfaces = childSurfacesFor(parentHref);
  if (surfaces.length === 0) return null;
  return (
    <HStack gap={2} wrap="wrap" align="center">
      <Text type="supporting">Also</Text>
      <RelatedSurfaces parentHref={parentHref} />
    </HStack>
  );
}
