'use client';

import type { ReactNode } from 'react';
import { ArrowDown, ArrowUp } from 'lucide-react';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button as AstryxButton } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack } from '@astryxdesign/core/HStack';
import { VStack } from '@astryxdesign/core/VStack';
import { Heading } from '@astryxdesign/core/Heading';
import { Text } from '@astryxdesign/core/Text';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { EmptyState as AstryxEmptyState } from '@astryxdesign/core/EmptyState';
import { Spinner } from '@astryxdesign/core/Spinner';
import { Table } from '@astryxdesign/core/Table';
import type { TablePlugin } from '@astryxdesign/core/Table';
import { useAuthStore } from '@/lib/app-state';

export type Tone = 'agent' | 'warning' | 'success' | 'danger';

// Astryx migration note: these shared primitives keep their exported APIs intact
// (22 consumers), but their *neutral* surfaces/text/borders now reference Astryx's
// mode-aware tokens (`--color-background-*`, `--color-text-*`, `--color-border`) via
// arbitrary-value utilities, so every consuming surface reads correctly in light
// AND dark. Saturated *brand/semantic* colors (green `--primary`, purple `--agent`,
// `--success`/`--warning`/`--danger`/`--data`) stay constant — they're legible in
// both modes by design. We reference the Astryx vars directly rather than via the
// `bg-card`/`text-primary` utility names because this app's own `@theme inline`
// block (globals.css) shadows those names back onto the legacy dark-only tokens.

// Astryx migration: the shared Button now delegates to Astryx's <Button> so the
// whole app composes from the Astryx component (one source for focus ring, press
// scale, loading, a11y). We keep the legacy 4-variant API (primary/agent/outline/
// ghost) intact for all 22 consumers by mapping onto Astryx variants. Brand colors
// ride the `--color-accent → --primary` bridge (globals.css), so Astryx primary is
// AgentRay green; the `agent` variant has no Astryx equivalent, so we tint a
// primary button purple via token utilities. Prototype shape (radius-md, 34/28px
// height) is restored globally by the `.astryx-button` override in globals.css.
const ASTRYX_VARIANT = { primary: 'primary', agent: 'primary', outline: 'secondary', ghost: 'ghost' } as const;

export function Button({ children, variant, size, icon, onClick, disabled }: { children: ReactNode; variant: 'primary' | 'agent' | 'outline' | 'ghost'; size?: 'sm'; icon?: ReactNode; onClick?: () => void; disabled?: boolean }) {
  const label = typeof children === 'string' ? children : '';
  return (
    <AstryxButton
      variant={ASTRYX_VARIANT[variant]}
      size={size === 'sm' ? 'sm' : 'md'}
      icon={icon}
      label={label}
      onClick={onClick}
      isDisabled={disabled}
      className={variant === 'agent' ? '![background:var(--agent)] !text-[var(--agent-foreground)]' : undefined}
    >
      {children}
    </AstryxButton>
  );
}

// Astryx migration: the page header is now an Astryx <HStack> (title block start,
// action end) wrapping a <VStack> of an Astryx <Heading> + supporting <Text>. The
// title uses level 2 (font-size-xl ≈ the prototype's 19px) and stays a flex row so
// an inline icon can sit beside the text.
export function Intro({ title, sub, action }: { title: ReactNode; sub: string; action?: ReactNode }) {
  return (
    <HStack align="start" justify="between" gap={4} className="mb-[18px]">
      <VStack gap={0.5}>
        <Heading level={2} className="flex items-center gap-2 tracking-[-0.02em]">{title}</Heading>
        <Text type="supporting">{sub}</Text>
      </VStack>
      {action}
    </HStack>
  );
}

// Astryx migration: the context chips are now Astryx <Badge>s (neutral/muted pill)
// in a wrapping <HStack>. The label keeps a secondary caption + emphasized value.
export function ContextChips({ range, extra }: { range: string; extra?: ReactNode }) {
  const project = useAuthStore((s) => s.project);
  return (
    <HStack gap={2} className="my-0.5 mb-4 flex-wrap">
      <Badge variant="neutral" label={<span>Project <b className="font-medium text-[var(--color-text-primary)]">{project?.name || '—'}</b></span>} />
      <Badge variant="neutral" label={<span>Range <b className="font-medium text-[var(--color-text-primary)]">{range}</b></span>} />
      {extra}
    </HStack>
  );
}

