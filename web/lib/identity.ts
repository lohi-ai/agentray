import type { Event } from '@/lib/api';

export type IdentityMap = Record<string, { email?: string; name?: string }>;

export function buildIdentityMap(events: Event[]): IdentityMap {
  const identities: IdentityMap = {};
  for (const event of events) {
    if (!event.distinct_id) continue;
    const traits = identityTraits(event);
    if (!traits.email && !traits.name) continue;
    identities[event.distinct_id] = { ...identities[event.distinct_id], ...traits };
  }
  return identities;
}

function identityTraits(event: Event) {
  const properties = parseProperties(event.properties);
  const setTraits = objectRecord(properties.$set);
  const setOnceTraits = objectRecord(properties.$set_once);
  return {
    email: stringValue(properties.email) || stringValue(setTraits.email) || stringValue(setOnceTraits.email),
    name: stringValue(properties.name) || stringValue(setTraits.name) || stringValue(setOnceTraits.name),
  };
}

function parseProperties(value: string) {
  if (!value) return {};
  try {
    return objectRecord(JSON.parse(value));
  } catch {
    return {};
  }
}

function objectRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function stringValue(value: unknown) {
  return typeof value === 'string' ? value : '';
}
