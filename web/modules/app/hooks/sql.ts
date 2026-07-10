'use client';

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useSavedQueries() {
  const queryClient = useQueryClient();
  const projectID = useAuthStore((s) => s.project?.id);
  const { savedResult, setSavedResult, setMessage } = useUIStore();
  // Saved queries get their own isolated fetch rather than riding the big
  // useConsoleQuery Promise.all — one failing console endpoint (e.g. a 500 from
  // web-analytics) must not blank the SQL screen's saved-query list.
  const query = useQuery({
    queryKey: ['saved-queries', projectID],
    queryFn: () => new AgentRayAPI(projectID!).savedQueries(),
    enabled: !!projectID,
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['saved-queries', projectID] });

  const createMutation = useMutation({
    mutationFn: ({ naturalLanguage, generatedSQL, verified }: { naturalLanguage: string; generatedSQL: string; verified: boolean }) =>
      new AgentRayAPI(projectID!).createSavedQuery(naturalLanguage, generatedSQL, verified),
    onSuccess: () => {
      setMessage('Saved query created.');
      invalidate();
    },
  });

  const runMutation = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).runSavedQuery(id),
    onSuccess: (data) => setSavedResult(data.result),
  });

  const renameMutation = useMutation({
    mutationFn: ({ id, naturalLanguage }: { id: string; naturalLanguage: string }) =>
      new AgentRayAPI(projectID!).renameSavedQuery(id, naturalLanguage),
    onSuccess: () => { setMessage('Saved query renamed.'); invalidate(); },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => new AgentRayAPI(projectID!).deleteSavedQuery(id),
    onSuccess: () => { setMessage('Saved query deleted.'); invalidate(); },
  });

  return {
    savedQueries: query.data?.saved_queries ?? [],
    savedResult,
    createSavedQuery: async (naturalLanguage: string, generatedSQL: string, verified: boolean) => {
      await createMutation.mutateAsync({ naturalLanguage, generatedSQL, verified });
    },
    runSavedQuery: async (id: string) => { await runMutation.mutateAsync(id); },
    renameSavedQuery: async (id: string, naturalLanguage: string) => { await renameMutation.mutateAsync({ id, naturalLanguage }); },
    deleteSavedQuery: async (id: string) => { await deleteMutation.mutateAsync(id); },
    busy: renameMutation.isPending || deleteMutation.isPending,
  };
}

export function useSQL() {
  const projectID = useAuthStore((s) => s.project?.id);
  const { sqlRows, setSQLRows } = useUIStore();
  // Wall-clock of the last successful run, for the "N rows · 0.18s" results meta.
  const [elapsedMs, setElapsedMs] = useState<number | null>(null);

  const runMutation = useMutation({
    mutationFn: async (sql: string) => {
      const t = performance.now();
      const res = await new AgentRayAPI(projectID!).runSQL(sql);
      setElapsedMs(performance.now() - t);
      return res;
    },
    onSuccess: (data) => setSQLRows(data.rows),
  });

  return {
    sqlRows,
    // Fire-and-forget: a failed query surfaces via `error` instead of rejecting,
    // so the page never has to try/catch a run. The old path awaited mutateAsync
    // and left the rejection unhandled, showing the user nothing on failure.
    run: (sql: string) => runMutation.mutate(sql),
    running: runMutation.isPending,
    error: runMutation.isError ? (runMutation.error instanceof Error ? runMutation.error.message : 'Query failed') : null,
    elapsedMs,
    clearError: () => runMutation.reset(),
  };
}
