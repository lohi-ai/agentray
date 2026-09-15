'use client';

import { useState } from 'react';
import { Trash2 } from 'lucide-react';
import { TextInput } from '@astryxdesign/core/TextInput';
import { DateTimeInput, type ISODateTimeString } from '@astryxdesign/core/DateTimeInput';
import { Selector } from '@astryxdesign/core/Selector';
import { apiErrorMessage, type Annotation } from '@/lib/api';
import { Modal, ConfirmDialog } from '@/modules/shared/components/modal';
import { Button } from '@/modules/shared/components/signal-primitives';
import { ANNOTATION_KINDS, fromRFC3339, toRFC3339 } from './annotations';
import type { AnnotationsController } from './use-annotations';

// AnnotationDialog is the one add/delete surface every temporal panel opens:
// validated label + timestamp fields, a kind selector, an optional link, and
// the list of annotations in the panel's window with a confirmed delete. The
// write goes through the shared ops, so a rejected write surfaces the API's
// reason inline rather than closing over the typed values.
export function AnnotationDialog({ annotations, onClose }: { annotations: AnnotationsController; onClose: () => void }) {
  const [label, setLabel] = useState('');
  const [kind, setKind] = useState('deploy');
  const [startsAt, setStartsAt] = useState('');
  const [endsAt, setEndsAt] = useState('');
  const [link, setLink] = useState('');
  const [touched, setTouched] = useState(false);
  const [error, setError] = useState('');
  const [confirming, setConfirming] = useState<Annotation | null>(null);

  const labelMissing = !label.trim();
  const startMissing = !toRFC3339(startsAt);
  const endBad = !!endsAt && !toRFC3339(endsAt);
  const endBeforeStart = !!endsAt && !endBad && !!startsAt && toRFC3339(endsAt) <= toRFC3339(startsAt);
  const canSave = !labelMissing && !startMissing && !endBad && !endBeforeStart && !annotations.add.isPending;

  async function save() {
    setTouched(true);
    if (labelMissing || startMissing || endBad || endBeforeStart) return;
    setError('');
    try {
      await annotations.add.mutateAsync({
        label: label.trim(),
        kind,
        link: link.trim() || undefined,
        starts_at: toRFC3339(startsAt),
        ends_at: endsAt ? toRFC3339(endsAt) : undefined,
      });
      setLabel('');
      setStartsAt('');
      setEndsAt('');
      setLink('');
      setTouched(false);
    } catch (err) {
      setError(apiErrorMessage(err, 'Could not save the annotation'));
    }
  }

  return (
    <Modal
      title="Annotations"
      onClose={onClose}
      footer={<>
        <Button variant="ghost" size="sm" onClick={onClose}>Close</Button>
        <Button variant="primary" size="sm" onClick={() => void save()} disabled={!canSave}>
          {annotations.add.isPending ? 'Saving…' : 'Add annotation'}
        </Button>
      </>}
    >
      <div className="flex flex-col gap-4">
        <TextInput
          label="What changed?"
          isRequired
          status={touched && labelMissing ? { type: 'error', message: 'Name the change — this is the label on the chart.' } : undefined}
          value={label}
          placeholder="e.g. v2.4 deploy"
          onChange={setLabel}
          onEnter={() => void save()}
          width="100%"
        />
        <Selector
          label="Kind"
          size="sm"
          options={ANNOTATION_KINDS}
          value={kind}
          onChange={setKind}
          width="100%"
        />
        <DateTimeInput
          label="When did it start?"
          isRequired
          status={touched && startMissing ? { type: 'error', message: 'Pick the moment the change began.' } : undefined}
          value={(startsAt || undefined) as ISODateTimeString | undefined}
          onChange={(v) => setStartsAt(v ?? '')}
          width="100%"
        />
        <DateTimeInput
          label="When did it end?"
          isOptional
          status={
            touched && endBad ? { type: 'error', message: 'That is not a readable time.' }
              : touched && endBeforeStart ? { type: 'error', message: 'The end has to be after the start — leave it empty for an instant.' }
              : undefined
          }
          value={(endsAt || undefined) as ISODateTimeString | undefined}
          onChange={(v) => setEndsAt(v ?? '')}
          width="100%"
        />
        <TextInput
          label="Link"
          isOptional
          value={link}
          placeholder="https://…"
          onChange={setLink}
          width="100%"
        />
        {error ? <p className="text-sm text-[var(--color-danger)]">{error}</p> : null}

        <div className="border-t border-[var(--color-border)] pt-3">
          <p className="mb-2 text-2xs uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">In this range</p>
          {annotations.annotations.length === 0 ? (
            <p className="text-sm text-[var(--color-text-secondary)]">No annotations in this range.</p>
          ) : (
            <ul className="flex flex-col gap-2">
              {annotations.annotations.map((a) => (
                <li key={a.id} className="flex items-center gap-2 text-sm">
                  <span className="min-w-0 flex-1 truncate">
                    <span className="font-medium">{a.label}</span>
                    <span className="ml-2 text-[var(--color-text-secondary)]">
                      {fromRFC3339(a.starts_at).replace('T', ' ')}{a.ends_at ? ` → ${fromRFC3339(a.ends_at).replace('T', ' ')}` : ''} · {a.kind}
                    </span>
                  </span>
                  <button
                    type="button"
                    aria-label={`Delete annotation ${a.label}`}
                    className="inline-flex min-h-[44px] min-w-[44px] items-center justify-center text-[var(--color-text-secondary)] hover:text-[var(--color-danger)]"
                    onClick={() => setConfirming(a)}
                  >
                    <Trash2 size={14} />
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
      {confirming ? (
        <ConfirmDialog
          title={`Delete “${confirming.label}”?`}
          detail="The mark comes off every chart that shows it. This cannot be undone."
          confirmLabel="Delete"
          danger
          onConfirm={() => annotations.remove.mutate(confirming.id)}
          onClose={() => setConfirming(null)}
        />
      ) : null}
    </Modal>
  );
}
