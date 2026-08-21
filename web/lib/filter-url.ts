import { defaultFilters, type Filters } from '@/lib/api';

// Filters live in the URL because a filtered view is a finding, and a finding
// has to be sendable. "iOS converts worse than the site" was previously a thing
// you could see and not link to: the state lived only in a Zustand store, so a
// pasted URL dropped the reader on the unfiltered page and a reload dropped the
// author there too.
//
// Only departures from the default are written, so an untouched page keeps a
// clean URL and the query string reads as the question that was asked.

// TEXT_KEYS are the filter fields carried verbatim. `limit` is deliberately
// absent: it is a page-size concern, not part of the question being shared.
const TEXT_KEYS = [
  'event_type',
  'event_name',
  'distinct_id',
  'session_id',
  'agent_id',
  'model_name',
  'search',
  'platform',
] as const;

/** Every query-string key this module owns — used to clear stale filter state
 *  without disturbing params that belong to the page (`?tab=`, `?agent=`). */
export const FILTER_URL_KEYS = [...TEXT_KEYS, 'hours', 'from', 'to', 'error_only'] as const;

export function filtersToQuery(filters: Filters, base?: URLSearchParams): URLSearchParams {
  const params = new URLSearchParams(base?.toString() ?? '');
  for (const key of FILTER_URL_KEYS) params.delete(key);

  if (filters.hours !== defaultFilters.hours) params.set('hours', String(filters.hours));
  if (filters.from) params.set('from', filters.from);
  if (filters.to) params.set('to', filters.to);
  for (const key of TEXT_KEYS) {
    if (filters[key]) params.set(key, filters[key]);
  }
  if (filters.error_only) params.set('error_only', 'true');
  return params;
}

/** Reads the filter half of a query string. Returns null when the URL carries no
 *  filter at all, so a page with only its own params never overwrites the
 *  filters the reader already has. */
export function filtersFromQuery(params: URLSearchParams): Filters | null {
  if (!FILTER_URL_KEYS.some((key) => params.has(key))) return null;

  const filters: Filters = { ...defaultFilters };
  // A hand-edited or truncated `hours` must not produce NaN and a request for
  // "the last NaN hours" — fall back to the default rather than to zero, which
  // would silently render an empty page as if there were no data.
  const hours = Number(params.get('hours'));
  if (Number.isFinite(hours) && hours > 0) filters.hours = Math.floor(hours);
  filters.from = params.get('from') ?? '';
  filters.to = params.get('to') ?? '';
  for (const key of TEXT_KEYS) filters[key] = params.get(key) ?? '';
  filters.error_only = params.get('error_only') === 'true';
  return filters;
}
