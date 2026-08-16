// validation-readout.ts — the arithmetic behind the validate scoreboard, kept
// out of the component so it can be tested without a DOM.
//
// The one rule that matters here: a conversion rate above 100% is not a great
// result, it is a broken measurement. More people joined the waitlist than the
// page recorded as having seen it means the tracking snippet is missing from
// some of the pages, and printing "200.0%" would turn that hole into a
// congratulation the owner then acts on.

export type ConversionReadout = {
  /** What to show in the stat, or `null` when there is nothing honest to show. */
  label: string | null;
  /** True when the baseline undercounts and the rate must be withheld. */
  unreadable: boolean;
};

export function conversionReadout(metric: number, baseline: number): ConversionReadout {
  if (baseline <= 0) return { label: null, unreadable: false };
  if (metric > baseline) return { label: null, unreadable: true };
  return { label: `${((metric / baseline) * 100).toFixed(1)}% of ${baseline}`, unreadable: false };
}
