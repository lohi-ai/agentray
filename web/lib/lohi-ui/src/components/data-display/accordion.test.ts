import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it, vi } from 'vitest';
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from './accordion';

function item(value: string) {
  return createElement(
    AccordionItem,
    { value },
    createElement(AccordionTrigger, null, value),
    createElement(AccordionContent, null, `${value} content`),
  );
}

describe('vendored Lohi accordion', () => {
  it('preserves controlled single and non-collapsible Radix semantics', () => {
    const onValueChange = vi.fn();
    const html = renderToStaticMarkup(createElement(
      Accordion,
      { type: 'single', value: 'one', onValueChange, collapsible: false },
      item('one'),
      item('two'),
    ));
    expect(html).toContain('one content');
    expect(html).not.toContain('two content');
    expect(html).toContain('aria-disabled="true"');
  });

  it('preserves multiple-value API and renders every open region', () => {
    const html = renderToStaticMarkup(createElement(
      Accordion,
      { type: 'multiple', value: ['one', 'two'], onValueChange: () => undefined },
      item('one'),
      item('two'),
    ));
    expect(html).toContain('one content');
    expect(html).toContain('two content');
    expect(html.match(/role="region"/g)).toHaveLength(2);
  });
});
