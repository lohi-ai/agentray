'use client';

import { useEffect, useState } from 'react';

// Subscribe to a CSS media query from React. Returns false during SSR and the
// first client paint (the server has no viewport), then settles to the real
// match in an effect — callers that branch layout on this should treat the wide
// case as the hydration-safe default.
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(false);
  useEffect(() => {
    const mql = window.matchMedia(query);
    const sync = () => setMatches(mql.matches);
    sync();
    mql.addEventListener('change', sync);
    return () => mql.removeEventListener('change', sync);
  }, [query]);
  return matches;
}
