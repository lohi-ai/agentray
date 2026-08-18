'use client';

import { useRouter } from 'next/navigation';
import { Beaker, Target, TriangleAlert } from 'lucide-react';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import type { MeasuredTest } from '@/lib/api';
import { AppShell } from '@/modules/shared/components/app-shell';
import {
  Button,
  Callout,
  EmptyState,
  Intro,
  Loading,
  Panel,
  StatsStrip,
} from '@/modules/shared/components/signal-primitives';
import { PrototypeCard } from './components/prototype-card';
import { usePrototypes } from './hooks';
import { chatHref, groupTests, stateOf } from './lib/prototype';

// /prototypes — the validate job, run N times.
//
// A prototype is one falsifiable bet: a hypothesis, the success event, the count
// agreed IN ADVANCE, the window, and the verdict. `/start` could only ever hold
// one — it asked the owner to validate their first idea, and then had nowhere to
// put the second. This screen is the same discipline without the LIMIT 1.

const DESIGN_PROMPT = 'Design the cheapest test that could prove my next feature wrong.';

export function PrototypesPage() {
  const router = useRouter();
  const { tests, total, truncated, waitlistCount, isLoading, error, refetch, commit, committingID } = usePrototypes();

  const { waiting, running, decided } = groupTests(tests);
  const passed = tests.filter((t) => stateOf(t) === 'passed').length;
  const failed = tests.filter((t) => stateOf(t) === 'failed').length;

  const open = (t: MeasuredTest) => router.push(`/prototypes/${encodeURIComponent(t.id)}`);
  const act = (t: MeasuredTest) => {
    // The one act this list performs in place is the commit — agreeing to a
    // number is a single yes, and making it a two-page trip invites the owner to
    // read the count first. Everything else opens the test.
    if (stateOf(t) === 'proposed') commit(t.id);
    else open(t);
  };

  return (
    <AppShell active="prototypes">
      <Intro
        title="Prototypes"
        sub="Market the idea before you build it. Paste the snippet on the page, collect the waitlist, and keep the number you agreed to."
        action={
          <Button variant="agent" icon={<Beaker size={15} aria-hidden />} onClick={() => router.push(chatHref(DESIGN_PROMPT))}>
            Talk to Marketing Lead
          </Button>
        }
      />
      <StatsStrip
        stats={[
          { label: 'Waiting on you', value: String(waiting.length), tone: waiting.length ? 'agent' : undefined },
          { label: 'Running', value: String(running.length) },
          { label: 'Passed', value: String(passed), tone: passed ? 'success' : undefined },
          { label: 'Failed', value: String(failed) },
          { label: 'On the waitlist', value: String(waitlistCount) },
        ]}
      />

      {/* A failed read and an empty project must never look the same. This one
          says what broke and what did not: nothing was decided, and no committed
          window stopped counting while the page was blind. */}
      {error ? (
        <Callout
          tone="warn"
          icon={<TriangleAlert size={18} aria-hidden />}
          label="Can’t read"
          title="We couldn’t load your prototypes"
          detail="The API didn’t answer. Nothing has been decided or reset — every committed test is still counting."
          action={
            <Button variant="outline" size="sm" onClick={refetch}>
              Try again
            </Button>
          }
        />
      ) : null}

      {waiting.length > 0 ? (
        <Callout
          tone="agentic"
          icon={<Target size={18} aria-hidden />}
          label="Waiting on you"
          title={
            waiting.length === 1
              ? 'A teammate proposed a test you have not agreed to'
              : `${waiting.length} proposed tests are waiting for you to agree to a number`
          }
          detail="Nothing is counted until you commit. A threshold picked after the result is not a threshold."
          action={
            <Button variant="agent" size="sm" onClick={() => open(waiting[0])}>
              Review it
            </Button>
          }
        />
      ) : null}

      {truncated ? (
        <Text type="supporting" className="mb-3 block">
          Showing the {tests.length} most recent of {total} prototypes — proposals and running tests first.
        </Text>
      ) : null}

      {isLoading && tests.length === 0 ? (
        <Loading label="Reading what you have bet on…" />
      ) : tests.length === 0 ? (
        <Panel title="Prototypes">
          {error ? (
            <EmptyState
              icon={<Beaker size={18} aria-hidden />}
              title="Nothing to show while we’re disconnected"
              detail="This list is empty because the request failed — not because you have no prototypes."
            />
          ) : (
            <EmptyState
              icon={<Beaker size={18} aria-hidden />}
              title="No prototypes yet"
              detail="A prototype is one idea with a kill/keep number on it. Ask a teammate for the cheapest test that could prove your next feature wrong, then commit to the number here."
              action={
                <Button variant="agent" size="sm" onClick={() => router.push(chatHref(DESIGN_PROMPT))}>
                  Design the test
                </Button>
              }
            />
          )}
        </Panel>
      ) : (
        <VStack gap={4} align="stretch">
          {waiting.length > 0 ? (
            <Panel title="Waiting on you">
              <VStack gap={4} align="stretch">
                {waiting.map((t) => (
                  <PrototypeCard
                    key={t.id}
                    test={t}
                    waitlistCount={waitlistCount}
                    busy={committingID === t.id}
                    onOpen={() => open(t)}
                    onAct={() => act(t)}
                  />
                ))}
              </VStack>
            </Panel>
          ) : null}
          <Panel title="Running">
            {running.length === 0 ? (
              <Text type="supporting">Nothing is being measured right now.</Text>
            ) : (
              <VStack gap={4} align="stretch">
                {running.map((t) => (
                  <PrototypeCard
                    key={t.id}
                    test={t}
                    waitlistCount={waitlistCount}
                    busy={committingID === t.id}
                    onOpen={() => open(t)}
                    onAct={() => act(t)}
                  />
                ))}
              </VStack>
            )}
          </Panel>
          {decided.length > 0 ? (
            <Panel title="Decided">
              <VStack gap={4} align="stretch">
                {decided.map((t) => (
                  <PrototypeCard
                    key={t.id}
                    test={t}
                    waitlistCount={waitlistCount}
                    busy={committingID === t.id}
                    onOpen={() => open(t)}
                    onAct={() => act(t)}
                  />
                ))}
              </VStack>
            </Panel>
          ) : null}
        </VStack>
      )}
    </AppShell>
  );
}
