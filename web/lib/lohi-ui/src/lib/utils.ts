export type ClassValue = string | false | null | undefined;

// The upstream primitive uses clsx + tailwind-merge. AgentRay intentionally
// vendors no new runtime packages for this scoped port, so the subset only
// needs deterministic class joining; component styles live in evidence.css.
export function cn(...values: ClassValue[]): string {
  return values.filter(Boolean).join(' ');
}
