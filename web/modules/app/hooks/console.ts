'use client';

import { useEffect, useMemo } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useRouter } from 'next/navigation';
import { AgentRayAPI, type AudienceInput, type Filters, type SubscriptionMappingInput } from '@/lib/api';
import { buildIdentityMap } from '@/lib/identity';
import { useAuthStore, useFiltersStore, useUIStore } from '@/lib/app-state';

export function useConsoleQuery() {
  const projectID = useAuthStore((s) => s.project?.id);
  const appliedFilters = useFiltersStore((s) => s.appliedFilters);

  return useQuery({
    queryKey: ['console', projectID, appliedFilters],
    queryFn: async () => {
      const client = new AgentRayAPI(projectID!);
      // Resilient fan-out: a single failing panel endpoint (e.g. a 500 from
      // web-analytics) must degrade only its own panel, not reject the whole
      // aggregate and blank every console-derived surface. Each call falls back
      // to `null` on rejection; consumers already null-check their slice.
      const settle = <T>(p: Promise<T>): Promise<T | null> => p.catch(() => null);
      const [activity, templates, web, persons, explorer, dashboards] = await Promise.all([
        settle(client.activity(appliedFilters)),
        settle(client.templates()),
        settle(client.webAnalytics(appliedFilters)),
        settle(client.persons(appliedFilters)),
        settle(client.exploreEvents(appliedFilters)),
        settle(client.dashboards()),
      ]);
      return { activity, templates, web, persons, explorer, dashboards };
    },
    enabled: !!projectID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });
}

export function useFilters() {
  const { filters, setFilters, commit, reset } = useFiltersStore();
  const consoleQuery = useConsoleQuery();

  async function refresh(nextFilters?: Filters) {
    if (nextFilters) setFilters(nextFilters);
    commit();
  }

  async function resetFilters() {
    reset();
  }

  return {
    filters,
    setFilters,
    refresh,
    resetFilters,
    loading: consoleQuery.isFetching,
  };
}

export function useActivity() {
  const query = useConsoleQuery();
  const setError = useUIStore((s) => s.setError);

  useEffect(() => {
    if (query.error) setError(query.error instanceof Error ? query.error.message : 'Unable to load data');
  }, [query.error, setError]);

  return {
    summary: query.data?.activity?.summary ?? null,
    loading: query.isFetching,
  };
}

export function useWebAnalytics() {
  const query = useConsoleQuery();
  const web = query.data?.web?.web_analytics;
  if (!web) return null;
  // The API sends `null` for these arrays on an empty/fresh project, but the
  // WebAnalytics type (and every consumer) treats them as non-null arrays.
  // Normalize here so callers can `.reduce`/`.slice` without crashing.
  return {
    ...web,
    top_paths: web.top_paths ?? [],
    referrers: web.referrers ?? [],
    traffic_by_class: web.traffic_by_class ?? [],
    traffic_by_provider: web.traffic_by_provider ?? [],
    ai_top_paths: web.ai_top_paths ?? [],
    referrers_by_channel: web.referrers_by_channel ?? [],
  };
}

export function usePersons() {
  const router = useRouter();
  const query = useConsoleQuery();
  const { filters, setFilters, commit } = useFiltersStore();

  async function focusPerson(distinctID: string) {
    router.push('/events');
    setFilters({ ...filters, distinct_id: distinctID });
    commit();
  }

  return {
    persons: query.data?.persons?.persons ?? null,
    focusPerson,
  };
}

export function useExplorer() {
  const query = useConsoleQuery();
  return query.data?.explorer?.explorer ?? null;
}

// useCohorts drives the Cohort Analysis page on its own query, keyed by the
// audience segment so the segment toggle refetches without disturbing the shared
// console fan-out (cohort retention is a heavier, page-specific read). The
// segment is a server-defined audience key (all | user | guest | paid | …).
export function useCohorts(segment: string) {
  const projectID = useAuthStore((s) => s.project?.id);
  const appliedFilters = useFiltersStore((s) => s.appliedFilters);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['cohorts', projectID, segment, appliedFilters],
    queryFn: async () => {
      const client = new AgentRayAPI(projectID!);
      const res = await client.cohorts(appliedFilters, segment);
      return res.cohorts;
    },
    enabled: !!projectID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  useEffect(() => {
    if (query.error) setError(query.error instanceof Error ? query.error.message : 'Unable to load cohorts');
  }, [query.error, setError]);

  return { cohorts: query.data ?? null, loading: query.isFetching };
}

