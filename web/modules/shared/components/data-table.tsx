'use client';

import { useMemo, useState, type ReactNode } from 'react';
import {
  Table,
  useTableSortable,
  useTableSortableState,
  useTableColumnSettings,
  type TableColumn,
  type TablePlugin,
  type TableSortState,
  type BodyRowRenderProps,
} from '@astryxdesign/core/Table';
import { Pagination } from '@astryxdesign/core/Pagination';
import { Card } from '@astryxdesign/core/Card';
import { TextInput } from '@astryxdesign/core/TextInput';
import { MultiSelector } from '@astryxdesign/core/MultiSelector';
import { Search } from 'lucide-react';

// DataColumn is Astryx's data-driven TableColumn plus the bits a page-level
// table needs that the design-system column doesn't carry: a label for the
// column-visibility menu, a flag to opt a column out of hiding, and pure
// accessors that drive global search and native sort for computed/rendered
// cells (where the raw `item[key]` isn't the value the user sees).
export interface DataColumn<T extends Record<string, unknown>> extends TableColumn<T> {
  /** Label shown in the column-visibility menu. Defaults to a string header, else the key. */
  menuLabel?: string;
  /** Whether the column can be hidden from the visibility menu. @default true */
  hideable?: boolean;
  /** Value used for the global search filter. Defaults to String(item[key]). */
  searchValue?: (item: T) => string;
  /** Value the native sort compares. Defaults to item[key]; set it for computed/rendered cells. */
  sortValue?: (item: T) => string | number;
}

// DataTable is the one client-side table surface in the app, built on Astryx's
// native data-driven <Table>: pages hand it `columns` (key/header/width/align/
// renderCell) + already-loaded rows and it owns search, native sort, column
// visibility, and pagination through Astryx's own plugins — no TanStack, no
// hand-rolled sort headers. It renders flush with an optional title/action
// header so it drops straight into a page or a Panel.
interface DataTableProps<T extends Record<string, unknown>> {
  columns: DataColumn<T>[];
  data: T[];
  /** Stable row identity for React keys. Defaults to row index. */
  idKey?: (keyof T & string) | ((item: T) => string | number);
  title?: ReactNode;
  action?: ReactNode;
  /** Show a global search box with this placeholder. */
  searchPlaceholder?: string;
  /** Initial rows per page. */
  pageSize?: number;
  pageSizeOptions?: number[];
  /** Initial sort, e.g. [{ sortKey: 'timestamp', direction: 'descending' }]. */
  defaultSort?: TableSortState;
  onRowClick?: (row: T) => void;
  /** Row className resolver — e.g. error rows in red. */
  rowClassName?: (row: T) => string | undefined;
  emptyMessage?: string;
}

// Numbers sort numerically; everything else uses a locale-aware numeric-aware
// string compare so "Step 2" lands before "Step 10".
function compareValues(a: string | number, b: string | number): number {
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true });
}

