'use client';

import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Table, TableHeader, TableBody, TableRow, TableHeaderCell, TableCell } from '@astryxdesign/core/Table';
import { Card } from '@astryxdesign/core/Card';
import { Chart, type ChartSpec } from '@/modules/shared/components/charts';

// ChartFence draws a ```chart code fence — the agent's way to render a real
// graph inline, the same ECharts engine the dashboard uses. The fence body is a
// JSON ChartSpec; if it doesn't parse we fall back to showing the raw source so
// a malformed spec degrades to visible text instead of a blank.
function ChartFence({ source }: { source: string }) {
  let spec: ChartSpec | null = null;
  try {
    spec = JSON.parse(source) as ChartSpec;
  } catch {
    spec = null;
  }
  if (!spec || (!spec.series && !spec.slices)) {
    return <pre className="m-0 overflow-x-auto rounded-md border border-[var(--color-border)] bg-[var(--color-background-card)] px-3 py-2.5 text-xs"><code>{source}</code></pre>;
  }
  return <div className="py-2"><Chart spec={spec} /></div>;
}

// AgentMarkdown renders an agent's reply as GitHub-flavored markdown via
// react-markdown + remark-gfm — headings, lists, **bold**, fenced code, and pipe
// tables, so a data answer reads as structured content instead of one collapsed
// wall of text. A ```chart fence is intercepted and drawn as a real ECharts graph
// (see ChartFence); everything else uses the library's defaults, themed by the
// `.md` rules in globals.css. Shared by chat and the dashboard daily readout.
export function AgentMarkdown({ text }: { text: string }) {
  return (
    <div className="[&>*]:m-0 [&>*+*]:mt-2 [&>:first-child]:mt-0 [&_strong]:font-[650] [&_h1]:mt-[14px] [&_h1]:text-[15px] [&_h1]:font-[650] [&_h1]:leading-[1.4] [&_h2]:mt-3 [&_h2]:text-sm [&_h2]:font-[650] [&_h2]:leading-[1.4] [&_h2]:text-[var(--color-text-primary)] [&_h3]:mt-[10px] [&_h3]:text-[13.5px] [&_h3]:font-[650] [&_h3]:leading-[1.4] [&_h3]:text-[var(--color-text-secondary)] [&_h4]:mt-[10px] [&_h4]:text-[13.5px] [&_h4]:font-[650] [&_h4]:leading-[1.4] [&_h4]:text-[var(--color-text-secondary)] [&_h5]:mt-[10px] [&_h5]:text-[13.5px] [&_h5]:font-[650] [&_h5]:leading-[1.4] [&_h5]:text-[var(--color-text-secondary)] [&_h6]:mt-[10px] [&_h6]:text-[13.5px] [&_h6]:font-[650] [&_h6]:leading-[1.4] [&_h6]:text-[var(--color-text-secondary)] [&_a]:text-primary [&_a]:underline [&_ul]:m-0 [&_ul]:flex [&_ul]:flex-col [&_ul]:gap-[3px] [&_ul]:pl-5 [&_ol]:m-0 [&_ol]:flex [&_ol]:flex-col [&_ol]:gap-[3px] [&_ol]:pl-5 [&_li]:pl-0.5 [&_ul_li]:list-disc [&_ol_li]:list-decimal">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          // Unwrap the library's <pre>: block code renders its own wrapper below,
          // and a ```chart fence renders a <div> (a chart), which must not nest
          // inside a <pre>.
          pre({ children }) {
            return <>{children}</>;
          },
          code({ className, children }) {
            const raw = String(children);
            const body = raw.replace(/\n$/, '');
            const lang = /language-(\w+)/.exec(className || '')?.[1];
            // react-markdown v10 drops the `inline` flag: a fenced block has a
            // language class or a trailing newline; everything else is inline.
            const isBlock = !!className || raw.includes('\n');
            if (lang === 'chart') return <ChartFence source={body} />;
            if (!isBlock) return <code className="rounded-[5px] border border-[var(--color-border)] bg-[var(--color-background-card)] px-[5px] py-px text-xs">{children}</code>;
            return <pre className="m-0 overflow-x-auto rounded-md border border-[var(--color-border)] bg-[var(--color-background-card)] px-3 py-2.5 text-xs"><code className={className}>{body}</code></pre>;
          },
          // Astryx migration: a GFM pipe table now renders through Astryx's
          // composable Table primitives (TableHeader/Body/Row/HeaderCell/Cell) in
          // children mode — react-markdown's parsed thead/tbody/tr/th/td are mapped
          // onto the Astryx components so the agent's tabular answers inherit the
          // design system's density, dividers, and themed surfaces. Wrapped for
          // horizontal scroll on narrow chat columns.
          table({ children }) {
            // Card padding absorbs the Astryx Table's container-bleed so cell
            // content aligns to the border edge (a zero-padding box clips it).
            return <Card padding={4}><Table density="compact">{children}</Table></Card>;
          },
          thead({ children }) { return <TableHeader>{children}</TableHeader>; },
          tbody({ children }) { return <TableBody>{children}</TableBody>; },
          tr({ children }) { return <TableRow>{children}</TableRow>; },
          th({ children }) { return <TableHeaderCell>{children}</TableHeaderCell>; },
          td({ children }) { return <TableCell>{children}</TableCell>; },
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