// Astryx migration: the stat strip is now an Astryx <Card> wrapping a responsive
// <Grid> (auto-fit, max 6 tracks — replaces the manual 6/3/2 breakpoints). Each
// stat is a <VStack> of an Astryx <Text> label + a tabular-numbers <Text size="xl">
// value (≈ the prototype's 21px metric). Semantic tones aren't in Text's color
// enum, so toned values keep a token-backed inline color; deltas use the
// success/danger brand tokens, which stay constant across light/dark by design.
export function StatsStrip({ stats }: { stats: Array<{ label: string; value: string; tone?: Tone; delta?: string; deltaTone?: 'up' | 'down' }> }) {
  return (
    <Card padding={1} className="mb-4">
      <Grid columns={{ minWidth: 140, max: 6 }} gap={0}>
        {stats.map((stat) => (
          <VStack key={stat.label} gap={1} className="px-4 py-[13px]">
            <Text type="supporting" maxLines={1}>{stat.label}</Text>
            <Text weight="semibold" hasTabularNumbers className="text-[length:var(--font-size-xl)] leading-tight tracking-[-0.02em]" style={stat.tone ? { color: `var(--${stat.tone})` } : undefined}>{stat.value}</Text>
            {stat.delta ? (
              <HStack align="center" gap={0.5} className={stat.deltaTone === 'down' ? 'text-danger' : 'text-success'}>
                {stat.deltaTone === 'down' ? <ArrowDown size={13} /> : <ArrowUp size={13} />}
                <Text type="supporting" color="inherit">{stat.delta}</Text>
              </HStack>
            ) : null}
          </VStack>
        ))}
      </Grid>
    </Card>
  );
}

const PILL_TONE: Record<string, string> = { working: 'text-agent', healthy: 'text-success', attention: 'text-warning', paused: 'text-[var(--color-text-secondary)]' };
const DOT_TONE: Record<string, string> = { working: 'bg-agent text-agent', healthy: 'bg-success text-success', attention: 'bg-warning text-warning', paused: 'bg-[var(--color-text-disabled)]', idle: 'bg-[var(--color-text-disabled)]' };

