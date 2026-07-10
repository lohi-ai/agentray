'use client';

import { useEffect, useRef } from 'react';
import * as echarts from 'echarts';

// ChartSpec is the one chart contract in the system. The dashboard builds it
// from a saved query; the agent emits it inline in chat. Either way it renders
// through the same themed ECharts engine, so a graph looks and behaves the same
// everywhere. Keep it small and declarative — engine details stay in buildOption.
export type ChartSpec = {
  type: 'line' | 'area' | 'bar' | 'pie';
  x?: (string | number)[];
  series: Array<{ name?: string; data: number[] }>;
  // pie/donut data: name→value pairs (use instead of series for type 'pie')
  slices?: Array<{ name: string; value: number }>;
  unit?: string;
  stack?: boolean;
  height?: number;
};

// cssVar reads a design token at runtime so charts match the app theme instead
// of hardcoding hex. Falls back to a sane value during SSR / before paint.
function cssVar(name: string, fallback: string): string {
  if (typeof window === 'undefined') return fallback;
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

function palette(): string[] {
  return [
    cssVar('--primary', '#46B7E8'),
    cssVar('--agent', '#8B7CF6'),
    cssVar('--data', '#22C786'),
    cssVar('--warning', '#E8A23C'),
    '#E86F9E', '#5AD1C5',
  ];
}

// buildOption turns a ChartSpec into a themed ECharts option: transparent
// background, token-driven axis/grid/text colors, tooltips, and a soft area
// gradient for line/area. This is the single place chart styling lives.
function buildOption(spec: ChartSpec): echarts.EChartsCoreOption {
  const colors = palette();
  const axisColor = cssVar('--muted-foreground', '#7E8AA0');
  const gridColor = cssVar('--border', '#243044');
  const text = cssVar('--foreground', '#E6EAF2');
  const surface = cssVar('--surface-1', '#0E1420');
  const unitFmt = spec.unit ? ` ${spec.unit}` : '';

  const tooltip = {
    trigger: spec.type === 'pie' ? 'item' : 'axis',
    backgroundColor: surface,
    borderColor: gridColor,
    textStyle: { color: text, fontSize: 12 },
    valueFormatter: (v: number) => `${typeof v === 'number' ? v.toLocaleString() : v}${unitFmt}`,
  } as echarts.EChartsCoreOption['tooltip'];

  if (spec.type === 'pie') {
    return {
      color: colors,
      tooltip,
      legend: { bottom: 0, textStyle: { color: axisColor, fontSize: 11 }, icon: 'circle' },
      series: [{
        type: 'pie', radius: ['52%', '74%'], center: ['50%', '44%'],
        data: spec.slices ?? [], label: { color: text, fontSize: 11 },
        itemStyle: { borderColor: surface, borderWidth: 2 },
      }],
    };
  }

  return {
    color: colors,
    tooltip,
    grid: { left: 8, right: 14, top: 16, bottom: 4, containLabel: true },
    legend: spec.series.length > 1 ? { top: 0, right: 0, textStyle: { color: axisColor, fontSize: 11 }, icon: 'roundRect' } : undefined,
    xAxis: {
      type: 'category', data: spec.x ?? spec.series[0]?.data.map((_, i) => i + 1),
      boundaryGap: spec.type === 'bar',
      axisLine: { lineStyle: { color: gridColor } },
      axisLabel: { color: axisColor, fontSize: 11, hideOverlap: true },
      axisTick: { show: false },
    },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: gridColor, type: 'dashed' } },
      axisLabel: { color: axisColor, fontSize: 11 },
    },
    series: spec.series.map((s, i) => ({
      name: s.name, type: spec.type === 'bar' ? 'bar' : 'line', data: s.data,
      stack: spec.stack ? 'total' : undefined,
      smooth: spec.type !== 'bar', showSymbol: false,
      lineStyle: spec.type !== 'bar' ? { width: 2 } : undefined,
      itemStyle: spec.type === 'bar' ? { borderRadius: [3, 3, 0, 0] } : undefined,
      areaStyle: spec.type === 'area' ? {
        opacity: 0.18,
        color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
          { offset: 0, color: colors[i % colors.length] },
          { offset: 1, color: 'transparent' },
        ]),
      } : undefined,
    })),
  };
}

