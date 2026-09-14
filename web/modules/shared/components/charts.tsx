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
  // Set false for a series that counts discrete things (people, sessions, runs) —
  // a smoothed curve visually implies fractional values ("1.5 people") that don't
  // exist. Defaults to true for line/area, matching prior behavior.
  smooth?: boolean;
  // Force whole-number y-axis ticks. Pair with smooth: false for count series —
  // otherwise ECharts' auto interval can still land on 0.5 steps.
  integerY?: boolean;
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

// Timeline x-axes arrive as raw ISO-8601 UTC strings ("2026-08-18T10:00:00Z")
// from the API. Rendering that string verbatim on an axis tick is unreadable
// and the leftmost tick gets clipped by the chart's own width. Format as local
// time appropriate to the range instead — one place, shared by every ECharts
// surface (dashboard, chat, product insights, persons) since they all build
// their option through this module.
const ISO_LIKE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;
const DAY_MS = 24 * 60 * 60 * 1000;

function formatAxisTick(value: string | number, series?: (string | number)[]): string {
  if (typeof value !== 'string' || !ISO_LIKE.test(value)) return String(value);
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  const xs = series ?? [];
  const first = xs[0];
  const last = xs[xs.length - 1];
  const spanMs = typeof first === 'string' && typeof last === 'string'
    ? Math.abs(new Date(last).getTime() - new Date(first).getTime())
    : 0;
  return spanMs > DAY_MS
    ? d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });
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
    textStyle: { color: text, fontSize: 13 },
    valueFormatter: (v: number) => `${typeof v === 'number' ? v.toLocaleString() : v}${unitFmt}`,
  } as echarts.EChartsCoreOption['tooltip'];

  if (spec.type === 'pie') {
    return {
      color: colors,
      tooltip,
      legend: { bottom: 0, textStyle: { color: axisColor, fontSize: 12 }, icon: 'circle' },
      series: [{
        type: 'pie', radius: ['52%', '74%'], center: ['50%', '44%'],
        data: spec.slices ?? [], label: { color: text, fontSize: 12 },
        itemStyle: { borderColor: surface, borderWidth: 2 },
      }],
    };
  }

  return {
    color: colors,
    tooltip,
    grid: { left: 8, right: 14, top: 16, bottom: 4, containLabel: true },
    legend: spec.series.length > 1 ? { top: 0, right: 0, textStyle: { color: axisColor, fontSize: 12 }, icon: 'roundRect' } : undefined,
    xAxis: {
      type: 'category', data: spec.x ?? spec.series[0]?.data.map((_, i) => i + 1),
      boundaryGap: spec.type === 'bar',
      axisLine: { lineStyle: { color: gridColor } },
      axisLabel: {
        color: axisColor, fontSize: 12, hideOverlap: true,
        formatter: (value: string) => formatAxisTick(value, spec.x),
      },
      axisTick: { show: false },
    },
    yAxis: {
      type: 'value',
      min: spec.integerY ? 0 : undefined,
      minInterval: spec.integerY ? 1 : undefined,
      splitLine: { lineStyle: { color: gridColor, type: 'dashed' } },
      axisLabel: {
        color: axisColor, fontSize: 12,
        formatter: spec.integerY ? (v: number) => String(Math.round(v)) : undefined,
      },
    },
    series: spec.series.map((s, i) => ({
      name: s.name, type: spec.type === 'bar' ? 'bar' : 'line', data: s.data,
      stack: spec.stack ? 'total' : undefined,
      smooth: spec.smooth ?? (spec.type !== 'bar'), showSymbol: false,
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

