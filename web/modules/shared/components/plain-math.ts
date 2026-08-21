// Models reach for LaTeX unprompted — "conversion from pageview $to$ chapter_open"
// came back from a real answer. Nothing here renders math, so the delimiters
// showed up verbatim and the reader saw "$to$" in the middle of a sentence.
//
// Currency is the thing this must never break, so the rule is deliberately
// narrow: unwrap only when the delimited run is a single digit-free word, or
// starts with a TeX command. "$5 to $10" (a space) and "$5.00$" (digits) both
// survive; "$to$" and "$\alpha$" do not.
const INLINE_MATH = /\$([^$\n]{1,60})\$|\\\(([^\n]{1,120}?)\\\)/g;

/** The body a delimited run should collapse to, or null when it is not math and
 *  has to stay verbatim. */
function mathBody(dollar: string | undefined, paren: string | undefined): string | null {
  if (paren != null) return paren.trim();
  const body = dollar ?? '';
  const texish = body.startsWith('\\') || !/[\s\d]/.test(body);
  return texish ? body : null;
}

/** Unwraps a run of prose the markdown parser has already isolated from code. */
export function unwrapInlineMath(value: string): string {
  // Models escape the delimiters as often as not (`\$to\$`), and markdown
  // renders `\$` as `$` either way — so dropping the backslash first is
  // display-neutral and lets one rule cover both spellings.
  return value
    .replace(/\\\$/g, '$')
    .replace(INLINE_MATH, (match, dollar?: string, paren?: string) => mathBody(dollar, paren) ?? match);
}

// A fence opens on three backticks or three tildes at any indent. Over-matching
// an indented line only costs an unwrap we skip; under-matching would rewrite
// someone's code.
const FENCE = /^\s*(`{3,}|~{3,})/;
// Inline code. Non-greedy against a same-length delimiter, so ``a `b` c`` closes
// on its own run and not on the one inside it.
const CODE_SPAN = /(`+)[\s\S]*?\1/g;

function rewriteProse(line: string): string {
  let out = '';
  let last = 0;
  let match: RegExpExecArray | null;
  CODE_SPAN.lastIndex = 0;
  while ((match = CODE_SPAN.exec(line)) !== null) {
    out += unwrapInlineMath(line.slice(last, match.index)) + match[0];
    last = match.index + match[0].length;
  }
  return out + unwrapInlineMath(line.slice(last));
}

/** Strips inline LaTeX from a raw markdown string, leaving fenced blocks and
 *  inline code exactly as written.
 *
 *  Chat needs the string form because its renderer (Astryx `Markdown`) parses
 *  escapes into separate inline nodes: by the time a text node reaches a plugin,
 *  `\$to\$` has become `$` / `to` / `$` and the pair is no longer recognisable.
 *  The one approximation is a 4-space indented code block, which reads as prose
 *  here — a rewrite there is cosmetic, and the agent writes fences. */
export function stripInlineMath(markdown: string): string {
  let fence: string | null = null;
  return markdown
    .split('\n')
    .map((line) => {
      const opener = FENCE.exec(line);
      if (fence !== null) {
        // Closes on the same character, at least as long as the opener.
        if (opener && opener[1][0] === fence[0] && opener[1].length >= fence.length) fence = null;
        return line;
      }
      if (opener) {
        fence = opener[1];
        return line;
      }
      return rewriteProse(line);
    })
    .join('\n');
}

type MdastNode = { type: string; value?: string; children?: MdastNode[] };

// remarkPlainMath is the same rule for the react-markdown surfaces (the daily
// readout), where remark has already parsed code into its own node types — so
// the walker only ever sees prose.
export function remarkPlainMath() {
  return (tree: MdastNode) => {
    const walk = (node: MdastNode) => {
      if (node.type === 'text' && typeof node.value === 'string') {
        node.value = unwrapInlineMath(node.value);
        return;
      }
      node.children?.forEach(walk);
    };
    walk(tree);
  };
}