// Chart is the shared graph component — one ECharts instance, themed, resizing
// with its container, disposed on unmount. dashboard + chat both render this.
export function Chart({ spec }: { spec: ChartSpec }) {
  const ref = useRef<HTMLDivElement>(null);
  const inst = useRef<echarts.ECharts | null>(null);
  const height = spec.height ?? 160;

  useEffect(() => {
    if (!ref.current) return;
    const chart = echarts.init(ref.current, undefined, { renderer: 'canvas' });
    inst.current = chart;
    chart.setOption(buildOption(spec));
    const ro = new ResizeObserver(() => chart.resize());
    ro.observe(ref.current);
    return () => { ro.disconnect(); chart.dispose(); inst.current = null; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(spec)]);

  return <div ref={ref} style={{ width: '100%', height }} />;
}

// Sparkline draws a filled area line from an arbitrary numeric series, scaling to
// fit the 600×180 viewBox. Used by data-bound panels (trends, timelines).
export function Sparkline({ values, color = '#46B7E8', height = 180, fill = true }: { values: number[]; color?: string; height?: number; fill?: boolean }) {
  const w = 600;
  const h = 180;
  if (values.length === 0) return <div className="block w-full grid place-items-center text-[var(--color-text-secondary)] text-xs" style={{ height }}>No data in range</div>;
  const max = Math.max(...values, 1);
  const min = Math.min(...values, 0);
  const span = max - min || 1;
  const step = values.length > 1 ? w / (values.length - 1) : w;
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(h - ((v - min) / span) * (h - 16) - 8).toFixed(1)}`);
  const line = `M${pts.join(' L')}`;
  const gid = `spark-${color.replace('#', '')}`;
  return (
    <svg className="block w-full" style={{ height }} viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none">
      {fill ? (
        <>
          <defs><linearGradient id={gid} x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor={color} stopOpacity="0.32" /><stop offset="100%" stopColor={color} stopOpacity="0" /></linearGradient></defs>
          <path d={`${line} L${w},${h} L0,${h} Z`} fill={`url(#${gid})`} />
        </>
      ) : null}
      <path d={line} fill="none" stroke={color} strokeWidth="2" />
    </svg>
  );
}

export function AreaChart() {
  return <><svg className="block w-full h-[180px]" viewBox="0 0 600 180" preserveAspectRatio="none"><defs><linearGradient id="g1" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stopColor="#46B7E8" stopOpacity="0.32" /><stop offset="100%" stopColor="#46B7E8" stopOpacity="0" /></linearGradient></defs><path d="M0,120 L60,100 L120,108 L180,72 L240,84 L300,52 L360,60 L420,40 L480,56 L540,30 L600,44 L600,180 L0,180 Z" fill="url(#g1)" /><path d="M0,120 L60,100 L120,108 L180,72 L240,84 L300,52 L360,60 L420,40 L480,56 L540,30 L600,44" fill="none" stroke="#46B7E8" strokeWidth="2" /></svg><div className="mt-[10px] text-[var(--color-text-secondary)] text-[11.5px]"><span><i className="inline-block w-[9px] h-[9px] mr-[5px] rounded-[3px] align-[-1px]" style={{ background: 'var(--data)' }} />Sessions</span></div></>;
}

export function RetentionChart() {
  return <svg className="block w-full h-[180px]" viewBox="0 0 600 180" preserveAspectRatio="none"><path d="M0,150 L60,140 L120,120 L180,118 L240,96 L300,90 L360,70 L420,66 L480,58 L540,52 L600,50" fill="none" stroke="#22C786" strokeWidth="2.5" /><path d="M0,168 L60,166 L120,160 L180,158 L240,150 L300,148 L360,140 L420,138 L480,134 L540,130 L600,128" fill="none" stroke="#7E8AA0" strokeWidth="2" strokeDasharray="4 4" /></svg>;
}

export function BarChart() {
  return <svg className="block w-full h-[180px]" viewBox="0 0 600 160" preserveAspectRatio="none"><g fill="#46B7E8"><rect x="20" y="40" width="60" height="100" rx="4" /><rect x="120" y="70" width="60" height="70" rx="4" /><rect x="220" y="90" width="60" height="50" rx="4" /></g><g fill="#22C786"><rect x="320" y="60" width="60" height="80" rx="4" /><rect x="420" y="100" width="60" height="40" rx="4" /><rect x="520" y="110" width="60" height="30" rx="4" /></g></svg>;
}