// useCohortAudiences manages a project's custom cohort audiences (the paid /
// premium-style groups users define). On any change it invalidates both its own
// list and every cohorts query so the segment toggle and the active view stay in
// sync with the catalog.
export function useCohortAudiences() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['cohort-audiences', projectID],
    queryFn: () => new AgentRayAPI(projectID!).cohortAudiences(),
    enabled: !!projectID,
  });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['cohort-audiences', projectID] });
    queryClient.invalidateQueries({ queryKey: ['cohorts', projectID] });
  };
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : 'Unable to save audience');

  const create = useMutation({
    mutationFn: (input: AudienceInput) => new AgentRayAPI(projectID!).createCohortAudience(input),
    onSuccess: invalidate,
    onError: fail,
  });
  const update = useMutation({
    mutationFn: (vars: { id: string; input: AudienceInput }) => new AgentRayAPI(projectID!).updateCohortAudience(vars.id, vars.input),
    onSuccess: invalidate,
    onError: fail,
  });
  const remove = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteCohortAudience(id),
    onSuccess: invalidate,
    onError: fail,
  });

  return {
    audiences: query.data?.audiences ?? [],
    loading: query.isLoading,
    busy: create.isPending || update.isPending || remove.isPending,
    create: (input: AudienceInput) => create.mutateAsync(input),
    update: (id: string, input: AudienceInput) => update.mutateAsync({ id, input }),
    remove: (id: string) => remove.mutateAsync(id),
  };
}

// useSubscriptionMapping reads and writes a project's subscription mapping — the
// config that unlocks the point-in-time subscription audience kinds. Saving
// invalidates every cohorts query (the projection changes) and the audiences list
// (the mapping-gated built-in segments appear/disappear).
export function useSubscriptionMapping() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const setError = useUIStore((s) => s.setError);

  const query = useQuery({
    queryKey: ['subscription-mapping', projectID],
    queryFn: () => new AgentRayAPI(projectID!).subscriptionMapping(),
    enabled: !!projectID,
  });

  const save = useMutation({
    mutationFn: (input: SubscriptionMappingInput) => new AgentRayAPI(projectID!).saveSubscriptionMapping(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['subscription-mapping', projectID] });
      queryClient.invalidateQueries({ queryKey: ['cohort-audiences', projectID] });
      queryClient.invalidateQueries({ queryKey: ['cohorts', projectID] });
    },
    onError: (e: unknown) => setError(e instanceof Error ? e.message : 'Unable to save subscription mapping'),
  });

  return {
    mapping: query.data?.mapping ?? null,
    loading: query.isLoading,
    busy: save.isPending,
    save: (input: SubscriptionMappingInput) => save.mutateAsync(input),
  };
}

// useLiveEvents drives the Events table on its own lightweight query so the page
// can poll the raw stream without dragging the whole console payload (activity,
// templates, web, persons, dashboards…) along on every tick. When `live` is on it
// refetches on an interval and on window focus so the table reads as realtime;
// off, it behaves like a normal one-shot fetch. Keyed on the applied filters so
// committing a filter (event name, search, range) refetches here too.
const LIVE_INTERVAL_MS = 5000;
export function useLiveEvents(live: boolean) {
  const projectID = useAuthStore((s) => s.project?.id);
  const appliedFilters = useFiltersStore((s) => s.appliedFilters);
  const query = useQuery({
    queryKey: ['live-events', projectID, appliedFilters],
    queryFn: () => new AgentRayAPI(projectID!).exploreEvents(appliedFilters),
    enabled: !!projectID,
    refetchInterval: live ? LIVE_INTERVAL_MS : false,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: live,
    staleTime: live ? 0 : 30 * 1000,
    placeholderData: (prev) => prev,
  });
  return {
    explorer: query.data?.explorer ?? null,
    loading: query.isLoading,
    fetching: query.isFetching,
    updatedAt: query.dataUpdatedAt,
  };
}

