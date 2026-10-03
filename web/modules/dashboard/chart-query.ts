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

type ScannedSQL = { executable: string; standaloneDateTokens: Set<number> };

type SQLLexicalClass =
  | 'line-comment'
  | 'block-comment'
  | 'single-quoted-string'
  | 'escape-string'
  | 'quoted-identifier'
  | 'dollar-quoted-string'
  | 'identifier-keyword'
  | 'operator-punctuation'
  | 'whitespace';

function codePointWidth(sql: string, index: number): number {
  return (sql.codePointAt(index) ?? 0) > 0xffff ? 2 : 1;
}

function isASCIIIdentifierStart(codePoint: number): boolean {
  return codePoint === 0x5f || (codePoint >= 0x41 && codePoint <= 0x5a) || (codePoint >= 0x61 && codePoint <= 0x7a);
}

function isIdentifierStart(sql: string, index: number): boolean {
  const codePoint = sql.codePointAt(index);
  return codePoint !== undefined && (isASCIIIdentifierStart(codePoint) || codePoint >= 0x80);
}

function isIdentifierContinuation(sql: string, index: number): boolean {
  const codePoint = sql.codePointAt(index);
  return codePoint !== undefined && (
    isASCIIIdentifierStart(codePoint) ||
    (codePoint >= 0x30 && codePoint <= 0x39) ||
    codePoint === 0x24 ||
    codePoint >= 0x80
  );
}

// DuckDB accepts an empty tag, or a tag beginning with an ASCII/Unicode
// identifier character and continuing with those characters or ASCII digits.
function dollarDelimiterAt(sql: string, start: number): string | null {
  if (sql[start] !== '$') return null;
  if (sql[start + 1] === '$') return '$$';
  let index = start + 1;
  if (!isIdentifierStart(sql, index)) return null;
  index += codePointWidth(sql, index);
  while (index < sql.length && sql[index] !== '$') {
    if (!isIdentifierContinuation(sql, index) || sql[index] === '$') return null;
    index += codePointWidth(sql, index);
  }
  return sql[index] === '$' ? sql.slice(start, index + 1) : null;
}

function masked(sql: string, start: number, end: number): string {
  return sql.slice(start, end).replace(/[^\r\n]/g, ' ');
}

function classifiedSpan(sql: string, start: number, end: number, state: SQLLexicalClass): string {
  switch (state) {
    case 'line-comment':
    case 'block-comment':
    case 'escape-string':
    case 'quoted-identifier':
    case 'dollar-quoted-string':
      return masked(sql, start, end);
    case 'single-quoted-string':
    case 'identifier-keyword':
    case 'operator-punctuation':
    case 'whitespace':
      return sql.slice(start, end);
  }
}

