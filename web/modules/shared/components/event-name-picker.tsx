'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { ChevronsUpDown, List, ListFilter, Search, X } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector, type SelectorOptionData } from '@astryxdesign/core/Selector';
import { useEventNames } from '@/modules/app/hooks';
import type { EventCatalogEntry } from '@/lib/api';
import { formatCompact, formatRelative } from '@/lib/format';

// Shared event-name affordances. Across the app, people filter, chart, and query
// by event_name but can't recall the exact string — so every event-name input is
// fed by the project's distinct-name catalog (useEventNames) rather than a bare
// text box. Two pieces: EventNameCombobox (type-ahead picker that still accepts
// free text) and EventCatalog (a browsable click-to-pick list).

// rank does a forgiving substring match and orders exact/prefix hits first so the
// name you half-typed surfaces at the top.
function rank(entries: EventCatalogEntry[], q: string): EventCatalogEntry[] {
  const query = q.trim().toLowerCase();
  if (!query) return entries;
  const scored = entries
    .map((e) => {
      const name = e.event_name.toLowerCase();
      if (!name.includes(query)) return null;
      const score = name === query ? 0 : name.startsWith(query) ? 1 : 2;
      return { e, score };
    })
    .filter((x): x is { e: EventCatalogEntry; score: number } => x !== null);
  scored.sort((a, b) => a.score - b.score);
  return scored.map((x) => x.e);
}

export function EventNameCombobox({
  value,
  onChange,
  onCommit,
  placeholder = 'Event name…',
  className,
  allowFreeText = true,
}: {
  value: string;
  // Fires on every keystroke and on selection — keep parent state in sync.
  onChange: (next: string) => void;
  // Fires only on an explicit choice (pick a row, press Enter, or clear). Wire
  // this to anything expensive (e.g. a refetch) so typing doesn't trigger it.
  // Defaults to onChange when omitted.
  onCommit?: (next: string) => void;
  placeholder?: string;
  className?: string;
  // When false, the field only commits names from the catalog (used where a
  // typo'd name would silently match nothing). Defaults to true so it can also
  // act as a permissive filter box.
  allowFreeText?: boolean;
}) {
  const { names, loading } = useEventNames();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState(value);
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);

  // Adjust the editing buffer when the controlled value changes externally
  // (e.g. cleared from a context chip) — render-phase sync, no effect needed.
  const [prevValue, setPrevValue] = useState(value);
  if (value !== prevValue) {
    setPrevValue(value);
    setDraft(value);
  }

  // Close on outside click so the dropdown behaves like a native control.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  const matches = useMemo(() => rank(names, draft).slice(0, 50), [names, draft]);

  function commit(next: string) {
    setDraft(next);
    onChange(next);
    (onCommit ?? onChange)(next);
    setOpen(false);
  }

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') { e.preventDefault(); setOpen(true); setActive((i) => Math.min(i + 1, matches.length - 1)); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setActive((i) => Math.max(i - 1, 0)); }
    else if (e.key === 'Enter') {
      e.preventDefault();
      const pick = matches[active];
      if (pick) commit(pick.event_name);
      else if (allowFreeText) commit(draft.trim());
    } else if (e.key === 'Escape') { setOpen(false); }
  }

  return (
    <div ref={rootRef} className={`relative ${className ?? ''}`}>
      <span className="inline-flex h-8 w-full items-center gap-2 rounded-sm border border-[var(--color-border)] bg-[var(--color-background-muted)] px-3 text-[var(--color-text-secondary)] focus-within:border-primary">
        <Search size={14} className="flex-none" />
        <input
          className="min-w-0 flex-1 border-none bg-transparent text-[12.5px] text-[var(--color-text-primary)] outline-none"
          value={draft}
          placeholder={placeholder}
          onChange={(e) => { setDraft(e.target.value); setOpen(true); setActive(0); if (allowFreeText) onChange(e.target.value); }}
          onFocus={() => setOpen(true)}
          onKeyDown={onKeyDown}
        />
        {draft ? (
          <button type="button" className="flex-none text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]" title="Clear" onClick={() => commit('')}><X size={13} /></button>
        ) : (
          <ChevronsUpDown size={13} className="flex-none opacity-60" />
        )}
      </span>
      {open ? (
        <div className="absolute z-30 mt-1 max-h-[280px] w-full min-w-[240px] overflow-auto rounded-md border border-[var(--color-border)] bg-[var(--color-background-card)] p-1 shadow-[0_16px_40px_rgba(0,0,0,0.35)]">
          {loading ? (
            <div className="px-2.5 py-2 text-xs text-[var(--color-text-secondary)]">Loading event names…</div>
          ) : matches.length === 0 ? (
            <div className="px-2.5 py-2 text-xs text-[var(--color-text-secondary)]">
              {names.length === 0 ? 'No events captured yet.' : 'No event name matches.'}
            </div>
          ) : (
            matches.map((m, i) => (
              <button
                type="button"
                key={m.event_name}
                onMouseEnter={() => setActive(i)}
                onClick={() => commit(m.event_name)}
                className={`flex w-full items-center gap-2 rounded-sm px-2.5 py-1.5 text-left text-[12.5px] ${i === active ? 'bg-[var(--color-background-surface)]' : ''} ${m.event_name === value ? 'text-primary' : 'text-[var(--color-text-primary)]'}`}
              >
                <span className="min-w-0 flex-1 truncate font-mono">{m.event_name}</span>
                {m.event_type ? <span className="flex-none text-[10.5px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">{m.event_type}</span> : null}
                <span className="flex-none font-mono tabular-nums text-[11px] text-[var(--color-text-secondary)]">{formatCompact(m.count)}</span>
              </button>
            ))
          )}
        </div>
      ) : null}
    </div>
  );
}

