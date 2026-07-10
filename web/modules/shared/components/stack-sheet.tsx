'use client';

import {
  createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState,
  type ReactNode,
} from 'react';
import { createPortal } from 'react-dom';
import { ChevronLeft, ChevronRight, Maximize, Maximize2, X } from 'lucide-react';
import { Card } from '@astryxdesign/core/Card';
import { IconButton } from '@astryxdesign/core/IconButton';
import { ToggleButton } from '@astryxdesign/core/ToggleButton';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';

// StackSheet — agentray port of the lohi-ui desktop StackSheet
// (web/lib/lohi-ui/src/components/desktop/stack-sheet.tsx). Same model: panels
// open as floating cards pinned to the right edge; opening another pushes it onto
// the stack, panels that don't fit collapse to a clickable strip, and each panel
// has width steps (default → wide → ultra). The mobile FloatingSheet branch is
// dropped — agentray is a desktop analytics surface — so this carries only the
// desktop portal. The chrome is genuine Astryx: the panel surface is a `Card`,
// header controls are `IconButton`/`ToggleButton` (the width-step toggles carry a
// real pressed state), so it inherits the design system's hover/focus/disabled
// states and tokens — no hand-rolled buttons, and no `hover:bg-accent` (which on
// this bridge resolves to brand green). Only the inactive strip stays bespoke:
// it's a vertical-text affordance Astryx has no primitive for.

// ── Layout constants ──────────────────────────────────────────────────────────

const STRIP_W = 44;   // width of an inactive (minimized) panel strip
const PANEL_GAP = 8;  // gap between panels (gap-2 = 8px)
const EDGE_PAD = 32;  // total horizontal margin: right-4 (16) + left visual buffer (16)

// ── Types ─────────────────────────────────────────────────────────────────────

export interface StackSheetPanel {
  id: string;
  title: ReactNode;
  content: ReactNode;
  /** Panel card width in px (desktop). Default 460. */
  width?: number;
  /** Extra content in the panel header, right of the title. */
  extra?: ReactNode;
  /**
   * When set, this panel is a child of `parentId`. Closing the parent
   * (via closeById) automatically closes this panel and all its own children.
   */
  parentId?: string;
}

interface StackSheetEntry extends StackSheetPanel {
  closing: boolean;
  /** 0 = default, 1 = wide (+280 px, max 1000), 2 = ultra (viewport − edge padding). */
  widthStep: 0 | 1 | 2;
}

interface StackSheetCtx {
  panels: StackSheetEntry[];
  push: (p: StackSheetPanel) => void;
  pop: () => void;
  close: (fromIndex: number) => void;
  closeById: (id: string) => void;
  clear: () => void;
}

const StackSheetContext = createContext<StackSheetCtx | null>(null);

export function useStackSheet() {
  const ctx = useContext(StackSheetContext);
  if (!ctx) throw new Error('useStackSheet used outside StackSheetProvider');
  return ctx;
}

// ── Width resolution ──────────────────────────────────────────────────────────

/** @param n total panels in the stack — used by ultra mode to subtract sibling strips. */
function resolveWidth(entry: StackSheetEntry, n = 1): number {
  const base = entry.width ?? 460;
  if (entry.widthStep === 1) return Math.min(base + 280, 1000);
  if (entry.widthStep === 2) {
    const vw = typeof window !== 'undefined' ? window.innerWidth : 1440;
    const stripsWidth = (n - 1) * (STRIP_W + PANEL_GAP);
    return Math.max(vw - EDGE_PAD - stripsWidth, base);
  }
  return base;
}

// ── Tree helpers ──────────────────────────────────────────────────────────────

/** Collect the panel id and all transitive children (via parentId). */
function collectDescendants(id: string, panels: StackSheetEntry[]): Set<string> {
  const result = new Set<string>();
  const visit = (targetId: string) => {
    result.add(targetId);
    panels.forEach((p) => {
      if (p.parentId === targetId && !result.has(p.id)) visit(p.id);
    });
  };
  visit(id);
  return result;
}

// ── Active-set computation ────────────────────────────────────────────────────
//
// Latest panel (highest index) is always active. Remaining width budget is spent
// expanding panels newest → oldest; panels that don't fit render as strips.

function computeActiveIds(panels: StackSheetEntry[], availableWidth: number): Set<string> {
  const n = panels.length;
  if (n === 0) return new Set();

  const baseWidth = n * STRIP_W + Math.max(0, n - 1) * PANEL_GAP;
  let budget = availableWidth - baseWidth;

  const active = new Set<string>();
  const latest = panels[n - 1];
  active.add(latest.id);
  budget -= resolveWidth(latest, n) - STRIP_W;
  if (budget < 0) return active;

  for (let i = n - 2; i >= 0; i--) {
    const cost = resolveWidth(panels[i], n) - STRIP_W;
    if (budget >= cost) {
      active.add(panels[i].id);
      budget -= cost;
    }
  }
  return active;
}

