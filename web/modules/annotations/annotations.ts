// Chart annotations: the deploy / campaign / price / other marks a member
// drops on a trend so "did X cause this movement" is answerable in-product.
// The record lives in the API (project-scoped, overlap-windowed); this module
// is the client half — the window read, the add/delete dialog, and the button
// every temporal panel carries.

export const ANNOTATION_KINDS = [
  { value: 'deploy', label: 'Deploy' },
  { value: 'campaign', label: 'Campaign' },
  { value: 'price', label: 'Price change' },
  { value: 'other', label: 'Other' },
];

// toRFC3339 turns a datetime-local value ("2026-09-10T12:00") into the
// RFC3339 instant the API stores. Empty or unparseable input returns '' so
// the caller can name the field instead of sending a bad timestamp.
export function toRFC3339(local: string): string {
  if (!local) return '';
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

// fromRFC3339 renders a stored instant back into the datetime-local shape the
// input expects — the local wall time, not the UTC string.
export function fromRFC3339(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// annotationWindow is the [from, to] a surface asks for. `to` is rounded down
// to the minute so the query key is stable across renders — a fresh Date()
// per render would refetch the same window forever.
export function annotationWindow(from: Date, to?: Date): { from: string; to: string } {
  const end = new Date(to ?? Date.now());
  end.setSeconds(0, 0);
  return { from: from.toISOString(), to: end.toISOString() };
}
