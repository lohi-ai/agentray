import { describe, expect, it } from 'vitest';
import { conversionReadout } from './validation-readout';

describe('conversionReadout', () => {
  it('reads a normal funnel as a percentage of who saw it', () => {
    expect(conversionReadout(12, 400)).toEqual({ label: '3.0% of 400', unreadable: false });
  });

  // Live evidence for this: a landing page with the snippet on the thank-you
  // page but not the home page reported "200.0% of 1". An owner reading that
  // concludes their message converts spectacularly, when in fact their
  // measurement is missing most of the traffic.
  it('withholds the rate when more people joined than were seen', () => {
    expect(conversionReadout(2, 1)).toEqual({ label: null, unreadable: true });
  });

  it('treats an equal count as readable — 100% is possible, over 100% is not', () => {
    expect(conversionReadout(5, 5)).toEqual({ label: '100.0% of 5', unreadable: false });
  });

  // No baseline yet is an unanswered question, not a broken one: it must not
  // raise the instrumentation warning on a page that has simply had no traffic.
  it('says nothing at all before any baseline arrives', () => {
    expect(conversionReadout(0, 0)).toEqual({ label: null, unreadable: false });
    expect(conversionReadout(3, 0)).toEqual({ label: null, unreadable: false });
  });
});
