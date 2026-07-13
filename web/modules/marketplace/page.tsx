'use client';

import { BarChart3, Database, LayoutTemplate, Megaphone, PieChart, Rocket, Send, ShieldCheck, Sparkles, Store, TrendingUp, Wand2 } from 'lucide-react';
import type { AgentPreset, TemplateChart } from '@/lib/api';
import { useMarketplace, useTemplates } from '@/modules/app/hooks';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Button, EmptyState, Loading } from '@/modules/shared/components/signal-primitives';

// PRESET_ICON maps a preset's icon name to a lucide glyph; Sparkles is the
// fallback so a new preset always renders even before its icon is mapped.
const PRESET_ICON: Record<string, React.ComponentType<{ size?: number }>> = {
  'trending-up': TrendingUp,
  megaphone: Megaphone,
  database: Database,
  rocket: Rocket,
  send: Send,
  'shield-check': ShieldCheck,
};

// ── Template preview helpers ───────────────────────────────────────────────
// Templates carry chart kinds but no live data, so we render representative
// shapes from a stable per-chart seed — enough to show what the dashboard
// feels like before applying it.

function hashStr(str: string): number {
  let h = 0;
  for (let i = 0; i < str.length; i += 1) h = (h * 31 + str.charCodeAt(i)) | 0;
  return Math.abs(h) || 1;
}

function sampleSeries(seed: number, n = 12): number[] {
  let s = seed % 2147483647;
  if (s <= 0) s += 2147483646;
  const rnd = () => (s = (s * 16807) % 2147483647) / 2147483647;
  let v = 28 + rnd() * 34;
  const out: number[] = [];
  for (let i = 0; i < n; i += 1) {
    v += (rnd() - 0.4) * 20;
    out.push(Math.max(6, v));
  }
  return out;
}

function areaPath(values: number[], w: number, h: number): { line: string; fill: string } {
  const max = Math.max(...values, 1);
  const min = Math.min(...values, 0);
  const span = max - min || 1;
  const step = values.length > 1 ? w / (values.length - 1) : w;
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(h - ((v - min) / span) * (h - 6) - 3).toFixed(1)}`);
  const line = `M${pts.join(' L')}`;
  return { line, fill: `${line} L${w},${h} L0,${h} Z` };
}

const MINI_COLORS = ['var(--data)', 'var(--primary)', 'var(--agent)', 'var(--warning)'];

function MiniChart({ chart, index }: { chart: TemplateChart; index: number }) {
  const seed = hashStr(chart.id || chart.name || String(index));
  const color = MINI_COLORS[index % MINI_COLORS.length];
  const w = 120;
  const h = 38;

  let body: React.ReactNode;
  let Icon = TrendingUp;

  if (chart.kind === 'stat') {
    Icon = Sparkles;
    const sample = ['1.2k', '94%', '3.8k', '$420', '12.5k'][seed % 5];
    body = <div className="font-mono tabular-nums text-[20px] font-[650] tracking-[-0.02em] leading-[38px]" style={{ color }}>{sample}</div>;
  } else if (chart.kind === 'pie') {
    Icon = PieChart;
    const a = 30 + (seed % 30);
    const b = a + 22 + (seed % 18);
    body = (
      <div
        className="h-[38px] w-[38px] rounded-full [mask:radial-gradient(circle,transparent_40%,#000_41%)]"
        style={{ background: `conic-gradient(var(--agent) 0 ${a}%, var(--data) ${a}% ${b}%, var(--primary) ${b}% 100%)` }}
      />
    );
  } else if (chart.kind === 'bar') {
    Icon = BarChart3;
    const vals = sampleSeries(seed, 6);
    const max = Math.max(...vals, 1);
    const bw = w / vals.length;
    body = (
      <svg className="block h-[38px] w-full" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden>
        {vals.map((v, i) => {
          const bh = Math.max(3, (v / max) * (h - 4));
          return <rect key={i} x={i * bw + bw * 0.18} y={h - bh} width={bw * 0.64} height={bh} rx={2} fill={color} />;
        })}
      </svg>
    );
  } else {
    const vals = sampleSeries(seed, 12);
    const { line, fill } = areaPath(vals, w, h);
    const gid = `tplg-${seed}`;
    body = (
      <svg className="block h-[38px] w-full" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden>
        <defs>
          <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={color} stopOpacity="0.32" />
            <stop offset="100%" stopColor={color} stopOpacity="0" />
          </linearGradient>
        </defs>
        <path d={fill} fill={`url(#${gid})`} />
        <path d={line} fill="none" stroke={color} strokeWidth="1.6" vectorEffect="non-scaling-stroke" />
      </svg>
    );
  }

  return (
    <div className="flex min-w-0 flex-col gap-[5px]">
      <span className="flex items-center gap-[5px] overflow-hidden text-[10.5px] whitespace-nowrap text-ellipsis text-[var(--color-text-secondary)]"><Icon size={11} /> {chart.name || 'Chart'}</span>
      {body}
    </div>
  );
}

