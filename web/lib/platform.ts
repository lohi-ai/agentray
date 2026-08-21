// Platform naming, in one place.
//
// The stored value is what the ingest classifier decided ('web' | 'ios' |
// 'android' | 'server', or 'unknown' when it could not tell). A product may also
// declare a platform of its own — a CLI, a TV app — which is stored verbatim, so
// an unmapped value renders as itself rather than being hidden behind "Other".
const PLATFORM_LABELS: Record<string, string> = {
  web: 'Web',
  ios: 'iOS app',
  android: 'Android app',
  server: 'Server',
  unknown: 'Unknown',
};

export function platformLabel(value: string) {
  return PLATFORM_LABELS[value] ?? value;
}
