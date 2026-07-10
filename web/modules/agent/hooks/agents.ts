'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type Agent } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useAgents() {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const agentsQuery = useQuery({
    queryKey: ['agents', projectID],
    queryFn: () => client().agents(),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['agents', projectID] });
  const onErr = (e: Error) => setError(e.message);

  const createAgent = useMutation({
    mutationFn: (vars: { name: string; slug?: string }) => client().createAgent(vars.name, vars.slug ?? ''),
    onSuccess: () => { setMessage('Agent created'); invalidate(); },
    onError: onErr,
  });

  const updateAgent = useMutation({
    mutationFn: (vars: { id: string; name: string; enabled: boolean }) => client().updateAgent(vars.id, vars.name, vars.enabled),
    onSuccess: () => { setMessage('Agent saved'); invalidate(); },
    onError: onErr,
  });

  const removeAgent = useMutation({
    mutationFn: (id: string) => client().deleteAgent(id),
    onSuccess: () => { setMessage('Agent deleted'); invalidate(); },
    onError: onErr,
  });

  return {
    agents: (agentsQuery.data?.agents ?? []) as Agent[],
    agentsLoading: agentsQuery.isLoading,
    createAgent: (name: string, slug = '') => createAgent.mutateAsync({ name, slug }),
    updateAgent: (id: string, name: string, isEnabled: boolean) => updateAgent.mutateAsync({ id, name, enabled: isEnabled }),
    removeAgent: (id: string) => removeAgent.mutateAsync(id),
  };
}
