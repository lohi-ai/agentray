// The dataset preview's column projection. Landed rows are schemaless JSON, so
// the table shows what is actually there — but the row also carries landing
// metadata the sync recorded (row_key, cursor, synced_at), and a source table
// is free to have columns with those exact names. Merging them by key lets a
// source column overwrite the landing metadata, and rendering both under one
// key duplicates a column. So every source column gets its own disambiguated
// table key, and the three landing keys stay the sync's.
//
// Pure and synchronous, so the projection is testable without a browser.

/** How many source columns the preview table shows. */
const PREVIEW_MAX_COLUMNS = 8;

/** Column keys the dialog renders from the sync's own landing metadata. */
export const LANDING_KEYS = ['row_key', 'cursor', 'synced_at'] as const;

export type PreviewRow = { row_key: string; cursor: string; synced_at: string; data: string };

export type PreviewProjection = {
  /** Source column names in first-seen order, capped to the table's width. */
  sourceKeys: string[];
  /** Source column name → unique table key (identical unless it collides). */
  sourceKeyMap: Record<string, string>;
  /** One object per row: source values under their table key + landing metadata. */
  rows: Record<string, unknown>[];
};

// A landed row is a JSON object; anything else (a bare number, an array, a
// truncated payload) is shown verbatim in a `data` column rather than
// reinterpreted as columns.
function parsedRow(row: PreviewRow): Record<string, unknown> {
  try {
    const parsed = JSON.parse(row.data);
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) return parsed as Record<string, unknown>;
  } catch {
    // fall through to the raw payload
  }
  return { data: row.data };
}

// previewColumns derives the table's columns from the union of keys across the
// preview rows, in the order the rows first mention them.
function previewColumns(rows: PreviewRow[]): string[] {
  const seen = new Set<string>();
  for (const r of rows) {
    for (const k of Object.keys(parsedRow(r))) {
      if (seen.size < PREVIEW_MAX_COLUMNS) seen.add(k);
    }
  }
  return [...seen];
}

export function previewProjection(rows: PreviewRow[]): PreviewProjection {
  const sourceKeys = previewColumns(rows);
  const taken = new Set<string>(LANDING_KEYS);
  const sourceKeyMap: Record<string, string> = {};
  for (const key of sourceKeys) {
    // Prefixing until unique terminates: the candidate strictly grows and the
    // set of taken keys is finite.
    let columnKey = key;
    while (taken.has(columnKey)) columnKey = `source.${columnKey}`;
    taken.add(columnKey);
    sourceKeyMap[key] = columnKey;
  }
  const projected = rows.map((row) => {
    const source = parsedRow(row);
    const out: Record<string, unknown> = {};
    for (const key of sourceKeys) out[sourceKeyMap[key]] = source[key];
    // Landing metadata is written last and under its own keys: it is what the
    // sync recorded, never what the source claims.
    out.row_key = row.row_key;
    out.cursor = row.cursor;
    out.synced_at = row.synced_at;
    return out;
  });
  return { sourceKeys, sourceKeyMap, rows: projected };
}