export function StatusPill({ status, label, grow = true }: { status: string; label: string; grow?: boolean }) {
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-[20px] bg-[var(--color-background-muted)] px-[9px] py-[3px] text-[11.5px] ${PILL_TONE[status] ?? ''} ${grow ? 'ms-auto' : ''}`}>
      <span className={`relative inline-block h-2 w-2 flex-none rounded-full ${DOT_TONE[status] ?? ''} ${status !== 'paused' && status !== 'idle' ? "after:absolute after:inset-0 after:rounded-full after:[animation:pulse_2s_var(--ease)_infinite] after:content-['']" : ''}`} />
      {label}
    </span>
  );
}

// Astryx migration: the callout is now Astryx's <Banner> — the canonical notice
// component (status icon + colored header, native title/description/endContent and
// a11y). tone maps onto Banner status; the prototype's uppercase eyebrow label is
// kept as a muted prefix on the title (the status color/icon already categorize).
const CALLOUT_STATUS = { growth: 'success', agentic: 'info', warn: 'warning' } as const;

export function Callout({ tone, icon, label, title, detail, action }: { tone: 'growth' | 'agentic' | 'warn'; icon: ReactNode; label: string; title: string; detail: string; action?: ReactNode }) {
  return (
    <Banner
      className="mb-4"
      status={CALLOUT_STATUS[tone]}
      icon={icon}
      title={<><Text type="supporting" className="me-2 uppercase tracking-[0.06em]">{label}</Text>{title}</>}
      description={detail}
      endContent={action}
    />
  );
}

// Astryx migration: the prototype's borderless card panel is now Astryx's <Card>
// (themed card surface, radius, and divider-ready padding). The header row is an
// <HStack> with the title as a compact <Heading level={5}> (font-size-sm semibold,
// the closest native step to the prototype's 13px title) and the action end-aligned.
export function Panel({ title, action, children }: { title: string; action?: ReactNode; children: ReactNode }) {
  return (
    <Card padding={4}>
      <HStack align="center" justify="between" className="mb-3">
        <Heading level={5}>{title}</Heading>
        {action}
      </HStack>
      {children}
    </Card>
  );
}

type SegmentOption = string | { value: string; label: string };

// Astryx migration: the prototype's segmented toggle is now Astryx's
// <SegmentedControl> (radio-group semantics, focus ring, keyboard nav). The
// muted track + surface-3 active thumb match the prototype via the
// `--color-neutral → surface-2` / `--color-background-surface → surface-3`
// bridges (globals.css). Keep the legacy API: value is optional (defaults to the
// first option) since several callers drive it uncontrolled.
export function Segment({ options, value, onChange }: { options: SegmentOption[]; value?: string; onChange?: (option: string) => void }) {
  const items = options.map((option) => (typeof option === 'string' ? { value: option, label: option } : option));
  const current = value ?? items[0]?.value ?? '';
  return (
    <SegmentedControl value={current} onChange={(next) => onChange?.(next)} label="Toggle" size="sm">
      {items.map((item) => (
        <SegmentedControlItem key={item.value} value={item.value} label={item.label} />
      ))}
    </SegmentedControl>
  );
}

// Astryx migration: delegates to Astryx <EmptyState> (role="status", semantic
// heading, compact spacing). Keeps the legacy icon/title/detail/action API; the
// dashed-border treatment is dropped in favor of Astryx's own empty-state design.
export function EmptyState({ icon, title, detail, action }: { icon?: ReactNode; title: string; detail?: string; action?: ReactNode }) {
  return <AstryxEmptyState icon={icon} title={title} description={detail} actions={action} isCompact />;
}

// BarRows renders a ranked value/count list as the prototype's bar table. The
// bar width scales each row against the largest count (max 88px, like the mock).
// Astryx migration: delegates to the data-driven Astryx <Table> (compact density,
// theme-token cells, accessible header scope). renderCell keeps the prototype's
// inline --data bar + monospace count; count column is end-aligned.
export function BarRows({ rows, valueHead = 'Source', countHead = 'Count', mono = false, empty = 'No data yet' }: { rows: Array<{ value: string; count: number }>; valueHead?: string; countHead?: string; mono?: boolean; empty?: string }) {
  const max = rows.reduce((m, r) => Math.max(m, r.count), 0) || 1;
  if (rows.length === 0) return <p style={{ color: 'var(--color-text-secondary)', fontSize: 12.5, margin: 0 }}>{empty}</p>;
  const columns = [
    {
      key: 'value',
      header: valueHead,
      align: 'start' as const,
      renderCell: (row: { value: string; count: number }) => (
        <span className="flex items-center gap-2">
          <span className="h-1.5 rounded-[3px] bg-[color-mix(in_srgb,var(--data)_55%,transparent)]" style={{ width: Math.max(6, Math.round((row.count / max) * 88)) }} />
          <span className={mono ? 'font-mono tabular-nums' : undefined}>{row.value || '(none)'}</span>
        </span>
      ),
    },
    {
      key: 'count',
      header: countHead,
      align: 'end' as const,
      renderCell: (row: { value: string; count: number }) => (
        <span className="font-mono tabular-nums">{new Intl.NumberFormat('en-US').format(row.count)}</span>
      ),
    },
  ];
  return <Table data={rows} columns={columns} density="compact" />;
}

// rowNavPlugin makes an Astryx data-driven <Table>'s rows clickable — the
// idiomatic Astryx way (data-driven Table has no onRowClick prop, so row-level
// behavior is injected through the plugin pipeline's transformBodyRow hook). It
// sets the row's onClick + a pointer cursor, preserving the prototype's
// whole-row navigation on the fleet/run tables. Usage: plugins={{ nav: rowNavPlugin(fn) }}.
export function rowNavPlugin<T extends Record<string, unknown>>(onRowClick: (item: T) => void): TablePlugin<T> {
  return {
    transformBodyRow: (props, item) => ({
      ...props,
      htmlProps: {
        ...props.htmlProps,
        onClick: () => onRowClick(item),
        style: { ...props.htmlProps.style, cursor: 'pointer' },
      },
    }),
  };
}

// Astryx migration: the prototype's pulsing dot is now Astryx's <Spinner>
// (canvas arc, reduced-motion aware). Kept inline inside the card panel for the
// prototype's horizontal loading-row layout, so the spinner sits beside the label.
export function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <Card padding={4}>
      <HStack align="center" gap={2}>
        <Spinner size="sm" shade="subtle" aria-label={label} />
        <Text type="supporting">{label}</Text>
      </HStack>
    </Card>
  );
}
