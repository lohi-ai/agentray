'use client';

import { useMemo, useState } from 'react';
import { Columns3, Search } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { EVENTS_COLUMNS, EVENTS_TABLE } from './events-schema';

// SchemaReference is the browsable column list for the one events table — the
// "what can I select?" companion to the event-name catalog. Clicking a column
// drops its name into the editor at the caret (onPick), mirroring EventCatalog.
export function SchemaReference({ onPick }: { onPick: (column: string) => void }) {
  const [q, setQ] = useState('');
  const matches = useMemo(() => {
    const query = q.trim().toLowerCase();
    if (!query) return EVENTS_COLUMNS;
    return EVENTS_COLUMNS.filter((c) => c.name.toLowerCase().includes(query) || c.type.toLowerCase().includes(query));
  }, [q]);

  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-3">
      <div className="mb-2 flex items-center gap-2">
        <Columns3 size={14} className="text-[var(--color-text-secondary)]" />
        <span className="text-[12.5px] font-semibold">{EVENTS_TABLE}</span>
        <span className="text-[11px] text-[var(--color-text-secondary)]">{EVENTS_COLUMNS.length} cols</span>
        <TextInput label="Filter columns" isLabelHidden size="sm" startIcon={Search} value={q} placeholder="Filter…" onChange={(v) => setQ(v)} width={130} className="ms-auto" />
      </div>
      <div className="flex max-h-[420px] flex-col gap-0.5 overflow-auto">
        {matches.length === 0 ? (
          <div className="px-2 py-2 text-xs text-[var(--color-text-secondary)]">No column matches.</div>
        ) : (
          matches.map((c) => (
            <button
              type="button"
              key={c.name}
              onClick={() => onPick(c.name)}
              className="flex flex-col gap-0.5 rounded-sm px-2 py-1.5 text-left transition-colors hover:bg-[var(--color-background-surface)]"
              title={c.note}
            >
              <span className="flex items-center gap-2">
                <span className="min-w-0 flex-1 truncate font-mono text-[12.5px] text-[var(--color-text-primary)]">{c.name}</span>
                <span className="flex-none font-mono text-[10.5px] text-[var(--color-text-secondary)]">{c.type}</span>
              </span>
              {c.note ? <span className="truncate text-[11px] text-[var(--color-text-secondary)]">{c.note}</span> : null}
            </button>
          ))
        )}
      </div>
    </div>
  );
}
