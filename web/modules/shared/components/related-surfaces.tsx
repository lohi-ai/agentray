'use client';

import Link from 'next/link';
import { CHILD_SURFACES, childSurfacesFor, isLinkedSurface } from '@/lib/ia';

// RelatedSurfacesNav is the same links, stacked for the page aside. The
// horizontal "Also …" row used to sit between the title and the filter bar,
// where it competed with the page's own controls; in the right column it is
// context rather than an interruption. The current page is dropped — an alias
// route (Replay under Events) would otherwise list a link to itself.
//
// A named-but-unbuilt surface renders as text with a "Coming soon" qualifier,
// not a link: the IA should say the destination exists without handing the
// reader a route the router cannot serve.
export function RelatedSurfacesNav({ parentHref, currentHref, hosted }: { parentHref: string; currentHref?: string; hosted?: boolean }) {
  const surfaces = childSurfacesFor(parentHref, CHILD_SURFACES, { hosted }).filter((s) => !isLinkedSurface(s) || s.href !== currentHref);
  if (surfaces.length === 0) return null;
  return (
    <nav className="flex flex-col">
      {surfaces.map((surface) =>
        isLinkedSurface(surface) ? (
          <Link
            key={surface.href}
            href={surface.href}
            className="inline-flex min-h-11 items-center rounded-sm px-2 text-sm text-[var(--color-text-secondary)] transition-colors hover:bg-[var(--color-background-muted)] hover:text-[var(--color-text-primary)]"
          >
            {surface.label}
          </Link>
        ) : (
          <span
            key={surface.label}
            className="inline-flex min-h-11 items-center gap-2 rounded-sm px-2 text-sm text-[var(--color-text-secondary)]"
          >
            {surface.label}
            <span className="rounded-[20px] bg-[var(--color-background-muted)] px-1.5 py-0.5 text-2xs">Coming soon</span>
          </span>
        ),
      )}
    </nav>
  );
}
