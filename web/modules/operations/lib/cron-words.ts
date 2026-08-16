// cron-words.ts — a cron expression in the owner's language.
//
// `0 9 * * 1` is a fine label next to the field the owner typed it into. In a
// list of everything that runs unattended it is jargon: the whole job of
// /operations is answering "what runs without me, and when", and it cannot do
// that in a notation most product owners cannot read.
//
// Pure and total. It NEVER throws and never invents: anything it cannot state
// plainly comes back as the raw expression, because a wrong plain-English
// schedule is far worse than an honest cron string.

const DAY_NAMES = ['Sundays', 'Mondays', 'Tuesdays', 'Wednesdays', 'Thursdays', 'Fridays', 'Saturdays'];
const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];

// pad2 keeps 7:00 from reading as a typo next to 16:00 in a column of times.
function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

// numeric parses a single non-negative integer field, or null for anything else
// (a list, a range, a step, a name, junk). Callers fall back to the raw cron
// rather than guessing at the shapes this does not cover.
function numeric(field: string, min: number, max: number): number | null {
  if (!/^\d+$/.test(field)) return null;
  const n = Number(field);
  return n >= min && n <= max ? n : null;
}

// stepOf reads `*/n` — the one non-scalar shape common enough to be worth
// stating in words, because "every 15 minutes" is the most-typed schedule there
// is and `*/15 * * * *` is the least readable.
function stepOf(field: string, max: number): number | null {
  const m = /^\*\/(\d+)$/.exec(field);
  if (!m) return null;
  const n = Number(m[1]);
  return n >= 1 && n <= max ? n : null;
}

function timeOfDay(minute: string, hour: string): string | null {
  const m = numeric(minute, 0, 59);
  const h = numeric(hour, 0, 23);
  if (m === null || h === null) return null;
  return `${pad2(h)}:${pad2(m)}`;
}

/**
 * cronToWords renders a standard 5-field cron (minute hour dom month dow) as a
 * sentence fragment a product owner can read, e.g. "Mondays 09:00".
 *
 * Returns the input unchanged when the expression is not one of the shapes it
 * can state exactly — including every list (`1,3,5`) and range (`1-5`), which
 * are legal cron this deliberately does not paraphrase.
 */
export function cronToWords(expr: string): string {
  const raw = (expr ?? '').trim();
  const fields = raw.split(/\s+/);
  if (fields.length !== 5) return raw;
  const [minute, hour, dom, month, dow] = fields;

  // Sub-daily: every N minutes / every N hours / hourly.
  if (dom === '*' && month === '*' && dow === '*') {
    const everyMin = stepOf(minute, 59);
    if (everyMin !== null && hour === '*') {
      return everyMin === 1 ? 'Every minute' : `Every ${everyMin} minutes`;
    }
    const everyHour = stepOf(hour, 23);
    const atMinute = numeric(minute, 0, 59);
    if (everyHour !== null && atMinute !== null) {
      const at = atMinute === 0 ? '' : ` at :${pad2(atMinute)}`;
      return everyHour === 1 ? `Every hour${at}` : `Every ${everyHour} hours${at}`;
    }
    if (hour === '*' && atMinute !== null) {
      return atMinute === 0 ? 'Every hour, on the hour' : `Every hour at :${pad2(atMinute)}`;
    }
  }

  const at = timeOfDay(minute, hour);
  if (at === null) return raw;

  // Weekly. Cron accepts both 0 and 7 for Sunday.
  if (dom === '*' && month === '*' && dow !== '*') {
    const d = numeric(dow, 0, 7);
    if (d === null) return raw;
    return `${DAY_NAMES[d === 7 ? 0 : d]} ${at}`;
  }

  // Monthly, and the yearly case where a month is pinned too.
  if (dow === '*' && dom !== '*') {
    const day = numeric(dom, 1, 31);
    if (day === null) return raw;
    if (month === '*') return `Day ${day} of the month, ${at}`;
    const mo = numeric(month, 1, 12);
    if (mo === null) return raw;
    return `${MONTH_NAMES[mo - 1]} ${day}, ${at}`;
  }

  // Daily.
  if (dom === '*' && month === '*' && dow === '*') return `Every day ${at}`;

  return raw;
}