export function useTemplates() {
  const queryClient = useQueryClient();
  const router = useRouter();
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setSelectedDashboardID = useUIStore((s) => s.setSelectedDashboardID);
  const query = useConsoleQuery();

  const applyMutation = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).applyTemplate(id),
    onSuccess: (data) => {
      setMessage(`Applied template: ${data.dashboard.name}`);
      setSelectedDashboardID(data.dashboard.id);
      queryClient.invalidateQueries({ queryKey: ['console', projectID] });
      router.push('/dashboard');
    },
  });

  const cloneChartMutation = useMutation({
    mutationFn: ({ templateID, chartID, dashboardID }: { templateID: string; chartID: string; dashboardID: string }) =>
      new AgentRayAPI(projectID!).cloneTemplateChart(templateID, chartID, dashboardID),
    onSuccess: (_, vars) => {
      setMessage('Chart added to dashboard.');
      queryClient.invalidateQueries({ queryKey: ['charts', projectID, vars.dashboardID] });
    },
  });

  return {
    templates: query.data?.templates?.templates ?? [],
    applyTemplate: async (id: string) => { await applyMutation.mutateAsync(id); },
    cloneChart: async (templateID: string, chartID: string, dashboardID: string) => {
      await cloneChartMutation.mutateAsync({ templateID, chartID, dashboardID });
    },
  };
}

// useMarketplace lists the foundation agent presets and installs one as a real
// agent in the current project, then routes to the new agent so the user lands
// where they can run it.
export function useMarketplace() {
  const queryClient = useQueryClient();
  const router = useRouter();
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);

  const presets = useQuery({
    queryKey: ['marketplace-agents', projectID],
    queryFn: () => new AgentRayAPI(projectID!).marketplaceAgents(),
    enabled: !!projectID,
  });

  const installMutation = useMutation({
    mutationFn: (slug: string) => new AgentRayAPI(projectID!).installAgentPreset(slug),
    onSuccess: (data) => {
      setMessage(`Installed agent: ${data.agent.name}`);
      queryClient.invalidateQueries({ queryKey: ['agents', projectID] });
      // Land on the agent's setup page so the user can finish wiring (enable +
      // model) and run it. There is no bare /agents/[id] index route.
      router.push(`/agents/${data.agent.id}/monitor`);
    },
  });

  return {
    presets: presets.data?.agents ?? [],
    loading: presets.isLoading,
    installing: installMutation.isPending,
    installAgent: async (slug: string) => { await installMutation.mutateAsync(slug); },
  };
}

// useEventNames loads the project's distinct event-name catalog for the
// event-name autocomplete. It spans all history (not the active range) and is
// cached for the session, since the set of names a product emits changes slowly
// — people are looking up a name they can't recall, not a live count.
export function useEventNames() {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['event-names', projectID],
    queryFn: () => new AgentRayAPI(projectID!).eventNames(),
    enabled: !!projectID,
    staleTime: 10 * 60 * 1000,
    refetchOnWindowFocus: false,
  });
  return { names: query.data?.names ?? [], loading: query.isLoading };
}

// useDailyReadout powers the agent-narrated slot on the dashboard home: the
// latest run's plain-language summary (what the agent saw overnight) plus the
// top open recommendations the agent has written. ackRec resolves a card.
export function useDailyReadout() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);

  const query = useQuery({
    queryKey: ['daily-readout', projectID],
    queryFn: async () => {
      const client = new AgentRayAPI(projectID!);
      const [runs, recs] = await Promise.all([client.agentRuns(5), client.agentRecommendations()]);
      return { runs: runs.runs, recommendations: recs.recommendations };
    },
    enabled: !!projectID,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const ackMutation = useMutation({
    mutationFn: ({ id, status, note }: { id: string; status: 'accepted' | 'dismissed'; note?: string }) =>
      new AgentRayAPI(projectID!).ackRecommendation(id, status, note ?? ''),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['daily-readout', projectID] }),
  });

  const runs = query.data?.runs ?? [];
  const latestRun = runs.find((r) => r.summary?.trim()) ?? runs[0] ?? null;
  const openRecs = (query.data?.recommendations ?? []).filter((r) => r.status === 'open');

  return {
    latestRun,
    recentRuns: runs,
    recommendations: openRecs,
    loading: query.isLoading,
    ackRec: async (id: string, status: 'accepted' | 'dismissed', note?: string) => {
      await ackMutation.mutateAsync({ id, status, note });
    },
    acking: ackMutation.isPending,
  };
}

