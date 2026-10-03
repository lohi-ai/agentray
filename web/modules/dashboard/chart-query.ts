import type { Filters } from '@/lib/api';

export type ResolvedChartQuery =
  | { ok: true; sql: string; from: Date; to: Date; hours: number; label: string }
  | { ok: false; message: string };

export type ProjectedChartRows =
  | { status: 'ready'; values: number[]; labels: (string | number)[] }
  | { status: 'empty' }
  | { status: 'non_plottable'; message: string };

const RANGE_ERROR = 'This query cannot apply the selected date range';
const tokenPattern = /\{\{\s*([^{}]+?)\s*\}\}/g;

// SQL placeholders inside string literals are executable and intentionally
// retained; placeholders inside comments are not part of the query contract.
function withoutSQLComments(sql: string): string {
  let out = '';
  let state: 'code' | 'single' | 'double' | 'line' | 'block' = 'code';
  for (let i = 0; i < sql.length; i += 1) {
    const ch = sql[i];
    const next = sql[i + 1];
    if (state === 'code') {
      if (ch === "'") state = 'single';
      else if (ch === '"') state = 'double';
      else if (ch === '-' && next === '-') {
        out += '  ';
        i += 1;
        state = 'line';
        continue;
      } else if (ch === '#') {
        out += ' ';
        state = 'line';
        continue;
      } else if (ch === '/' && next === '*') {
        out += '  ';
        i += 1;
        state = 'block';
        continue;
      }
      out += ch;
      continue;
    }
    if (state === 'single' || state === 'double') {
      out += ch;
      const quote = state === 'single' ? "'" : '"';
      if (ch === '\\' && next !== undefined) {
        out += next;
        i += 1;
      } else if (ch === quote && next === quote) {
        out += next;
        i += 1;
      } else if (ch === quote) {
        state = 'code';
      }
      continue;
    }
    if (state === 'line') {
      if (ch === '\n' || ch === '\r') {
        out += ch;
        state = 'code';
      } else {
        out += ' ';
      }
      continue;
    }
    if (ch === '*' && next === '/') {
      out += '  ';
      i += 1;
      state = 'code';
    } else {
      out += ch === '\n' || ch === '\r' ? ch : ' ';
    }
  }
  return out;
}

export function resolveChartQuery(sql: string, filters: Filters, now = new Date()): ResolvedChartQuery {
  const executable = withoutSQLComments(sql);
  const matches = [...executable.matchAll(tokenPattern)];
  const names = matches.map((match) => match[1].trim());
  if (names.some((name) => !['from', 'to', 'hours'].includes(name))) {
    return { ok: false, message: `${RANGE_ERROR}: use only '{{from}}', '{{to}}', and optional {{hours}} tokens.` };
  }
  if (!names.includes('from') || !names.includes('to')) {
    return { ok: false, message: `${RANGE_ERROR}: add both quoted '{{from}}' and '{{to}}' tokens.` };
  }
  for (const match of matches) {
    if ((match[1].trim() === 'from' || match[1].trim() === 'to') &&
        (executable[(match.index ?? 0) - 1] !== "'" || executable[(match.index ?? 0) + match[0].length] !== "'")) {
      return { ok: false, message: `${RANGE_ERROR}: date tokens must be single-quoted SQL values.` };
    }
  }

  const to = filters.to ? new Date(filters.to) : new Date(now);
  const from = filters.from ? new Date(filters.from) : new Date(to.getTime() - filters.hours * 3_600_000);
  if (!Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) || from >= to) {
    return { ok: false, message: `${RANGE_ERROR}: choose a valid start before the end.` };
  }
  const hours = (to.getTime() - from.getTime()) / 3_600_000;
  const replacements: Record<string, string> = {
    from: from.toISOString(),
    to: to.toISOString(),
    hours: String(hours),
  };
  let resolved = '';
  let offset = 0;
  for (const match of matches) {
    const index = match.index ?? 0;
    resolved += sql.slice(offset, index) + replacements[match[1].trim()];
    offset = index + match[0].length;
  }
  resolved += sql.slice(offset);
  return {
    ok: true,
    sql: resolved,
    from,
    to,
    hours,
    label: `${from.toISOString()} to ${to.toISOString()} (UTC, end exclusive)`,
  };
}

function safeChartNumber(value: unknown): number | null {
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value))) return null;
    return value;
  }
  if (typeof value !== 'string' || value.trim() === '') return null;
  const text = value.trim();
  if (!/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$/.test(text)) return null;
  const number = Number(text);
  if (!Number.isFinite(number)) return null;
  if (/^[+-]?\d+$/.test(text) && !Number.isSafeInteger(number)) return null;
  return number;
}

export function projectChartRows(
  rows: Array<Record<string, unknown>>,
  requestedY?: string,
  requestedX?: string,
): ProjectedChartRows {
  if (rows.length === 0) return { status: 'empty' };
  const keys = Object.keys(rows[0]);
  const yField = requestedY || keys.find((key) => rows.every((row) => safeChartNumber(row[key]) !== null));
  if (!yField || !keys.includes(yField)) {
    return { status: 'non_plottable', message: 'Query returned no safely plottable numeric column.' };
  }
  const values: number[] = [];
  for (const row of rows) {
    const value = safeChartNumber(row[yField]);
    if (value === null) {
      return {
        status: 'non_plottable',
        message: `Column "${yField}" contains a missing, non-finite, or unsafe numeric value.`,
      };
    }
    values.push(value);
  }
  const xField = requestedX || keys.find((key) => key !== yField);
  const labels = rows.map((row, index) => {
    const value = xField ? row[xField] : undefined;
    return typeof value === 'string' || typeof value === 'number' ? value : index + 1;
  });
  return { status: 'ready', values, labels };
}