// A single pass over DuckDB lexical tokens. Only ordinary string literals are
// retained for placeholder validation; non-code regions are position-preserving
// masks so match offsets still refer to the original SQL.
function scanSQL(sql: string): ScannedSQL {
  let out = '';
  let state: SQLLexicalClass = 'operator-punctuation';
  const standaloneDateTokens = new Set<number>();
  let index = 0;
  while (index < sql.length) {
    const start = index;
    const ch = sql[index];
    const next = sql[index + 1];

    if (/\s/u.test(ch)) {
      state = 'whitespace';
      index += 1;
      while (index < sql.length && /\s/u.test(sql[index])) index += 1;
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    if (ch === '-' && next === '-') {
      state = 'line-comment';
      index += 2;
      while (index < sql.length && sql[index] !== '\n' && sql[index] !== '\r') index += 1;
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    if (ch === '/' && next === '*') {
      state = 'block-comment';
      let depth = 1;
      index += 2;
      while (index < sql.length && depth > 0) {
        if (sql[index] === '/' && sql[index + 1] === '*') {
          depth += 1;
          index += 2;
        } else if (sql[index] === '*' && sql[index + 1] === '/') {
          depth -= 1;
          index += 2;
        } else {
          index += codePointWidth(sql, index);
        }
      }
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    if ((ch === 'E' || ch === 'e') && next === "'" && !isIdentifierContinuation(sql, index - 1)) {
      state = 'escape-string';
      index += 2;
      while (index < sql.length) {
        if (sql[index] === '\\' && index + 1 < sql.length) {
          index += 1 + codePointWidth(sql, index + 1);
        } else if (sql[index] === "'" && sql[index + 1] === "'") {
          index += 2;
        } else if (sql[index] === "'") {
          index += 1;
          break;
        } else {
          index += codePointWidth(sql, index);
        }
      }
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    if (ch === "'") {
      state = 'single-quoted-string';
      index += 1;
      const contentStart = index;
      let contentEnd = -1;
      while (index < sql.length) {
        if (sql[index] === "'" && sql[index + 1] === "'") {
          index += 2;
        } else if (sql[index] === "'") {
          contentEnd = index;
          index += 1;
          break;
        } else {
          index += codePointWidth(sql, index);
        }
      }
      out += classifiedSpan(sql, start, index, state);
      const literal = contentEnd < 0 ? '' : sql.slice(contentStart, contentEnd);
      if (
        /^\{\{\s*(from|to)\s*\}\}$/.test(literal) &&
        !isIdentifierContinuation(sql, start - 1) &&
        !isIdentifierContinuation(sql, index)
      ) {
        standaloneDateTokens.add(contentStart);
      }
      continue;
    }

    if (ch === '"') {
      state = 'quoted-identifier';
      index += 1;
      while (index < sql.length) {
        if (sql[index] === '"' && sql[index + 1] === '"') {
          index += 2;
        } else if (sql[index] === '"') {
          index += 1;
          break;
        } else {
          index += codePointWidth(sql, index);
        }
      }
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    const dollarDelimiter = ch === '$' && !isIdentifierContinuation(sql, index - 1)
      ? dollarDelimiterAt(sql, index)
      : null;
    if (dollarDelimiter) {
      state = 'dollar-quoted-string';
      index += dollarDelimiter.length;
      const close = sql.indexOf(dollarDelimiter, index);
      index = close < 0 ? sql.length : close + dollarDelimiter.length;
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    if (isIdentifierStart(sql, index)) {
      state = 'identifier-keyword';
      index += codePointWidth(sql, index);
      while (index < sql.length && isIdentifierContinuation(sql, index)) {
        index += codePointWidth(sql, index);
      }
      out += classifiedSpan(sql, start, index, state);
      continue;
    }

    state = 'operator-punctuation';
    index += codePointWidth(sql, index);
    out += classifiedSpan(sql, start, index, state);
  }
  return { executable: out, standaloneDateTokens };
}

export function resolveChartQuery(sql: string, filters: Filters, now = new Date()): ResolvedChartQuery {
  const { executable, standaloneDateTokens } = scanSQL(sql);
  const matches = [...executable.matchAll(tokenPattern)];
  const names = matches.map((match) => match[1].trim());
  if (names.some((name) => !['from', 'to', 'hours'].includes(name))) {
    return { ok: false, message: `${RANGE_ERROR}: use only '{{from}}', '{{to}}', and optional {{hours}} tokens.` };
  }
  if (!names.includes('from') || !names.includes('to')) {
    return { ok: false, message: `${RANGE_ERROR}: add both quoted '{{from}}' and '{{to}}' tokens.` };
  }
  for (const match of matches) {
    if (
      (match[1].trim() === 'from' || match[1].trim() === 'to') &&
      !standaloneDateTokens.has(match.index ?? -1)
    ) {
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
  if (Number.isInteger(number) && !Number.isSafeInteger(number)) return null;
  const mantissa = text.split(/[eE]/, 1)[0].replace(/[^0-9]/g, '');
  if (number === 0 && /[1-9]/.test(mantissa)) return null;
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