function AgentPresetCard({ preset, installing, onInstall }: { preset: AgentPreset; installing: boolean; onInstall: () => void }) {
  const Icon = PRESET_ICON[preset.icon] ?? Sparkles;
  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-4 flex flex-col gap-[10px] transition-[transform,background,box-shadow] duration-[120ms] ease-[cubic-bezier(0.2,0.8,0.2,1)] hover:-translate-y-0.5 hover:bg-[var(--color-background-muted)] hover:shadow-[0_8px_24px_-14px_rgba(0,0,0,0.75)]">
      <div className="flex items-center gap-[11px]">
        <span className="grid h-[38px] w-[38px] flex-none place-items-center rounded-[11px] bg-[color-mix(in_srgb,var(--agent)_16%,transparent)] text-agent"><Icon size={19} /></span>
        <div>
          <h3 className="m-0 text-sm font-semibold">{preset.name}</h3>
          <span className="text-[11px] capitalize text-[var(--color-text-secondary)]">{preset.category}</span>
        </div>
      </div>
      <p className="m-0 text-[13px] leading-[1.45] font-medium text-[var(--color-text-primary)]">{preset.tagline}</p>
      <p className="m-0 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{preset.description}</p>
      {preset.skills.length ? (
        <div className="flex flex-wrap gap-[6px]">
          {preset.skills.map((s) => (
            <span
              key={s.name}
              title={s.description}
              className="inline-flex items-center gap-1 rounded-[20px] px-[9px] py-[3px] text-[11px] font-medium bg-[color-mix(in_srgb,var(--agent)_12%,var(--surface-3))] text-[color-mix(in_srgb,var(--agent)_55%,var(--foreground))]"
            >
              <Wand2 size={11} /> {s.name}
            </span>
          ))}
        </div>
      ) : null}
      <div className="mt-auto pt-[6px]">
        <Button variant="primary" size="sm" icon={<Sparkles size={15} />} disabled={installing} onClick={onInstall}>
          {installing ? 'Installing…' : 'Install agent'}
        </Button>
      </div>
    </div>
  );
}

