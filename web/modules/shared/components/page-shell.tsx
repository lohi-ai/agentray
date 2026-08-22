'use client';

import type { CSSProperties, ReactNode } from 'react';

// PageShell is the layout every screen inside the app frame is built on.
//
// The shape is fixed so a reader never has to re-learn where a control lives:
//
//   ┌───────────────────────────────────────────────┐
//   │ banner (optional — demo bar, warnings)        │  auto
//   ├───────────────────────────────────────────────┤
//   │ title + subtitle          [action buttons]    │  auto
//   ├───────────────────────────────────────────────┤
//   │ tabs (optional)                               │  auto
//   ├──────────────────────────────┬────────────────┤
//   │ content                      │ aside          │  minmax(0,1fr)
//   └──────────────────────────────┴────────────────┘
//     minmax(0,1fr)                  auto
//
// Rows are declared from the slots that are actually filled, so an absent
// banner or tab strip contributes no track and therefore no gap — a static
// `grid-rows-[auto_auto_auto_1fr]` would leave a --pad-sized hole above the
// title on the majority of pages that have neither.
//
// `minmax(0,1fr)` rather than `1fr` in both axes: a bare `1fr` track is
// min-content-sized, so one wide table or a long unbroken id would push the
// content column past the viewport instead of scrolling inside it.

export type PageShellProps = {
  /** Full-width strip above the header — the demo bar, an outage notice. */
  banner?: ReactNode;
  title?: ReactNode;
  /** One line under the title saying what the screen answers. */
  sub?: ReactNode;
  /** Action buttons, right-aligned on the header row. */
  actions?: ReactNode;
  /** Tab strip — use <PageTabs>, or anything that renders one row. */
  tabs?: ReactNode;
  /** Right-hand context column. Absent → content spans the full width. */
  aside?: ReactNode;
  /**
   * Hand the content row to the child untouched: no padding, no scroll
   * container, no vertical rhythm. For screens that own their own scrolling
   * (chat).
   */
  bleed?: boolean;
  children: ReactNode;
};

export function PageShell({ banner, title, sub, actions, tabs, aside, bleed = false, children }: PageShellProps) {
  const hasHeader = !!(title || sub || actions);
  const rows = [
    banner ? 'auto' : null,
    hasHeader ? 'auto' : null,
    tabs ? 'auto' : null,
    'minmax(0,1fr)',
  ].filter(Boolean) as string[];

  const shell: CSSProperties = {
    gridTemplateRows: rows.join(' '),
    gap: 'var(--pad)',
    padding: 'var(--pad)',
  };

  const body: CSSProperties = {
    gridTemplateColumns: aside ? 'minmax(0,1fr) auto' : 'minmax(0,1fr)',
    gap: 'var(--pad)',
  };

  return (
    <div className="grid h-full min-h-0 min-w-0" style={shell}>
      {banner ? <div className="min-w-0">{banner}</div> : null}

      {/* auto-fit rather than `1fr auto`: an `auto` action track takes its
          max-content width, so a screen with four controls (Events) starved the
          title to a two-word column and then overflowed the page anyway. Here
          each track has a 320px floor, so the row is two columns while both fit
          and collapses to one — title above its actions — when they don't. */}
      {hasHeader ? (
        <header
          className="grid min-w-0 items-start gap-[var(--pad)]"
          style={{ gridTemplateColumns: actions ? 'repeat(auto-fit, minmax(min(100%,320px),1fr))' : 'minmax(0,1fr)' }}
        >
          <div className="min-w-0">
            {title ? (
              <h1 className="m-0 flex items-center gap-2 text-lg font-semibold tracking-[-0.02em]">{title}</h1>
            ) : null}
            {sub ? <p className="m-0 mt-0.5 text-sm text-[var(--color-text-secondary)]">{sub}</p> : null}
          </div>
          {actions ? <div className="flex flex-wrap items-center justify-end gap-2">{actions}</div> : null}
        </header>
      ) : null}

      {tabs ? <div className="min-w-0">{tabs}</div> : null}

      <div className="grid min-h-0 min-w-0" style={body}>
        {/* The content column, not the blocks in it, owns the vertical rhythm.
            Every top-level block used to carry its own trailing margin — the
            filter bar an mb-4, the stat strip an mb-4, a callout another — so
            the gap between two blocks depended on which of them came first,
            and a page that stacked two marginless panels got none at all.
            One `gap` here means every page breathes on the same step and a
            block can be moved or dropped without re-tuning its neighbours. */}
        {bleed ? (
          <div id="main-content" className="min-h-0 min-w-0 overflow-hidden">{children}</div>
        ) : (
          <div
            id="main-content"
            className="flex min-h-0 min-w-0 flex-col gap-[var(--pad)] overflow-y-auto overflow-x-hidden"
          >
            {children}
          </div>
        )}
        {aside ? (
          <aside
            className="hidden min-h-0 overflow-y-auto border-s border-[var(--color-border)] ps-[var(--pad)] [@media(min-width:1400px)]:block"
            style={{ width: 'var(--aside-w)' }}
          >
            {aside}
          </aside>
        ) : null}
      </div>
    </div>
  );
}

