'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Dialog, DialogHeader } from '@astryxdesign/core/Dialog';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Selector } from '@astryxdesign/core/Selector';
import { Button } from './signal-primitives';
import { EventNameCombobox } from './event-name-picker';

// Astryx migration: the shared overlay shell now delegates to Astryx <Dialog>
// (native <dialog>, focus trap, scroll-lock, Escape + backdrop dismiss) with a
// <DialogHeader> for the title and built-in close affordance. The exported API
// (title/onClose/children/footer/wide) is unchanged, so every app dialog
// (PromptDialog, ConfirmDialog, assign-products, chart-editor, …) keeps working.
// Consumers conditionally mount <Modal>, so isOpen is always true while mounted;
// closing flows through onOpenChange. Surfaces/text/borders inherit the bridged
// dark/light tokens, matching the prototype's card-on-dimmed-backdrop language.
export function Modal({ title, onClose, children, footer, wide = false }: { title: string; onClose: () => void; children?: ReactNode; footer?: ReactNode; wide?: boolean }) {
  return (
    <Dialog isOpen onOpenChange={(open) => { if (!open) onClose(); }} width={wide ? 760 : 420} aria-label={title}>
      <DialogHeader title={title} onOpenChange={(open) => { if (!open) onClose(); }} />
      {children ? <div className="py-1">{children}</div> : null}
      {footer ? <div className="mt-3.5 flex justify-end gap-2">{footer}</div> : null}
    </Dialog>
  );
}

export type PromptOption = { value: string; label: string };

// PromptDialog replaces window.prompt: a single text field, plus an optional
// select (for choosing from a known set like chart metrics). Enter submits.
export function PromptDialog({
  title, label, placeholder, defaultValue = '', submitLabel = 'Save', selectLabel, options, eventNameForChoices, eventNameLabel = 'Event name', onSubmit, onClose,
}: {
  title: string;
  label?: string;
  placeholder?: string;
  defaultValue?: string;
  submitLabel?: string;
  selectLabel?: string;
  options?: PromptOption[];
  // When the selected choice is in this set, an event-name autocomplete appears
  // and its value is passed as the third onSubmit arg. Lets a chart that breaks
  // down a single event name pick it from the catalog instead of typing it blind.
  eventNameForChoices?: string[];
  eventNameLabel?: string;
  onSubmit: (value: string, choice: string, eventName: string) => void;
  onClose: () => void;
}) {
  const [value, setValue] = useState(defaultValue);
  const [choice, setChoice] = useState(options?.[0]?.value ?? '');
  const [eventName, setEventName] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => { inputRef.current?.focus(); inputRef.current?.select(); }, []);

  const showEventName = !!eventNameForChoices?.includes(choice);

  function submit() {
    if (!value.trim()) return;
    onSubmit(value.trim(), choice, showEventName ? eventName.trim() : '');
    onClose();
  }

  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant="primary" size="sm" onClick={submit}>{submitLabel}</Button></>}
    >
      <div className="max-w-[440px]" style={{ marginBottom: options ? 14 : 0 }}>
        {label ? <label className="mb-1.5 block text-[12.5px]">{label}</label> : null}
        <TextInput
          ref={inputRef}
          label={label ?? title}
          isLabelHidden
          value={value}
          placeholder={placeholder}
          onChange={(v) => setValue(v)}
          onEnter={submit}
          width="100%"
        />
      </div>
      {options ? (
        <div className="max-w-[440px]">
          {selectLabel ? <label className="mb-1.5 block text-[12.5px]">{selectLabel}</label> : null}
          <Selector
            label={selectLabel ?? 'Choice'}
            isLabelHidden
            size="sm"
            options={options.map((o) => ({ value: o.value, label: o.label }))}
            value={choice}
            onChange={(v) => setChoice(v)}
          />
        </div>
      ) : null}
      {showEventName ? (
        <div className="mt-3.5 max-w-[440px]">
          <label className="mb-1.5 block text-[12.5px]">{eventNameLabel}</label>
          <EventNameCombobox value={eventName} onChange={setEventName} placeholder="Pick an event to chart…" />
        </div>
      ) : null}
    </Modal>
  );
}

// ConfirmDialog replaces window.confirm for destructive actions.
export function ConfirmDialog({ title, detail, confirmLabel = 'Confirm', danger = false, onConfirm, onClose }: { title: string; detail?: string; confirmLabel?: string; danger?: boolean; onConfirm: () => void; onClose: () => void }) {
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={<><Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button><Button variant={danger ? 'agent' : 'primary'} size="sm" onClick={() => { onConfirm(); onClose(); }}><span style={danger ? { color: 'var(--danger)' } : undefined}>{confirmLabel}</span></Button></>}
    >
      {detail ? <p style={{ margin: 0, fontSize: 12.5, color: 'var(--muted-foreground)', lineHeight: 1.5 }}>{detail}</p> : null}
    </Modal>
  );
}