// EventNameSelect is a pick-first filter control: a dropdown trigger that reads
// like a faceted filter (current selection or "All events") and opens a popover
// of the project's event names to click. Unlike EventNameCombobox it does not
// accept free text — the whole point is that people choose a known name instead
// of typing one. A search box inside the popover narrows long catalogs; the top
// row clears the filter. Use this for the Events screen filter; use the combobox
// where free text is still wanted (chart builder).
export function EventNameSelect({
  value,
  onChange,
  placeholder = 'All events',
  className,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
  className?: string;
}) {
  const { names, loading } = useEventNames();

  // Look up the catalog entry by name so renderOption can show the type + count
  // alongside each name (Astryx option data only carries value/label/icon).
  const byName = useMemo(() => new Map(names.map((n) => [n.event_name, n] as const)), [names]);
  const options = useMemo<SelectorOptionData[]>(
    () => names.map((n) => ({ value: n.event_name, label: n.event_name })),
    [names],
  );

  function renderOption(option: SelectorOptionData) {
    const entry = byName.get(option.value);
    return (
      <span className="flex w-full items-center gap-2">
        <span className="min-w-0 flex-1 truncate font-mono">{option.label}</span>
        {entry?.event_type ? <span className="flex-none text-[10.5px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">{entry.event_type}</span> : null}
        {entry ? <span className="flex-none font-mono tabular-nums text-[11px] text-[var(--color-text-secondary)]">{formatCompact(entry.count)}</span> : null}
      </span>
    );
  }

  return (
    <div className={className}>
      <Selector
        label="Filter by event name"
        isLabelHidden
        size="sm"
        startIcon={ListFilter}
        placeholder={placeholder}
        hasSearch
        searchPlaceholder="Filter event names…"
        hasClear
        isLoading={loading}
        options={options}
        value={value || null}
        onChange={(next) => onChange(next ?? '')}
        renderOption={renderOption}
      />
    </div>
  );
}

// EventCatalog is the always-visible browsable list of the project's event names
// — the "I can't remember the name" reference. Clicking a row hands the name back
// (e.g. to set a filter or insert into SQL).
export function EventCatalog({ onPick, selected, title = 'Event names', max = 200 }: { onPick: (name: string) => void; selected?: string; title?: string; max?: number }) {
  const { names, loading } = useEventNames();
  const [q, setQ] = useState('');
  const matches = useMemo(() => rank(names, q).slice(0, max), [names, q, max]);

  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-3">
      <div className="mb-2 flex items-center gap-2">
        <List size={14} className="text-[var(--color-text-secondary)]" />
        <span className="text-[12.5px] font-semibold">{title}</span>
        <span className="text-[11px] text-[var(--color-text-secondary)]">{names.length}</span>
        <TextInput label="Filter event names" isLabelHidden size="sm" startIcon={Search} value={q} placeholder="Filter…" onChange={(v) => setQ(v)} width={150} className="ms-auto" />
      </div>
      <div className="flex max-h-[420px] flex-col gap-0.5 overflow-auto">
        {loading ? (
          <div className="px-2 py-2 text-xs text-[var(--color-text-secondary)]">Loading…</div>
        ) : matches.length === 0 ? (
          <div className="px-2 py-2 text-xs text-[var(--color-text-secondary)]">{names.length === 0 ? 'No events captured yet.' : 'No match.'}</div>
        ) : (
          matches.map((m) => (
            <button
              type="button"
              key={m.event_name}
              onClick={() => onPick(m.event_name)}
              className={`flex items-center gap-2 rounded-sm px-2 py-1.5 text-left text-[12.5px] transition-colors hover:bg-[var(--color-background-surface)] ${m.event_name === selected ? 'bg-[var(--color-background-surface)] text-primary' : 'text-[var(--color-text-primary)]'}`}
              title={`Last seen ${formatRelative(m.last_seen)}`}
            >
              <span className="min-w-0 flex-1 truncate font-mono">{m.event_name}</span>
              {m.event_type ? <span className="flex-none text-[10px] uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">{m.event_type}</span> : null}
              <span className="flex-none font-mono tabular-nums text-[11px] text-[var(--color-text-secondary)]">{formatCompact(m.count)}</span>
            </button>
          ))
        )}
      </div>
    </div>
  );
}
