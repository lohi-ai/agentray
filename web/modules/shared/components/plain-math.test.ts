import { describe, expect, it } from 'vitest';
import { remarkPlainMath, stripInlineMath } from '@/modules/shared/components/plain-math';

describe('unwrapInlineMath (via stripInlineMath)', () => {
  it('unwraps the LaTeX a model reaches for unprompted', () => {
    // Verbatim from a Growth Lead answer: nothing renders math, so the reader
    // saw the dollar signs in the middle of a sentence.
    expect(stripInlineMath('Conversion from user.pageview $to$ reader.chapter_open.'))
      .toBe('Conversion from user.pageview to reader.chapter_open.');
  });

  it('unwraps the escaped spelling too', () => {
    // What the stored answer actually contained. Astryx parses `\$` into its own
    // inline node, so by the time a text node reaches a plugin the pair is gone
    // — the escape has to be resolved before the parse.
    expect(stripInlineMath('user.pageview \\$to\\$ reader.chapter_open'))
      .toBe('user.pageview to reader.chapter_open');
  });

  it('unwraps a TeX command', () => {
    expect(stripInlineMath('the rate $\\alpha$ here')).toBe('the rate \\alpha here');
  });

  it('unwraps \\( \\) inline math', () => {
    expect(stripInlineMath('solve \\(x + 1\\) now')).toBe('solve x + 1 now');
  });

  it('never eats currency', () => {
    // The whole reason the rule is narrow: this product prints dollar amounts on
    // every agent-cost surface, and an over-eager unwrap would silently delete
    // the unit from a number about money.
    for (const money of [
      'AI cost $0.00 and $0.00 today',
      'from $5 to $10 per seat',
      'it costs $5.00$ apparently',
      'budget $1,240 vs $980',
    ]) {
      expect(stripInlineMath(money)).toBe(money);
    }
  });

  it('keeps escaped currency readable', () => {
    // `\$5` and `$5` render identically, so dropping the backslash is
    // display-neutral — what matters is that the amount survives.
    expect(stripInlineMath('spend went from \\$5 to \\$10')).toBe('spend went from $5 to $10');
  });

  it('leaves a lone dollar sign alone', () => {
    expect(stripInlineMath('spend is $0.00')).toBe('spend is $0.00');
  });
});

describe('stripInlineMath', () => {
  it('rewrites prose in a list item', () => {
    const md = '1. **Activation:** rate from `user.pageview` \\$to\\$ `reader.chapter_open`.';
    expect(stripInlineMath(md)).toBe('1. **Activation:** rate from `user.pageview` to `reader.chapter_open`.');
  });

  it('never touches a fenced block', () => {
    // A SQL fence is the most common thing on this surface, and `$to$` is valid
    // dollar-quoting — rewriting it would hand the reader a query that fails.
    const md = ['before $to$ after', '```sql', 'SELECT $to$ FROM t -- $to$', '```', 'tail $to$'].join('\n');
    expect(stripInlineMath(md)).toBe(
      ['before to after', '```sql', 'SELECT $to$ FROM t -- $to$', '```', 'tail to'].join('\n'),
    );
  });

  it('closes a fence only on its own delimiter', () => {
    const md = ['~~~', 'x $to$ y', '```', 'still code $to$', '~~~', 'prose $to$'].join('\n');
    const out = stripInlineMath(md).split('\n');
    expect(out[1]).toBe('x $to$ y');
    expect(out[3]).toBe('still code $to$');
    expect(out[5]).toBe('prose to');
  });

  it('never touches inline code', () => {
    expect(stripInlineMath('run `SELECT $to$ FROM t` then $to$ again'))
      .toBe('run `SELECT $to$ FROM t` then to again');
  });

  it('leaves an untouched answer byte-identical', () => {
    const md = 'Revenue was $1,240 last week.\n\n```js\nconst p = "$to$";\n```\n';
    expect(stripInlineMath(md)).toBe(md);
  });
});

describe('remarkPlainMath', () => {
  it('rewrites text nodes and leaves code untouched', () => {
    const tree = {
      type: 'root',
      children: [
        {
          type: 'paragraph',
          children: [
            { type: 'text', value: 'pageview $to$ signup' },
            { type: 'inlineCode', value: 'SELECT $to$ FROM t' },
          ],
        },
        { type: 'code', value: 'const price = "$to$";' },
      ],
    };
    remarkPlainMath()(tree);
    expect(tree.children[0].children![0].value).toBe('pageview to signup');
    // remark gives code its own node types, so the walker never rewrites them.
    expect(tree.children[0].children![1].value).toBe('SELECT $to$ FROM t');
    expect(tree.children[1].value).toBe('const price = "$to$";');
  });
});