function TemplateCard({ name, isSystem, description, charts, onApply }: { name: string; isSystem: boolean; description: string; charts: TemplateChart[]; onApply: () => void }) {
  const preview = [...charts].sort((a, b) => a.sort_order - b.sort_order).slice(0, 4);
  return (
    <div className="rounded-xl bg-[var(--color-background-card)] p-4 group flex flex-col gap-[10px] transition-[transform,background,box-shadow] duration-[120ms] ease-[cubic-bezier(0.2,0.8,0.2,1)] hover:-translate-y-0.5 hover:bg-[var(--color-background-muted)] hover:shadow-[0_8px_24px_-14px_rgba(0,0,0,0.75)]">
      <div className="flex items-center mb-3">
        <h3 className="m-0 text-[13px] font-semibold">{name}</h3>
        {isSystem ? <span className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)] ms-auto">system</span> : null}
      </div>
      {description ? <p className="m-0 text-[12.5px] leading-[1.5] text-[var(--color-text-secondary)]">{description}</p> : null}
      {preview.length ? (
        <div className="grid grid-cols-2 gap-2 rounded-lg bg-[var(--color-background-muted)] p-[10px] group-hover:bg-[color-mix(in_srgb,var(--surface-1)_70%,var(--background))]">
          {preview.map((c, i) => <MiniChart key={c.id || i} chart={c} index={i} />)}
        </div>
      ) : null}
      <div className="mt-auto flex items-center justify-between pt-1">
        <span className="inline-flex items-center gap-[6px] text-[11.5px] text-[var(--color-text-secondary)]"><LayoutTemplate size={13} /> {charts.length} chart{charts.length === 1 ? '' : 's'}</span>
        <Button variant="outline" size="sm" onClick={onApply}>Use template</Button>
      </div>
    </div>
  );
}

export function MarketplacePage() {
  const { presets, loading, installing, installAgent } = useMarketplace();
  const { templates, applyTemplate } = useTemplates();

  return (
    <AppShell active="dashboards">
      <div
        className="relative mb-2 overflow-hidden rounded-xl p-[22px] pb-6 bg-[var(--color-background-card)]"
        style={{
          backgroundImage:
            'radial-gradient(120% 140% at 0% 0%, color-mix(in srgb, var(--agent) 18%, transparent), transparent 60%), radial-gradient(120% 140% at 100% 0%, color-mix(in srgb, var(--primary) 16%, transparent), transparent 58%)',
        }}
      >
        <span className="mb-3 inline-flex items-center gap-[7px] rounded-[20px] px-[10px] py-1 text-[11.5px] font-semibold bg-[color-mix(in_srgb,var(--agent)_16%,transparent)] text-agent"><Store size={13} /> Marketplace</span>
        <h1 className="m-0 text-[22px] font-[650] tracking-[-0.02em]">Productive in one click</h1>
        <p className="mt-[6px] mb-0 max-w-[560px] text-[13px] leading-[1.55] text-[var(--color-text-secondary)]">Hire a ready-made agent or start a dashboard from a template — preview the graphs, then drop it straight into your workspace.</p>
      </div>

      <div className="mt-[26px] mb-3 flex items-baseline gap-[9px]">
        <h2 className="m-0 text-sm font-[650]">Foundation agents</h2>
        <span className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)] text-[11px]">{presets.length || ''}</span>
      </div>
      {loading && presets.length === 0 ? (
        <Loading label="Loading agents…" />
      ) : presets.length === 0 ? (
        <EmptyState icon={<Sparkles size={22} />} title="No agents available" detail="Foundation agents will appear here." />
      ) : (
        <div className="grid grid-cols-3 gap-3.5 max-[980px]:grid-cols-1">
          {presets.map((p) => (
            <AgentPresetCard key={p.slug} preset={p} installing={installing} onInstall={() => void installAgent(p.slug)} />
          ))}
        </div>
      )}

      <div className="mt-[26px] mb-3 flex items-baseline gap-[9px]">
        <h2 className="m-0 text-sm font-[650]">Dashboard templates</h2>
        <span className="text-[11px] font-medium uppercase tracking-[0.08em] text-[var(--color-text-secondary)] text-[11px]">{templates.length || ''}</span>
      </div>
      {templates.length === 0 ? (
        <EmptyState icon={<LayoutTemplate size={22} />} title="No templates available" detail="System templates will appear here once published." />
      ) : (
        <div className="grid grid-cols-3 gap-3.5 max-[980px]:grid-cols-1">
          {templates.map((t) => (
            <TemplateCard
              key={t.id}
              name={t.name}
              isSystem={t.is_system}
              description={t.description}
              charts={t.charts}
              onApply={() => void applyTemplate(t.id)}
            />
          ))}
        </div>
      )}
    </AppShell>
  );
}
