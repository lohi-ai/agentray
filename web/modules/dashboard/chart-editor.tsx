'use client';

import { useEffect, useMemo, useState } from 'react';
import { TextInput } from '@astryxdesign/core/TextInput';
import type { Chart, ChartInput } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';
import { useActivity } from '@/modules/app/hooks';
import { Modal } from '@/modules/shared/components/modal';
import { EventNameCombobox } from '@/modules/shared/components/event-name-picker';
import { Button, Segment } from '@/modules/shared/components/signal-primitives';
import { ChartCard } from './chart-card';

type Source = 'metric' | 'sql';

const METRICS: Array<{ value: Chart['metric']; label: string }> = [
  { value: 'events', label: 'Events' },
  { value: 'event_breakdown', label: 'Event breakdown' },
  { value: 'tokens', label: 'Tokens' },
  { value: 'cost', label: 'Cost' },
  { value: 'sessions', label: 'Sessions' },
];

// Metric charts can render any of the four kinds; SQL charts plot a series, so
// `stat` is hidden for them (stat reads a scalar off the activity summary).
const METRIC_KINDS: Array<{ value: Chart['kind']; label: string }> = [
  { value: 'line', label: 'Line' },
  { value: 'bar', label: 'Bar' },
  { value: 'pie', label: 'Pie' },
  { value: 'stat', label: 'Stat' },
];
const SQL_KINDS = METRIC_KINDS.filter((k) => k.value !== 'stat');

const FIELD_LABEL = 'mb-1.5 block text-[12.5px] text-[var(--color-text-secondary)]';

