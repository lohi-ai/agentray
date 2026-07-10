'use client';

import { LayoutTemplate } from 'lucide-react';
import { useTemplates } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Intro } from '@/modules/shared/components/signal-primitives';

export function TemplatesPage() {
  const { templates, applyTemplate } = useTemplates();

  return (
    <AppShell active="dashboards">
      <Intro title="Templates" sub="Start from a ready-made dashboard instead of a blank canvas." />
      {templates.length === 0 ? (
        <EmptyState icon={<LayoutTemplate size={22} />} title="No templates available" detail="System templates will appear here once published." />
      ) : (
        <div className="grid grid-cols-3 gap-3.5 max-[980px]:grid-cols-1">
          {templates.map((t) => (
            <div className="rounded-xl bg-[var(--color-background-card)] p-4" key={t.id}>
              <div className="flex items-center gap-2 mb-3"><h3 className="m-0 text-[13px] font-semibold">{t.name}</h3>{t.is_system ? <span className="rounded-full bg-[var(--color-background-muted)] px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)]">system</span> : null}</div>
              <p style={{ color: 'var(--muted-foreground)', fontSize: 12.5, margin: '0 0 12px' }}>{t.description || `${t.charts.length} chart${t.charts.length === 1 ? '' : 's'}`}</p>
              <Button variant="primary" size="sm" icon={<LayoutTemplate size={15} />} onClick={() => void applyTemplate(t.id)}>Use template</Button>
            </div>
          ))}
        </div>
      )}
    </AppShell>
  );
}
