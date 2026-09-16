// Analysis destinations under Analytics. Board keys match the seeded 007
// declarations (internal/dataplane/store/default_boards.go). Unserved tiles
// are App Store Connect labels with no AgentRay catalog metric — they render
// as named empty states, never as a fabricated zero.

export const ANALYSIS_BOARDS = [
  { key: 'acquisition', href: '/acquisition', label: 'Acquisition' },
  { key: 'monetization', href: '/monetization', label: 'Monetization' },
  { key: 'usage', href: '/usage', label: 'Usage' },
] as const;

export type AnalysisBoardKey = (typeof ANALYSIS_BOARDS)[number]['key'];

export type UnservedTile = {
  label: string;
  reason: string;
};

export const UNSERVED_TILES: Record<AnalysisBoardKey, readonly UnservedTile[]> = {
  acquisition: [
    { label: 'Redownloads', reason: 'AgentRay does not distinguish a redownload from first-observed activity.' },
    { label: 'Conversion rate', reason: 'Apple’s downloads÷impressions rate is not computed. AgentRay activation is a different metric and is not relabelled as this.' },
    { label: 'Impressions / day', reason: 'No impression event exists in the catalog.' },
    { label: 'Product page views', reason: 'App Store product-page views are not captured. Ranked in-product pages are the Top pages tile.' },
    { label: 'Updates', reason: 'No app-update event exists in the catalog.' },
  ],
  monetization: [
    { label: 'In-app purchases / day', reason: 'No purchase-count metric is computed; SDK event volume is not money.' },
  ],
  usage: [
    { label: 'Average retention D14', reason: 'AgentRay serves D1, D7 and D30. D14 is not computed.' },
    { label: 'Crashes by app version', reason: 'No verified crash event with a normalized app-version contract exists.' },
    { label: 'Deletions', reason: 'An uninstall is not an event the SDK can send; no deletion signal exists.' },
  ],
};