// ── Shared panel header ───────────────────────────────────────────────────────

function PanelHeader({
  title, extra, onClose, onBack, widthStep, onSetWidthStep, className,
}: {
  title: ReactNode;
  extra?: ReactNode;
  onClose: () => void;
  onBack?: () => void;
  widthStep?: 0 | 1 | 2;
  onSetWidthStep?: (step: 0 | 1 | 2) => void;
  className?: string;
}) {
  return (
    <HStack
      align="center"
      gap={1}
      className={`shrink-0 border-b border-[var(--color-border)] px-3 py-2.5 ${className ?? ''}`}
    >
      {onBack && (
        <IconButton variant="ghost" size="sm" label="Back" tooltip="Back" icon={<ChevronLeft size={16} />} onClick={onBack} />
      )}
      <Text weight="semibold" className="min-w-0 flex-1 truncate text-[15px] text-[var(--color-text-primary)]">{title}</Text>
      {extra && <div className="flex shrink-0 items-center gap-2">{extra}</div>}
      {onSetWidthStep && (
        <div className="flex shrink-0 items-center">
          <ToggleButton
            size="sm"
            label="Widen"
            icon={<Maximize2 size={14} />}
            isPressed={widthStep === 1}
            onPressedChange={(v) => onSetWidthStep(v ? 1 : 0)}
          />
          <ToggleButton
            size="sm"
            label="Full width"
            icon={<Maximize size={14} />}
            isPressed={widthStep === 2}
            onPressedChange={(v) => onSetWidthStep(v ? 2 : 0)}
          />
        </div>
      )}
      <IconButton variant="ghost" size="sm" label="Close" tooltip="Close" icon={<X size={16} />} onClick={onClose} />
    </HStack>
  );
}

// ── Unified panel card ────────────────────────────────────────────────────────
//
// One component for both active (full-width) and inactive (strip) states. Width
// transitions smoothly; strip/full views crossfade via opacity. Content stays
// mounted so state is preserved during resize.

function PanelCard({
  entry, isActive, isMain, onClose, onStripClick, onSetWidthStep, totalPanels,
}: {
  entry: StackSheetEntry;
  isActive: boolean;
  isMain: boolean;
  onClose: () => void;
  onStripClick: () => void;
  onSetWidthStep: (step: 0 | 1 | 2) => void;
  totalPanels: number;
}) {
  const width = isActive ? resolveWidth(entry, totalPanels) : STRIP_W;
  const skipTransition = entry.closing;

  return (
    <Card
      padding={0}
      className="relative h-full shrink-0 overflow-hidden"
      style={{
        width: `${width}px`,
        boxShadow: isMain
          ? '0 8px 32px -4px rgb(0 0 0 / 0.4), 0 2px 8px -2px rgb(0 0 0 / 0.3)'
          : '0 4px 16px -4px rgb(0 0 0 / 0.3), 0 1px 4px -1px rgb(0 0 0 / 0.2)',
        transition: skipTransition ? 'none' : 'width 280ms cubic-bezier(0.32, 0.72, 0, 1), box-shadow 280ms ease',
        animation: entry.closing
          ? 'stack-sheet-exit 200ms cubic-bezier(0.4, 0, 1, 1) both'
          : 'stack-sheet-enter 240ms cubic-bezier(0.32, 0.72, 0, 1) both',
      }}
    >
      {/* ── Strip overlay (shown when inactive) ── */}
      <button
        type="button"
        onClick={onStripClick}
        title={typeof entry.title === 'string' ? entry.title : undefined}
        aria-hidden={isActive}
        tabIndex={isActive ? -1 : 0}
        className="group absolute inset-0 flex cursor-pointer flex-col items-center justify-between py-3"
        style={{
          opacity: isActive ? 0 : 1,
          pointerEvents: isActive ? 'none' : 'auto',
          transition: skipTransition ? 'none' : isActive ? 'opacity 120ms ease' : 'opacity 180ms ease 100ms',
        }}
      >
        <ChevronRight className="w-3.5 h-3.5 shrink-0 text-[var(--color-text-disabled)] transition-colors group-hover:text-[var(--color-text-secondary)]" />
        <div className="flex min-h-0 flex-1 items-center justify-center overflow-hidden py-2">
          {typeof entry.title === 'string' && (
            <span
              className="select-none text-[11px] font-medium text-[var(--color-text-disabled)] transition-colors group-hover:text-[var(--color-text-secondary)]"
              style={{
                writingMode: 'vertical-rl',
                textOrientation: 'mixed',
                transform: 'rotate(180deg)',
                overflow: 'hidden',
                whiteSpace: 'nowrap',
                maxHeight: '140px',
              }}
            >
              {entry.title}
            </span>
          )}
        </div>
        <div className="h-3.5 w-3.5 shrink-0" />
      </button>

      {/* ── Full panel content (shown when active) ── */}
      <div
        className="absolute inset-0 flex flex-col"
        style={{
          opacity: isActive ? 1 : 0,
          pointerEvents: isActive ? 'auto' : 'none',
          transition: skipTransition ? 'none' : isActive ? 'opacity 200ms ease 80ms' : 'opacity 100ms ease',
        }}
      >
        <PanelHeader
          title={entry.title}
          extra={entry.extra}
          onClose={onClose}
          widthStep={entry.widthStep}
          onSetWidthStep={onSetWidthStep}
        />
        <div className="flex-1 overflow-y-auto">{entry.content}</div>
      </div>
    </Card>
  );
}