// PageTabs is the one tab strip in the product. Settings used to hand-roll it
// and four other screens used a segmented control for the same job, so the
// same interaction changed shape depending on which screen you were on.
export function PageTabs<T extends string>({
  tabs,
  value,
  onChange,
}: {
  tabs: readonly { id: T; label: ReactNode; badge?: ReactNode }[];
  value: T;
  onChange: (id: T) => void;
}) {
  return (
    <div role="tablist" className="flex min-w-0 gap-1 overflow-x-auto border-b border-[var(--color-border)]">
      {tabs.map((tab) => {
        const selected = tab.id === value;
        return (
          <button
            key={tab.id}
            role="tab"
            type="button"
            aria-selected={selected}
            onClick={() => onChange(tab.id)}
            className={`relative inline-flex min-h-10 flex-none items-center gap-2 whitespace-nowrap px-3 text-sm transition-colors ${
              selected
                ? "text-[var(--color-text-primary)] after:absolute after:inset-x-2 after:-bottom-px after:h-0.5 after:rounded-full after:bg-primary after:content-['']"
                : 'text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]'
            }`}
          >
            {tab.label}
            {tab.badge}
          </button>
        );
      })}
    </div>
  );
}

// AutoGrid is the responsive card grid: as many `min`-wide tracks as fit, at
// most `max` of them, on the shared spacing scale.
//
// It exists because Astryx's <Grid columns={{minWidth}}> emits
// `minmax(<min>px, …)` with no floor, so a 440px-minimum grid inside a 382px
// phone column lays out a 440px track and the card is clipped by the page's
// overflow-x — content simply disappears, with no scrollbar to reveal it.
// `min(100%, <min>px)` lets the track shrink to the container, which is what
// every caller meant. The `max` cap uses the same track-max arithmetic Astryx
// does, so column counts are unchanged.
export function AutoGrid({
  min,
  max,
  gap = 4,
  className,
  children,
}: {
  /** Ideal minimum track width in px — the wrap threshold. */
  min: number;
  /** Cap on the number of columns. Omit for as many as fit. */
  max?: number;
  /** Spacing step (×4px). */
  gap?: number;
  className?: string;
  children: ReactNode;
}) {
  const g = `${gap * 4}px`;
  const trackMax = max ? `calc((100% - ${max - 1} * ${g}) / ${max})` : '1fr';
  return (
    <div
      className={className}
      style={{
        display: 'grid',
        gap: g,
        gridTemplateColumns: `repeat(auto-fill, minmax(min(100%, ${min}px), ${trackMax}))`,
      }}
    >
      {children}
    </div>
  );
}

// AsideSection is the unit the right column is built from: a small caps label
// over its content. Keeps every aside on the same rhythm without each page
// inventing a heading style.
export function AsideSection({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return (
    <section className="mb-[var(--space-6)] last:mb-0">
      {title ? (
        <h2 className="m-0 mb-2 text-2xs font-medium uppercase tracking-[0.07em] text-[var(--color-text-secondary)]">
          {title}
        </h2>
      ) : null}
      {children}
    </section>
  );
}
