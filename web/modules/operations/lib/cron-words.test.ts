import { describe, expect, it } from 'vitest';
import { cronToWords } from './cron-words';

// cronToWords is the only translation layer between a five-field expression and
// what the owner is told will happen. Every case here is a schedule that exists
// in the product today — a wrong reading is worse than no reading, because the
// owner stops reading the cron itself.

describe('cronToWords', () => {
  it('states the common schedules in plain words', () => {
    expect(cronToWords('0 9 * * 1')).toBe('Mondays 09:00');
    expect(cronToWords('30 6 * * *')).toBe('Every day 06:30');
    expect(cronToWords('0 * * * *')).toBe('Every hour, on the hour');
    expect(cronToWords('*/15 * * * *')).toBe('Every 15 minutes');
    expect(cronToWords('0 */6 * * *')).toBe('Every 6 hours');
    expect(cronToWords('0 0 1 * *')).toBe('Day 1 of the month, 00:00');
    expect(cronToWords('0 0 1 1 *')).toBe('January 1, 00:00');
  });

  it('pads the hour so a column of times lines up', () => {
    // 7:00 next to 16:00 reads as a typo; 07:00 does not.
    expect(cronToWords('0 7 * * 2')).toBe('Tuesdays 07:00');
    expect(cronToWords('5 0 * * *')).toBe('Every day 00:05');
  });

  it('reads both 0 and 7 as Sunday', () => {
    expect(cronToWords('0 8 * * 0')).toBe('Sundays 08:00');
    expect(cronToWords('0 8 * * 7')).toBe('Sundays 08:00');
  });

  it('hands back the expression it cannot state exactly', () => {
    // A guess here would be a lie with a schedule attached. Lists and ranges
    // get the raw cron, which is at least true.
    expect(cronToWords('0 9 * * 1-5')).toBe('0 9 * * 1-5');
    expect(cronToWords('0 9,17 * * *')).toBe('0 9,17 * * *');
  });

  it('is total — junk in never throws', () => {
    expect(cronToWords('')).toBe('');
    expect(cronToWords('not a cron')).toBe('not a cron');
    expect(cronToWords('* * *')).toBe('* * *');
  });
});
