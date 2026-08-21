'use client';

import Link from 'next/link';
import { childSurfacesFor } from '@/lib/ia';

// RelatedSurfacesNav is the same links, stacked for the page aside. The
// horizontal "Also …" row used to sit between the title and the filter bar,
// where it competed with the page's own controls; in the right column it is
// context rather than an interruption. The current page is dropped — an alias
// route (Replay under Events) would otherwise list a link to itself.
export function RelatedSurfacesNav({ parentHref, currentHref }: { parentHref: string; currentHref?: string }) {
  const surfaces = childSurfacesFor(parentHref).filter((s) => s.href !== currentHref);
  if (surfaces.length === 0) return null;
  return (
    <nav className="flex flex-col">
      {surfaces.map((surface) => (
        <Link
          key={surface.href}
          href={surface.href}
          className="inline-flex min-h-9 items-center rounded-sm px-2 text-sm text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]"
        >
          {surface.label}
        </Link>
      ))}
    </nav>
  );
}
