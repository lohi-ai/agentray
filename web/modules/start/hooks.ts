'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, apiBase, type Agent } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { useEventNames, useMarketplace } from '@/modules/app/hooks';
import { useAgents, useWorkspaceModels } from '@/modules/agent/hooks';
import { JOBS, jobById, jobPacks, type JobDef, type JobId, type JobState, suggestedJob } from '@/lib/jobs';

// An installed pack keeps its slug, or the first free numbered variant
// (freeAgentSlug in store/marketplace.go: "ops-watch", "ops-watch-2", …), so a
// pack hired twice still counts as hired. The suffix must be digits: a bare
// `startsWith(slug + '-')` would read "marketing-lead" as an install of a
// hypothetical "marketing" pack and mark a job set up that never was.
export function agentForPack(agents: readonly Agent[], slug: string): Agent | null {
  if (!slug) return null;
  const numbered = new RegExp(`^${slug.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}-\\d+$`);
  return agents.find((a) => a.slug === slug || numbered.test(a.slug)) ?? null;
}

export function installedPacks(agents: readonly Agent[], packs: readonly string[]): string[] {
  return packs.filter((slug) => !!agentForPack(agents, slug));
}

// useJobBoard resolves the whole /start page from what the workspace actually
// looks like. Every query it reads is one another surface already loads, so
// arriving here costs the shared cache a marketplace list at most.
//
// It takes the owner's explicit pick (or null) and *derives* the active job,
// rather than handing the page a suggestion to write back into state: the
// suggestion depends on queries that resolve after mount, and storing it would
// mean a setState in an effect and a cascading render on every settle.
export function useJobBoard(picked: JobId | null) {
  const project = useAuthStore((s) => s.project);
  const projectID = project?.id;
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();

  const { names, loading: namesLoading } = useEventNames();
  const { agents, agentsLoading } = useAgents();
  const { models, modelsLoading } = useWorkspaceModels();
  const { presets } = useMarketplace();

  const roster: Agent[] = agents ?? [];
  const ready = !namesLoading && !agentsLoading;
  // Which job to open on is a question about the WHOLE workspace, not about the
  // job on screen — scoping it to one job's packs would answer "operate?" by
  // looking only at growth's roster.
  const everyPack = JOBS.flatMap((j) => jobPacks(j));
  const suggested = suggestedJob({
    installedPacks: installedPacks(roster, everyPack),
    eventNameCount: names.length,
  });
  // An explicit pick always wins. Without one, stay null until the workspace has
  // loaded so the page never flashes the wrong job and then swaps under a click.
  const job: JobDef | null = jobById(picked ?? '') ?? (ready ? jobById(suggested) : null);
  const hired = job ? agentForPack(roster, job.packs.find((slug) => agentForPack(roster, slug)) ?? '') : null;

  // Only the hired agent's triggers, and only once it exists — the honest
  // signal for "this runs without you", at one request instead of one per
  // agent on the roster.
  const triggers = useQuery({
    queryKey: ['agent-triggers', projectID, hired?.id],
    queryFn: () => new AgentRayAPI(projectID!).agentTriggers(hired!.id),
    enabled: !!projectID && !!hired?.id,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  // Hiring from here must not navigate away: the step list is the context that
  // makes the next step legible, and useMarketplace's install redirects to the
  // agent's monitor page.
  const install = useMutation({
    mutationFn: (slug: string) => new AgentRayAPI(projectID!).installAgentPreset(slug),
    onSuccess: (data) => {
      setMessage(`${data.agent.name} is on your team`);
      queryClient.invalidateQueries({ queryKey: ['agents', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  // The validation test and waitlist, in one request. Only fetched for the job
  // that has them: grow and operate have no pre-product scoreboard, and asking
  // for one there would be a request per page view that nothing renders.
  const validation = useQuery({
    queryKey: ['validation-status', projectID],
    queryFn: () => new AgentRayAPI(projectID!).validationStatus(),
    enabled: !!projectID && job?.id === 'validate',
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  const test = validation.data?.test ?? null;

  const state: JobState = {
    installedPacks: job ? installedPacks(roster, jobPacks(job)) : [],
    eventNameCount: names.length,
    // Undefined while loading, so no step flashes as blocked.
    hasModelKey: modelsLoading ? undefined : !!models?.has_key,
    scheduled: (triggers.data?.triggers ?? []).some((t) => t.kind === 'schedule' && t.enabled),
    testCommitted: test?.status === 'committed',
    testProposed: test?.status === 'proposed',
    waitlistCount: validation.data?.waitlist_count ?? 0,
  };

  const commit = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).commitValidationTest(id),
    onSuccess: () => {
      setMessage('Committed. The number is settled before the data arrives.');
      queryClient.invalidateQueries({ queryKey: ['validation-status', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  const decide = useMutation({
    mutationFn: (v: { id: string; status: string; note: string }) =>
      new AgentRayAPI(projectID!).decideValidationTest(v.id, v.status, v.note),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['validation-status', projectID] });
    },
    onError: (e: Error) => setError(e.message),
  });

  // No `loading` flag: `job` is null exactly while the workspace is unreadable,
  // so the page has one thing to branch on instead of two that can disagree.
  return {
    job,
    state,
    hired,
    presets,
    installing: install.isPending,
    hire: (slug: string) => install.mutate(slug),
    validation: validation.data ?? null,
    // The snippet is only useful with the real key and the real host already in
    // it — an owner who has to substitute two placeholders by hand is an owner
    // who mistypes one and spends an evening wondering why nothing arrives.
    apiKey: project?.api_key ?? '',
    apiHost: apiBase(),
    committing: commit.isPending,
    commitTest: (id: string) => commit.mutate(id),
    decideTest: (id: string, status: string, note: string) => decide.mutate({ id, status, note }),
  };
}
