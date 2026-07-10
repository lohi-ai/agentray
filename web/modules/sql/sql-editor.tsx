'use client';

import { useMemo } from 'react';
import CodeMirror, { EditorView, keymap, Prec } from '@uiw/react-codemirror';
import { sql, SQLDialect } from '@codemirror/lang-sql';
import { indentWithTab } from '@codemirror/commands';
import type { CompletionContext, CompletionResult } from '@codemirror/autocomplete';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';
import { useEventNames } from '@/modules/app/hooks';
import { EVENTS_COLUMN_NAMES, EVENTS_TABLE } from './events-schema';

// CodeMirror replaces the bare <textarea> on the SQL screen: line numbers, SQL
// syntax highlighting, Tab-to-indent, Cmd/Ctrl+Enter to run, and autocomplete of
// the events table, its columns, and the project's event names. Everything is
// token-themed so it matches the AgentRay dark cockpit (no CodeMirror default
// light chrome leaks through).

// ClickHouse is closest to MySQL's backtick-identifier dialect for the editor's
// tokenizer; we only need keyword/identifier/string highlighting, not exact DDL.
const DIALECT = SQLDialect.define({ backslashEscapes: true });

// Dark syntax palette mapped onto our brand tokens so keywords/strings/numbers
// read against --surface-1 the same way the rest of the cockpit does.
const highlight = HighlightStyle.define([
  { tag: t.keyword, color: 'var(--agent)' },
  { tag: [t.string, t.special(t.string)], color: 'var(--success)' },
  { tag: [t.number, t.bool, t.null], color: 'var(--data)' },
  { tag: [t.function(t.variableName), t.labelName], color: 'var(--data)' },
  { tag: t.comment, color: 'var(--faint)', fontStyle: 'italic' },
  { tag: t.operator, color: 'var(--color-text-secondary)' },
  { tag: [t.propertyName, t.variableName], color: 'var(--color-text-primary)' },
]);

const theme = EditorView.theme(
  {
    '&': { backgroundColor: 'transparent', color: 'var(--color-text-primary)', fontSize: '12.5px' },
    '.cm-content': { fontFamily: 'var(--font-mono)', padding: '10px 0', caretColor: 'var(--primary)' },
    '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--primary)' },
    '&.cm-focused': { outline: 'none' },
    '.cm-gutters': { backgroundColor: 'transparent', color: 'var(--faint)', border: 'none', fontFamily: 'var(--font-mono)' },
    '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--surface-3) 45%, transparent)' },
    '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--color-text-secondary)' },
    '.cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection': {
      backgroundColor: 'color-mix(in srgb, var(--primary) 28%, transparent)',
    },
    '.cm-tooltip': {
      backgroundColor: 'var(--color-background-card)',
      border: '1px solid var(--color-border)',
      borderRadius: 'var(--radius-md)',
      boxShadow: '0 16px 40px rgba(0,0,0,0.35)',
    },
    '.cm-tooltip-autocomplete ul li[aria-selected]': {
      backgroundColor: 'var(--color-background-surface)',
      color: 'var(--color-text-primary)',
    },
    '.cm-tooltip-autocomplete ul li': { fontFamily: 'var(--font-mono)' },
    '.cm-completionDetail': { color: 'var(--color-text-secondary)', fontStyle: 'normal', marginInlineStart: '8px' },
  },
  { dark: true },
);

export function SqlEditor({
  value,
  onChange,
  onRun,
}: {
  value: string;
  onChange: (next: string) => void;
  onRun: () => void;
}) {
  const { names } = useEventNames();

  const extensions = useMemo(() => {
    // Column/table completion driven by the static events schema.
    const language = sql({ dialect: DIALECT, schema: { [EVENTS_TABLE]: EVENTS_COLUMN_NAMES }, upperCaseKeywords: true });

    // Event-name completion: while typing inside a single-quoted string literal,
    // offer the project's known event names so people don't have to recall them.
    // Registered as an extra language-data source so it merges WITH lang-sql's
    // built-in column/table completion rather than replacing it.
    const eventNameSource = (ctx: CompletionContext): CompletionResult | null => {
      const quoted = ctx.matchBefore(/'[^']*/);
      if (!quoted) return null;
      const typed = quoted.text.slice(1).toLowerCase();
      const options = names
        .filter((n) => n.event_name.toLowerCase().includes(typed))
        .slice(0, 50)
        .map((n) => ({ label: n.event_name, type: 'text', detail: n.event_type || undefined, apply: `${n.event_name}'` }));
      if (options.length === 0) return null;
      return { from: quoted.from + 1, options, validFor: /^[^']*$/ };
    };

    // Cmd/Ctrl+Enter runs — highest precedence so it beats CodeMirror defaults.
    const runKey = Prec.highest(
      keymap.of([{ key: 'Mod-Enter', preventDefault: true, run: () => { onRun(); return true; } }]),
    );

    const eventNames = language.language.data.of({ autocomplete: eventNameSource });

    return [language, eventNames, keymap.of([indentWithTab]), runKey, syntaxHighlighting(highlight), theme, EditorView.lineWrapping];
  }, [names, onRun]);

  return (
    <CodeMirror
      value={value}
      onChange={onChange}
      extensions={extensions}
      basicSetup={{ lineNumbers: true, foldGutter: false, highlightActiveLine: true, autocompletion: true, indentOnInput: true, bracketMatching: true }}
      theme="none"
      minHeight="120px"
      style={{ fontSize: '12.5px' }}
    />
  );
}