// useAgentGrants powers the "assign this agent to a product" control: it lists
// the workspace's projects and which ones the agent is granted into, and toggles
// a grant. Assigning grants with empty scopes ("no cap") — fine-grained scope
// narrowing per project is a later increment.
export function useAgentGrants(agentID: string) {
  const queryClient = useQueryClient();
  const projects = useAuthStore((s) => s.projects);
  const activeProjectID = useAuthStore((s) => s.project?.id);

  const query = useQuery({
    queryKey: ['agent-grants', agentID],
    queryFn: () => new AgentRayAPI(activeProjectID!).agentGrants(agentID),
    enabled: !!agentID && !!activeProjectID,
  });

  const grant = useMutation({
    mutationFn: (projectID: string) => new AgentRayAPI(projectID).grantAgent(agentID, {}),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['agent-grants', agentID] }),
  });
  const revoke = useMutation({
    mutationFn: (projectID: string) => new AgentRayAPI(projectID).revokeAgent(agentID),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['agent-grants', agentID] }),
  });

  const grantedProjectIDs = new Set((query.data?.grants ?? []).map((g) => g.project_id));

  return {
    projects,
    grantedProjectIDs,
    loading: query.isLoading,
    busy: grant.isPending || revoke.isPending,
    assign: async (projectID: string) => { await grant.mutateAsync(projectID); },
    unassign: async (projectID: string) => { await revoke.mutateAsync(projectID); },
  };
}

export function useInsight() {
  const projectID = useAuthStore((s) => s.project?.id);
  const { appliedFilters } = useFiltersStore();
  const { insight, setInsight } = useUIStore();

  const runMutation = useMutation({
    mutationFn: ({ type, metric, steps }: { type: string; metric: string; steps: string[] }) =>
      new AgentRayAPI(projectID!).insight(type, appliedFilters, metric, steps),
    onSuccess: (data) => setInsight(data.insight),
  });

  return {
    insight,
    setInsight,
    runInsight: async (type: string, metric: string, steps: string[]) => {
      await runMutation.mutateAsync({ type, metric, steps });
    },
  };
}

export function useReplay() {
  const projectID = useAuthStore((s) => s.project?.id);
  const { replay, setReplay } = useUIStore();

  const loadMutation = useMutation({
    mutationFn: (sessionID: string) => new AgentRayAPI(projectID!).agentReplay(sessionID),
    onSuccess: (data) => setReplay(data.replay),
  });

  return {
    replay,
    setReplay,
    loadReplay: async (sessionID: string) => { await loadMutation.mutateAsync(sessionID); },
  };
}

export function useIdentityMap() {
  const query = useConsoleQuery();
  const replay = useUIStore((s) => s.replay);

  return useMemo(() => {
    const summary = query.data?.activity?.summary;
    const explorer = query.data?.explorer?.explorer;
    const persons = query.data?.persons?.persons;

    const map = buildIdentityMap([
      ...(summary?.recent_events || []),
      ...(explorer?.events || []),
      ...(explorer?.timeline || []),
      ...(replay?.events || []),
    ]);
    for (const person of persons?.persons || []) {
      if (!person.email && !person.name) continue;
      map[person.distinct_id] = {
        email: person.email || map[person.distinct_id]?.email,
        name: person.name || map[person.distinct_id]?.name,
      };
    }
    return map;
  }, [query.data, replay]);
}

export function useUIState() {
  const consoleQuery = useConsoleQuery();
  return {
    message: useUIStore((s) => s.message),
    error: useUIStore((s) => s.error),
    loading: consoleQuery.isFetching,
  };
}