export function DataTable<T extends Record<string, unknown>>({
  columns,
  data,
  idKey,
  title,
  action,
  searchPlaceholder,
  pageSize: initialPageSize = 10,
  pageSizeOptions = [10, 20, 30, 50],
  defaultSort = [],
  onRowClick,
  rowClassName,
  emptyMessage = 'No results.',
}: DataTableProps<T>) {
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(initialPageSize);
  // Active column keys drive the column-settings plugin; always-visible columns
  // (hideable === false) stay pinned in the set regardless of the menu.
  const [activeKeys, setActiveKeys] = useState<string[]>(() => columns.map((c) => c.key));

  // Make every column sortable by default — that's what surfaces Astryx's native
  // sort affordance — unless a column opts out. Columns keep their own align/width.
  const tableColumns = useMemo<TableColumn<T>[]>(
    () => columns.map((c) => ({ ...c, sortable: c.sortable ?? true })),
    [columns],
  );

  // Custom comparators for columns whose sort value is computed/rendered rather
  // than the raw cell — keyed by column key (which doubles as the sort key).
  const comparators = useMemo(() => {
    const map: Record<string, (a: T, b: T) => number> = {};
    for (const c of columns) {
      if (c.sortValue) map[c.key] = (a, b) => compareValues(c.sortValue!(a), c.sortValue!(b));
    }
    return map;
  }, [columns]);

  // Global search: match the raw search string of any column, case-insensitive.
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return data;
    return data.filter((item) =>
      columns.some((c) => {
        const v = c.searchValue ? c.searchValue(item) : item[c.key];
        return v != null && String(v).toLowerCase().includes(q);
      }),
    );
  }, [data, columns, search]);

  const { sortedData, sortConfig } = useTableSortableState<T>({
    data: filtered,
    defaultSort,
    comparators,
  });
  const sortPlugin = useTableSortable<T>(sortConfig);

  const hideable = useMemo(() => columns.filter((c) => c.hideable !== false), [columns]);
  const hideableKeys = useMemo(() => new Set(hideable.map((c) => c.key)), [hideable]);
  const columnSettingsPlugin = useTableColumnSettings<T>({
    columns: columns.map((c) => ({
      key: c.key,
      label: c.menuLabel ?? (typeof c.header === 'string' ? c.header : c.key),
      isAlwaysVisible: c.hideable === false,
    })),
    activeColumnKeys: activeKeys,
    onChangeActiveColumnKeys: (keys) => setActiveKeys([...keys]),
  });

  // Row navigation / styling lives in a tiny plugin so it composes with sort and
  // column-settings rather than fighting the data-driven renderer.
  const rowNavPlugin = useMemo<TablePlugin<T>>(
    () => ({
      transformBodyRow(props: BodyRowRenderProps, item: T): BodyRowRenderProps {
        if (!onRowClick && !rowClassName) return props;
        const extra = rowClassName?.(item);
        return {
          ...props,
          htmlProps: {
            ...props.htmlProps,
            ...(onRowClick ? { onClick: () => onRowClick(item), style: { ...props.htmlProps.style, cursor: 'pointer' } } : {}),
            className: [props.htmlProps.className, extra].filter(Boolean).join(' ') || undefined,
          },
        };
      },
    }),
    [onRowClick, rowClassName],
  );

  // Clamp the page when the row count shrinks (filtering, page-size bump).
  const total = sortedData.length;
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const safePage = Math.min(page, pageCount);
  const pageData = useMemo(
    () => sortedData.slice((safePage - 1) * pageSize, safePage * pageSize),
    [sortedData, safePage, pageSize],
  );

  const hasHeader = title || action || searchPlaceholder || hideable.length > 0;

  return (
    // Astryx sets the data-driven header cell to 0 top / 9px bottom padding,
    // which jams the labels against the card edge. The Table doesn't forward a
    // className to <table>, so even out the header's vertical padding from the
    // wrapper (`!` beats StyleX's atomic classes) — header now matches body rhythm.
    <div className="flex w-full flex-col gap-3 [&_thead_th]:!pt-2.5 [&_thead_th]:!pb-2.5">

      {hasHeader ? (
        <div className="flex flex-wrap items-center gap-2.5">
          {title ? <h3 className="text-[13.5px] font-semibold text-[var(--color-text-primary)]">{title}</h3> : null}
          <div className="ml-auto flex items-center gap-2">
            {searchPlaceholder ? (
              <div className="w-48">
                <TextInput
                  label={searchPlaceholder}
                  isLabelHidden
                  size="sm"
                  startIcon={Search}
                  value={search}
                  onChange={(v) => { setSearch(v); setPage(1); }}
                  placeholder={searchPlaceholder}
                />
              </div>
            ) : null}
            {hideable.length > 0 ? (
              <MultiSelector
                label="Toggle columns"
                isLabelHidden
                size="sm"
                placeholder="View"
                triggerDisplay="count"
                hasSearch={hideable.length > 12}
                searchPlaceholder="Search columns…"
                value={activeKeys.filter((k) => hideableKeys.has(k))}
                onChange={(next) => {
                  const sel = new Set(next);
                  setActiveKeys(columns.filter((c) => c.hideable === false || sel.has(c.key)).map((c) => c.key));
                }}
                options={hideable.map((c) => ({ value: c.key, label: c.menuLabel ?? (typeof c.header === 'string' ? c.header : c.key) }))}
              />
            ) : null}
            {action}
          </div>
        </div>
      ) : null}

      {/* The Card padding absorbs the Astryx Table's container-bleed (negative
          row margins) so cell content aligns to the border edge — a zero-padding
          box clips it. */}
      <Card padding={4}>
        <Table
          data={pageData}
          columns={tableColumns}
          idKey={idKey}
          density="compact"
          hasHover={!!onRowClick}
          plugins={{ sort: sortPlugin, columnSettings: columnSettingsPlugin, rowNav: rowNavPlugin }}
          emptyState={<div className="py-6 text-center text-[var(--color-text-secondary)]">{emptyMessage}</div>}
        />
      </Card>

      {total > pageSize ? (
        <Pagination
          page={safePage}
          onChange={setPage}
          totalItems={total}
          pageSize={pageSize}
          pageSizeOptions={pageSizeOptions}
          onPageSizeChange={(s) => { setPageSize(s); setPage(1); }}
          variant="count"
        />
      ) : null}
    </div>
  );
}
