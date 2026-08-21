'use client';

import { useEffect, useRef, useState } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { useFiltersStore } from '@/lib/app-state';
import { filtersFromQuery, filtersToQuery } from '@/lib/filter-url';

/**
 * Two-way binding between the applied filters and the address bar.
 *
 * Mounted once, in FilterBar, so every filtered surface gets it and no page can
 * forget. The URL is the source of truth exactly once — on first mount, so a
 * pasted link wins — and after that the store writes to the URL, so the address
 * bar trails the controls instead of fighting them.
 */
export function useFilterUrlSync() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const applied = useFiltersStore((s) => s.appliedFilters);
  const setFilters = useFiltersStore((s) => s.setFilters);
  const commit = useFiltersStore((s) => s.commit);

  // Read the URL exactly once, at mount: a pasted link wins, and after that the
  // store owns the state — re-reading would undo the control the reader just
  // used. The lazy initializer keeps the parse pure and out of the effect.
  const [fromURL] = useState(() => filtersFromQuery(new URLSearchParams(searchParams.toString())));
  const hydrated = useRef(false);

  // Hydrate and publish from one effect, in that order. Two effects would both
  // run in the mount commit, and the publisher would still be holding this
  // render's *default* filters — rewriting a pasted link back to an empty one
  // before the store had a chance to adopt it.
  useEffect(() => {
    if (!hydrated.current) {
      hydrated.current = true;
      if (fromURL) {
        setFilters(fromURL);
        commit();
        // The commit re-runs this effect with the adopted filters in hand.
        return;
      }
    }

    const current = new URLSearchParams(searchParams.toString());
    const next = filtersToQuery(applied, current);
    // Sort so an unchanged filter set never rewrites history through key
    // reordering alone.
    next.sort();
    current.sort();
    const nextQS = next.toString();
    if (nextQS === current.toString()) return;
    router.replace(nextQS ? `${pathname}?${nextQS}` : pathname, { scroll: false });
  }, [applied, fromURL, pathname, router, searchParams, setFilters, commit]);
}
