'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, apiErrorMessage, newIdempotencyKey, type ChartInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';
import { useConsoleQuery } from './console';

function useChartsQuery(dashboardID: string) {
  const projectID = useAuthStore((s) => s.project?.id);

  return useQuery({
    queryKey: ['charts', projectID, dashboardID],
    queryFn: () => new AgentRayAPI(projectID!).charts(dashboardID),
    enabled: !!projectID && !!dashboardID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });
}

export function useDashboards() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const { selectedDashboardID, setSelectedDashboardID, setMessage, setError } = useUIStore();
  const query = useConsoleQuery();
  const dashboards = query.data?.dashboards?.dashboards ?? [];
  const selectedDashboard = dashboards.find((d) => d.id === selectedDashboardID) || dashboards[0] || null;
  const chartsQuery = useChartsQuery(selectedDashboard?.id || '');

  function invalidate() {
    return queryClient.invalidateQueries({ queryKey: ['console', projectID] });
  }

  function invalidateCharts(dashboardID: string) {
    return queryClient.invalidateQueries({ queryKey: ['charts', projectID, dashboardID] });
  }

  const createMutation = useMutation({
    mutationFn: (input: { name: string; description: string }) =>
      new AgentRayAPI(projectID!).createDashboard(input.name, input.description),
    onSuccess: async (data) => {
      setMessage('Dashboard created.');
      setSelectedDashboardID(data.dashboard.id);
      await invalidate();
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to create dashboard'),
  });

  const updateMutation = useMutation({
    mutationFn: (input: { name: string; description: string; revision: number; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).updateDashboard(selectedDashboard!.id, input.name, input.description, {
        revision: input.revision,
        idempotencyKey: input.idempotencyKey,
      }),
    onSuccess: async () => { setMessage('Dashboard updated.'); await invalidate(); },
    onError: (err) => setError(apiErrorMessage(err, 'Failed to update dashboard')),
  });

  const deleteMutation = useMutation({
    mutationFn: (input: { revision: number; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).deleteDashboard(selectedDashboard!.id, {
        revision: input.revision,
        idempotencyKey: input.idempotencyKey,
      }),
    onSuccess: async () => {
      setMessage('Dashboard deleted.');
      setSelectedDashboardID('');
      await invalidate();
    },
    onError: (err) => setError(apiErrorMessage(err, 'Failed to delete dashboard')),
  });

  const saveChartMutation = useMutation({
    mutationFn: ({ input, chartID, revision, idempotencyKey }: { input: ChartInput; chartID?: string; revision?: number; idempotencyKey: string }) => {
      if (!selectedDashboard) throw new Error('No dashboard selected');
      return chartID
        ? new AgentRayAPI(projectID!).updateChart(chartID, input, { revision, idempotencyKey })
        : new AgentRayAPI(projectID!).createChart(selectedDashboard.id, input);
    },
    onSuccess: async (_, vars) => {
      setMessage(vars.chartID ? 'Chart updated.' : 'Chart created.');
      if (selectedDashboard) await invalidateCharts(selectedDashboard.id);
    },
    onError: (err) => setError(apiErrorMessage(err, 'Failed to save chart')),
  });

  const deleteChartMutation = useMutation({
    mutationFn: ({ chartID, revision, idempotencyKey }: { chartID: string; revision?: number; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).deleteChart(chartID, { revision, idempotencyKey }),
    onSuccess: async () => {
      setMessage('Chart deleted.');
      if (selectedDashboard) await invalidateCharts(selectedDashboard.id);
    },
    onError: (err) => setError(apiErrorMessage(err, 'Failed to delete chart')),
  });

  const reorderChartsMutation = useMutation({
    mutationFn: ({ dashboardID, chartIDs, revision, idempotencyKey }: { dashboardID: string; chartIDs: string[]; revision?: number; idempotencyKey: string }) =>
      new AgentRayAPI(projectID!).reorderCharts(dashboardID, chartIDs, { revision, idempotencyKey }),
    // reorder bumps the dashboard revision — the board's fence — so the
    // console query (which carries it) must refresh too, or the next reorder
    // sends a stale revision and conflicts.
    onSuccess: async (_, vars) => { await invalidateCharts(vars.dashboardID); await invalidate(); },
    onError: (err) => setError(apiErrorMessage(err, 'Failed to reorder charts')),
  });

  const saveSQLChartMutation = useMutation({
    mutationFn: ({ dashboardID, input }: { dashboardID: string; input: ChartInput }) =>
      new AgentRayAPI(projectID!).createChart(dashboardID, input),
    onSuccess: async (_, vars) => {
      setMessage('Chart saved to dashboard.');
      await invalidateCharts(vars.dashboardID);
    },
  });

  return {
    dashboards,
    selectedDashboard,
    selectedDashboardID: selectedDashboard?.id || '',
    charts: chartsQuery.data?.charts ?? [],
    loading: query.isFetching,
    setSelectedDashboardID: async (id: string) => { setSelectedDashboardID(id); },
    createDashboard: async (input: { name: string; description: string }) => { await createMutation.mutateAsync(input); },
    updateDashboard: async (input: { name: string; description: string }) => {
      await updateMutation.mutateAsync({ ...input, revision: selectedDashboard?.revision ?? 0, idempotencyKey: newIdempotencyKey() });
    },
    deleteDashboard: async () => {
      await deleteMutation.mutateAsync({ revision: selectedDashboard?.revision ?? 0, idempotencyKey: newIdempotencyKey() });
    },
    saveChart: async (input: ChartInput, chartID?: string) => {
      const revision = chartID ? chartsQuery.data?.charts?.find((c) => c.id === chartID)?.revision : undefined;
      await saveChartMutation.mutateAsync({ input, chartID, revision, idempotencyKey: newIdempotencyKey() });
    },
    deleteChart: async (chartID: string) => {
      const revision = chartsQuery.data?.charts?.find((c) => c.id === chartID)?.revision;
      await deleteChartMutation.mutateAsync({ chartID, revision, idempotencyKey: newIdempotencyKey() });
    },
    reorderCharts: async (dashboardID: string, chartIDs: string[]) => {
      const revision = dashboards.find((d) => d.id === dashboardID)?.revision;
      await reorderChartsMutation.mutateAsync({ dashboardID, chartIDs, revision, idempotencyKey: newIdempotencyKey() });
    },
  };
}
