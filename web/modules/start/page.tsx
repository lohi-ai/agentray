'use client';

import { useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import { jobById, type JobId } from '@/lib/jobs';
import { AppShell } from '@/modules/shared/components/app-shell';
import { Intro, Loading } from '@/modules/shared/components/signal-primitives';
import { JobPicker } from './components/job-picker';
import { JobPlan } from './components/job-plan';
import { useJobBoard } from './hooks';

// /start is the job index: the owner says where their product is, and the page
// resolves that into the teammate to hire, the channel it runs in, and the
// surfaces that hold its answers. The rest of the nav indexes by artifact
// (Dashboards, Events, SQL…), which only helps someone who already knows which
// artifact serves their problem.
export function StartPage() {
  const router = useRouter();
  const searchParams = useSearchParams();
  // The URL is the only place a pick is stored between visits; local state
  // exists so a click lands before the router settles.
  const [clicked, setClicked] = useState<JobId | null>(null);
  const picked = clicked ?? jobById(searchParams.get('job') ?? '')?.id ?? null;

  // The hook derives the active job — an explicit pick, else the one this
  // workspace is actually in once its queries have settled.
  const board = useJobBoard(picked);

  const pick = (id: JobId) => {
    setClicked(id);
    // Shareable and back-button-able: /start?job=operate opens on the health
    // job for whoever it was sent to.
    router.replace(`/start?job=${id}`, { scroll: false });
  };

  return (
    <AppShell>
      <Intro
        title="Where is your product right now?"
        sub="Pick the job. AgentRay hires the teammate, wires the channel, and points it at the evidence."
      />
      {board.job ? (
        <>
          <JobPicker active={board.job.id} state={board.state} onPick={pick} />
          <JobPlan
            job={board.job}
            state={board.state}
            agentID={board.hired?.id ?? ''}
            presets={board.presets}
            installing={board.installing}
            onHire={board.hire}
            validation={board.validation}
            committing={board.committing}
            onCommit={board.commitTest}
            onDecide={board.decideTest}
            apiKey={board.apiKey}
            apiHost={board.apiHost}
          />
        </>
      ) : (
        <Loading label="Reading your workspace…" />
      )}
    </AppShell>
  );
}
