'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type ChartInput } from '@/lib/api';
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
    mutationFn: (input: { name: string; description: string }) =>
      new AgentRayAPI(projectID!).updateDashboard(selectedDashboard!.id, input.name, input.description),
    onSuccess: async () => { setMessage('Dashboard updated.'); await invalidate(); },
  });

  const deleteMutation = useMutation({
    mutationFn: () => new AgentRayAPI(projectID!).deleteDashboard(selectedDashboard!.id),
    onSuccess: async () => {
      setMessage('Dashboard deleted.');
      setSelectedDashboardID('');
      await invalidate();
    },
  });

  const saveChartMutation = useMutation({
    mutationFn: ({ input, chartID }: { input: ChartInput; chartID?: string }) => {
      if (!selectedDashboard) throw new Error('No dashboard selected');
      return chartID
        ? new AgentRayAPI(projectID!).updateChart(chartID, input)
        : new AgentRayAPI(projectID!).createChart(selectedDashboard.id, input);
    },
    onSuccess: async (_, vars) => {
      setMessage(vars.chartID ? 'Chart updated.' : 'Chart created.');
      if (selectedDashboard) await invalidateCharts(selectedDashboard.id);
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to save chart'),
  });

  const deleteChartMutation = useMutation({
    mutationFn: (chartID: string) => new AgentRayAPI(projectID!).deleteChart(chartID),
    onSuccess: async () => {
      setMessage('Chart deleted.');
      if (selectedDashboard) await invalidateCharts(selectedDashboard.id);
    },
  });

  const reorderChartsMutation = useMutation({
    mutationFn: ({ dashboardID, chartIDs }: { dashboardID: string; chartIDs: string[] }) =>
      new AgentRayAPI(projectID!).reorderCharts(dashboardID, chartIDs),
    onSuccess: async (_, vars) => { await invalidateCharts(vars.dashboardID); },
    onError: (err) => setError(err instanceof Error ? err.message : 'Failed to reorder charts'),
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
    updateDashboard: async (input: { name: string; description: string }) => { await updateMutation.mutateAsync(input); },
    deleteDashboard: async () => { await deleteMutation.mutateAsync(); },
    saveChart: async (input: ChartInput, chartID?: string) => { await saveChartMutation.mutateAsync({ input, chartID }); },
    deleteChart: async (chartID: string) => { await deleteChartMutation.mutateAsync(chartID); },
    reorderCharts: async (dashboardID: string, chartIDs: string[]) => { await reorderChartsMutation.mutateAsync({ dashboardID, chartIDs }); },
    saveSQLChart: async (dashboardID: string, input: ChartInput) => { await saveSQLChartMutation.mutateAsync({ dashboardID, input }); },
  };
}
