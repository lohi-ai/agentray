'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type AgentBudgetInput, type BudgetPeriod } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

// useAgentBudget drives the budget bar on the agent setup page: the agent's own
// budget rows, the effective day status (spend + cap incl. workspace default),
// and save/clear mutations that refresh the bar in place.
export function useAgentBudget(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID && !!agentID;

  const query = useQuery({
    queryKey: ['agent-budget', projectID, agentID],
    queryFn: () => client().agentBudgets(agentID),
    enabled,
    staleTime: 15 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['agent-budget', projectID, agentID] });

  const save = useMutation({
    mutationFn: (input: AgentBudgetInput) => client().upsertAgentBudget(agentID, input),
    onSuccess: () => {
      setMessage('Budget saved');
      invalidate();
    },
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to save budget'),
  });

  const clear = useMutation({
    mutationFn: (period: BudgetPeriod) => client().deleteAgentBudget(agentID, period),
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : 'Unable to clear budget'),
  });

  return {
    budgets: query.data?.budgets ?? [],
    status: query.data?.status ?? null,
    budgetLoading: query.isFetching,
    saveBudget: save,
    clearBudget: clear,
  };
}