// ── StackSheetProvider ────────────────────────────────────────────────────────

export function StackSheetProvider({ children }: { children: ReactNode }) {
  const [panels, setPanels] = useState<StackSheetEntry[]>([]);

  const [windowWidth, setWindowWidth] = useState(() =>
    typeof window !== 'undefined' ? window.innerWidth : 1440,
  );
  useEffect(() => {
    const onResize = () => setWindowWidth(window.innerWidth);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);

  const activeIds = useMemo(
    () => computeActiveIds(panels, windowWidth - EDGE_PAD),
    [panels, windowWidth],
  );

  const push = useCallback((panel: StackSheetPanel) => {
    setPanels((prev) => {
      const idx = prev.findIndex((p) => p.id === panel.id);
      // Re-pushing an open id replaces its content IN PLACE — keep its width step
      // and its siblings. This is what lets a caller refresh a live panel's content
      // (new threads/runs) on every data tick without resetting the user's width
      // choice or closing the other panel. (The old truncate-on-replace assumed a
      // drill-down stack; closeById + parentId already handle cascade close.)
      if (idx >= 0) return prev.map((p, i) => (i === idx ? { ...panel, closing: false, widthStep: p.widthStep } : p));
      return [...prev, { ...panel, closing: false, widthStep: 0 }];
    });
  }, []);

  const setWidthStep = useCallback((id: string, step: 0 | 1 | 2) => {
    setPanels((prev) => prev.map((p) => (p.id === id ? { ...p, widthStep: step } : p)));
  }, []);

  const close = useCallback((fromIndex: number) => {
    setPanels((prev) => prev.map((p, i) => (i >= fromIndex ? { ...p, closing: true } : p)));
    setTimeout(() => setPanels((prev) => prev.filter((_, i) => i < fromIndex)), 220);
  }, []);

  const closeById = useCallback((id: string) => {
    setPanels((prev) => {
      if (!prev.some((p) => p.id === id)) return prev;
      const toClose = collectDescendants(id, prev);
      return prev.map((p) => (toClose.has(p.id) ? { ...p, closing: true } : p));
    });
    setTimeout(() => setPanels((prev) => prev.filter((p) => !p.closing)), 220);
  }, []);

  const pop = useCallback(() => {
    setPanels((prev) => (prev.length ? prev.map((p, i) => (i === prev.length - 1 ? { ...p, closing: true } : p)) : prev));
    setTimeout(() => setPanels((prev) => prev.slice(0, -1)), 220);
  }, []);

  const clear = useCallback(() => {
    setPanels((prev) => prev.map((p) => ({ ...p, closing: true })));
    setTimeout(() => setPanels([]), 220);
  }, []);

  // Escape pops the top panel.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && panels.length) pop(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [panels.length, pop]);

  const ctx: StackSheetCtx = { panels, push, pop, close, closeById, clear };
  const closeByIdRef = useRef(closeById);
  useLayoutEffect(() => { closeByIdRef.current = closeById; });

  const desktopPortal = panels.length > 0 && typeof document !== 'undefined'
    ? createPortal(
        <div className="fixed right-4 top-4 bottom-4 z-50 flex flex-row-reverse items-stretch gap-2 pointer-events-none">
          {panels.map((entry, i) => (
            <div key={entry.id} className="pointer-events-auto h-full">
              <PanelCard
                entry={entry}
                isActive={activeIds.has(entry.id)}
                isMain={i === 0}
                onClose={() => closeByIdRef.current(entry.id)}
                onStripClick={() => close(i + 1)}
                onSetWidthStep={(step) => setWidthStep(entry.id, step)}
                totalPanels={panels.length}
              />
            </div>
          ))}
        </div>,
        document.body,
      )
    : null;

  return (
    <StackSheetContext.Provider value={ctx}>
      {children}
      {desktopPortal}
    </StackSheetContext.Provider>
  );
}
