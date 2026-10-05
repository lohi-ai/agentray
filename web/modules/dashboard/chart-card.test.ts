// @ts-nocheck -- This intentionally supplies a tiny hook runtime so the test
// can drive SqlGraph's request lifecycle without adding a DOM test dependency.
import fs from 'node:fs';
import { describe, expect, it } from 'vitest';
import ts from 'typescript';

function compile(path: string): string {
  return ts.transpileModule(fs.readFileSync(path, 'utf8'), {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      jsx: ts.JsxEmit.ReactJSX,
    },
  }).outputText;
}

describe('SqlGraph', () => {
  it('shows loading instead of a completed result while a range or projection request is pending', async () => {
    const chartQuery = {};
    new Function('exports', compile('modules/dashboard/chart-query.ts'))(chartQuery);

    let slots = [];
    let cursor = 0;
    let effects = [];
    const calls = [];
    let filters = { from: '2026-09-01', to: '2026-09-02', hours: 24 };
    const pending = [];
    const equal = (a, b) => a && b && a.length === b.length && a.every((value, index) => value === b[index]);
    const react = {
      useMemo(fn, deps) {
        const index = cursor++;
        if (!slots[index] || !equal(slots[index].deps, deps)) slots[index] = { deps, value: fn() };
        return slots[index].value;
      },
      useState(initial) {
        const index = cursor++;
        if (!slots[index]) slots[index] = { value: initial };
        return [slots[index].value, (value) => { slots[index].value = value; }];
      },
      useEffect(fn, deps) {
        const index = cursor++;
        if (!slots[index] || !equal(slots[index].deps, deps)) {
          slots[index]?.cleanup?.();
          slots[index] = { deps };
          effects.push(() => { slots[index].cleanup = fn(); });
        }
      },
    };
    class AgentRayAPI {
      runSQL(sql) {
        calls.push(sql);
        return new Promise((resolve, reject) => pending.push({ resolve, reject }));
      }
    }
    const jsx = (type, props) => ({ type, props });
    function requireForGraph(name) {
      if (name === 'react') return react;
      if (name === 'react/jsx-runtime') return { jsx, jsxs: jsx };
      if (name === './chart-query') return chartQuery;
      if (name === '@/lib/api') return { AgentRayAPI, APIError: class extends Error {} };
      if (name === '@/lib/app-state') return { useFiltersStore: (selector) => selector({ appliedFilters: filters }) };
      if (name === '@/lib/format') return { formatCompact: String, formatCost: String };
      if (name === '@/modules/app/hooks/media') return { useMediaQuery: () => false };
      if (name === './evidence-panel') return { evidenceFilterKey: (value) => JSON.stringify([value.from || '', value.to || '', value.hours]) };
      if (name === 'next/navigation') return { useRouter: () => ({}) };
      return new Proxy({}, { get: (_, key) => key });
    }
    const chartModule = {};
    new Function('require', 'exports', `${compile('modules/dashboard/chart-card.tsx')}\nexports.SqlGraph = SqlGraph;`)(requireForGraph, chartModule);

    expect(chartModule.queryEvidenceFacts([
      { value: 1, unit: null },
      { value: 2, unit: 'VND' },
    ])).toMatchObject({ unit: 'VND' });
    expect(chartModule.queryEvidenceFacts([
      { value: 1, unit: 'VND' },
      { value: 2, unit: 'people' },
    ]).unit).toBeUndefined();
    expect(chartModule.queryEvidenceFacts([
      { value: 1, UNIT: 'VND', Metric_Definition: 'Gross topups' },
    ])).toMatchObject({ unit: 'VND', definition: 'Gross topups' });

    let chart = { sql: '', kind: 'line' };
    function flatten(node) {
      if (node == null || node === false) return '';
      if (Array.isArray(node)) return node.map(flatten).join(' ');
      if (typeof node !== 'object') return String(node);
      if (typeof node.type === 'function') return flatten(node.type(node.props));
      return flatten(node.props?.children);
    }
    function render() {
      cursor = 0;
      const tree = chartModule.SqlGraph({ chart, projectID: 'project-1' });
      const text = flatten(tree);
      for (const effect of effects.splice(0)) effect();
      return text;
    }
    async function settle(rows) {
      pending.shift().resolve({ rows });
      await Promise.resolve();
      await Promise.resolve();
      return render();
    }

    chart = {
      sql: "SELECT count(*) AS count, sum(cost) AS cost FROM events WHERE timestamp >= '{{from}}' AND timestamp < '{{to}}'",
      kind: 'line',
      y_field: 'count',
    };
    expect(render()).toMatch(/Running query/);
    expect(await settle([{ count: 10, cost: 200 }])).toMatch(/latest\s+10/);

    filters = { from: '2026-09-03', to: '2026-09-04', hours: 24 };
    expect(render()).toMatch(/Running query/);
    filters = { from: '2026-09-01', to: '2026-09-02', hours: 24 };
    expect(render()).toMatch(/Running query/);
    expect(calls).toHaveLength(3);

    pending.shift().resolve({ rows: [{ count: 99, cost: 999 }] });
    await Promise.resolve();
    await Promise.resolve();
    expect(render()).toMatch(/Running query/);
    expect(await settle([{ count: 20, cost: 400 }])).toMatch(/latest\s+20/);

    chart = { ...chart, y_field: 'cost' };
    expect(render()).toMatch(/Running query/);
    expect(calls).toHaveLength(4);
    expect(await settle([{ count: 20, cost: 400 }])).toMatch(/latest\s+400/);

    for (const slot of slots) slot?.cleanup?.();
    expect(pending).toHaveLength(0);
  });
});
