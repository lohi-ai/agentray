'use client';

import { useState } from 'react';
import { CheckCircle2 } from 'lucide-react';
import { Selector } from '@astryxdesign/core/Selector';
import { Text } from '@astryxdesign/core/Text';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { VStack } from '@astryxdesign/core/VStack';
import { HStack } from '@astryxdesign/core/HStack';
import { useUpgradeRequest } from '@/modules/app/hooks';
import { useUser } from '@/modules/app/hooks';
import { Modal } from '@/modules/shared/components/modal';
import { Button } from '@/modules/shared/components/signal-primitives';
import { planByID } from '@/lib/plans';

// Rough monthly volume, in the user's own words. Kept coarse on purpose: an
// exact number is a question they cannot answer before they have instrumented
// anything, and a field nobody can fill is a field nobody submits.
const VOLUMES = [
  { value: 'unsure', label: 'Not sure yet' },
  { value: 'under-1m', label: 'Under 1M events / month' },
  { value: '1m-5m', label: '1M – 5M events / month' },
  { value: '5m-20m', label: '5M – 20M events / month' },
  { value: 'over-20m', label: 'Over 20M events / month' },
];

// UpgradeSheet is the honest CTA while there is no payment processor: an
// interest form that writes a real row, not a fake checkout. The copy says so in
// the first line — a stranger who clicks "Upgrade" and lands on a form deserves
// to be told immediately why there is no card field, rather than hunting for one.
export function UpgradeSheet({ plan, onClose }: { plan: string; onClose: () => void }) {
  const user = useUser();
  const { request, submit, submitting } = useUpgradeRequest();
  const target = planByID(plan);
  const [email, setEmail] = useState(user?.email ?? '');
  const [volume, setVolume] = useState(VOLUMES[0].value);
  const [note, setNote] = useState('');
  const [sent, setSent] = useState(false);

  const emailValid = /.+@.+\..+/.test(email.trim());

  async function send() {
    if (!emailValid || submitting) return;
    await submit({ plan: target.id, email: email.trim(), volume, note: note.trim() });
    setSent(true);
  }

  // Success replaces the form in place rather than closing the dialog out from
  // under the user — they should see that the thing they did landed.
  if (sent || (request && request.plan === target.id && !sent)) {
    const when = request ? new Date(request.created_at) : new Date();
    return (
      <Modal
        title={`${target.name} — you're on the list`}
        onClose={onClose}
        footer={<Button variant="primary" onClick={onClose}>Close</Button>}
      >
        <VStack gap={3} align="start">
          <HStack gap={2} align="center">
            <CheckCircle2 size={18} className="text-success" aria-hidden />
            <Text weight="medium">We have your name.</Text>
          </HStack>
          <Text type="supporting">
            {`Asked on ${when.toLocaleDateString('en-US', { month: 'long', day: 'numeric' })}. We will email ${request?.email || email} when ${target.name} opens — no card, no charge until then, and nothing about your account changes in the meantime.`}
          </Text>
        </VStack>
      </Modal>
    );
  }

  return (
    <Modal
      title={`Move to ${target.name}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Not now</Button>
          <Button variant="primary" disabled={!emailValid || submitting} onClick={() => void send()}>
            {submitting ? 'Sending…' : 'Put me on the list'}
          </Button>
        </>
      }
    >
      <VStack gap={4} align="stretch">
        <Text type="supporting">
          We&rsquo;re not taking cards yet — but we are taking names. Tell us where you are and we&rsquo;ll
          come back to you when {target.name} opens. Nothing changes on your account today.
        </Text>

        <TextInput
          label="Email"
          type="email"
          value={email}
          onChange={(v) => setEmail(v)}
          width="100%"
          status={email.length > 0 && !emailValid ? { type: 'error', message: 'That does not look like an email address.' } : undefined}
        />

        <Selector
          label="Roughly how many events a month?"
          value={volume}
          onChange={(v) => setVolume(v)}
          options={VOLUMES}
          width="100%"
        />

        <TextArea
          label="Anything we should know? (optional)"
          value={note}
          onChange={(v) => setNote(v)}
          placeholder="What would make this worth paying for?"
          rows={3}
          width="100%"
        />
      </VStack>
    </Modal>
  );
}