// ChartEditor is the create/edit surface for a dashboard chart. It shows a live
// preview built from the same ChartCard used on the board, so what you see while
// editing is exactly what gets saved. Passing `chart` switches it to edit mode.
export function ChartEditor({ chart, onSubmit, onClose }: {
  chart?: Chart;
  onSubmit: (input: ChartInput, chartID?: string) => void;
  onClose: () => void;
}) {
  const projectID = useAuthStore((s) => s.project?.id);
  const { summary } = useActivity();

  const [name, setName] = useState(chart?.name ?? '');
  const [source, setSource] = useState<Source>(chart?.sql ? 'sql' : 'metric');
  const [kind, setKind] = useState<Chart['kind']>(chart?.kind ?? 'line');
  const [metric, setMetric] = useState<Chart['metric']>(chart?.metric ?? 'events');
  const [eventName, setEventName] = useState(chart?.event_name ?? '');
  const [sql, setSql] = useState(chart?.sql ?? '');
  const [xField, setXField] = useState(chart?.x_field ?? '');
  const [yField, setYField] = useState(chart?.y_field ?? '');
  const [colSpan, setColSpan] = useState(chart?.col_span && chart.col_span >= 1 ? chart.col_span : 1);

  // Debounce the SQL feeding the preview so we don't re-run the query on every
  // keystroke — only ~500ms after typing settles.
  const [previewSql, setPreviewSql] = useState(sql);
  useEffect(() => {
    const id = setTimeout(() => setPreviewSql(sql), 500);
    return () => clearTimeout(id);
  }, [sql]);

  const kinds = source === 'sql' ? SQL_KINDS : METRIC_KINDS;
  const effectiveKind = source === 'sql' && kind === 'stat' ? 'line' : kind;

  const input: ChartInput = {
    name: name.trim(),
    kind: effectiveKind,
    metric: source === 'metric' ? metric : 'events',
    event_name: source === 'metric' && metric === 'event_breakdown' ? eventName.trim() : '',
    event_type: '',
    sql: source === 'sql' ? sql.trim() : '',
    x_field: source === 'sql' ? xField.trim() : '',
    y_field: source === 'sql' ? yField.trim() : '',
    col_span: colSpan,
  };

  // The preview chart mirrors the saved shape but uses the debounced SQL.
  const previewChart = useMemo<Chart>(() => ({
    id: chart?.id ?? 'preview',
    dashboard_id: chart?.dashboard_id ?? '',
    project_id: projectID ?? '',
    name: name.trim() || 'Untitled chart',
    kind: effectiveKind,
    metric: input.metric,
    event_name: input.event_name,
    event_type: '',
    sql: source === 'sql' ? previewSql.trim() : '',
    x_field: input.x_field,
    y_field: input.y_field,
    sort_order: chart?.sort_order ?? 0,
    col_span: colSpan,
    created_at: '',
    updated_at: '',
  }), [chart, projectID, name, effectiveKind, input.metric, input.event_name, input.x_field, input.y_field, source, previewSql, colSpan]);

  const canSave = !!name.trim() && (source === 'metric' || !!sql.trim());

  function save() {
    if (!canSave) return;
    onSubmit(input, chart?.id);
    onClose();
  }

  return (
    <Modal
      title={chart ? 'Edit chart' : 'Add chart'}
      onClose={onClose}
      wide
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" onClick={save} disabled={!canSave}>{chart ? 'Save changes' : 'Add chart'}</Button></>}
    >
      <div className="grid grid-cols-[1fr_320px] gap-5 max-[680px]:grid-cols-1">
        {/* Form */}
        <div className="flex flex-col gap-3.5">
          <div>
            <label className={FIELD_LABEL}>Chart name</label>
            <TextInput label="Chart name" isLabelHidden value={name} placeholder="e.g. Daily signups" onChange={(v) => setName(v)} hasAutoFocus width="100%" />
          </div>

          <div>
            <label className={FIELD_LABEL}>Source</label>
            <Segment options={[{ value: 'metric', label: 'Metric' }, { value: 'sql', label: 'SQL query' }]} value={source} onChange={(v) => setSource(v as Source)} />
          </div>

          <div>
            <label className={FIELD_LABEL}>Chart type</label>
            <Segment options={kinds} value={effectiveKind} onChange={(v) => setKind(v as Chart['kind'])} />
          </div>

          <div>
            <label className={FIELD_LABEL}>Width</label>
            <Segment
              options={[{ value: '1', label: '1 column' }, { value: '2', label: '2 columns' }, { value: '3', label: 'Full width' }]}
              value={String(colSpan)}
              onChange={(v) => setColSpan(Number(v))}
            />
          </div>

          {source === 'metric' ? (
            <>
              <div>
                <label className={FIELD_LABEL}>Metric</label>
                <Segment options={METRICS} value={metric} onChange={(v) => setMetric(v as Chart['metric'])} />
              </div>
              {metric === 'event_breakdown' ? (
                <div>
                  <label className={FIELD_LABEL}>Event to break down</label>
                  <EventNameCombobox value={eventName} onChange={setEventName} placeholder="Pick an event…" />
                </div>
              ) : null}
            </>
          ) : (
            <>
              <div>
                <label className={FIELD_LABEL}>SQL query</label>
                {/* Bespoke (not Astryx TextArea): a monospace SQL code field, same
                    exception class as the SQL page's editor — Astryx TextArea can't
                    set a monospace control font through its typed props. */}
                <textarea
                  className="min-h-[120px] w-full resize-y rounded-md border border-[var(--color-border-emphasized)] bg-[var(--color-background-muted)] px-3 py-2 font-mono text-[12px] leading-[1.5] text-[var(--color-text-primary)] outline-0 focus:border-primary focus:shadow-[0_0_0_3px_var(--ring)]"
                  value={sql}
                  placeholder={"select day, count(*) as n\nfrom events\nwhere timestamp >= '{{from}}'\ngroup by day order by day"}
                  onChange={(e) => setSql(e.target.value)}
                  spellCheck={false}
                />
                <p className="mt-1.5 text-[11px] text-[var(--color-text-secondary)]">Use <code className="font-mono">{'{{from}}'}</code>, <code className="font-mono">{'{{to}}'}</code>, or <code className="font-mono">{'{{hours}}'}</code> to honour the dashboard time range.</p>
              </div>
              <div className="grid grid-cols-2 gap-2.5">
                <div>
                  <label className={FIELD_LABEL}>X field <span className="text-[var(--color-text-disabled)]">(optional)</span></label>
                  <TextInput label="X field" isLabelHidden value={xField} placeholder="auto" onChange={(v) => setXField(v)} width="100%" />
                </div>
                <div>
                  <label className={FIELD_LABEL}>Y field <span className="text-[var(--color-text-disabled)]">(optional)</span></label>
                  <TextInput label="Y field" isLabelHidden value={yField} placeholder="auto" onChange={(v) => setYField(v)} width="100%" />
                </div>
              </div>
            </>
          )}
        </div>

        {/* Live preview */}
        <div>
          <label className={FIELD_LABEL}>Preview</label>
          <ChartCard chart={previewChart} summary={summary} projectID={projectID} preview />
        </div>
      </div>
    </Modal>
  );
}
