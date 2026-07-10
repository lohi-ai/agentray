'use client';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AgentRayAPI, type AgentTriggerInput } from '@/lib/api';
import { useAuthStore, useUIStore } from '@/lib/app-state';

export function useAgentBuild(agentID = '') {
  const projectID = useAuthStore((s) => s.project?.id);
  const setMessage = useUIStore((s) => s.setMessage);
  const setError = useUIStore((s) => s.setError);
  const queryClient = useQueryClient();
  const client = () => new AgentRayAPI(projectID!);
  const enabled = !!projectID;

  const toolsQuery = useQuery({
    queryKey: ['agent-tools', projectID, agentID],
    queryFn: () => client().agentTools(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const secretsQuery = useQuery({
    queryKey: ['agent-secrets', projectID, agentID],
    queryFn: () => client().agentSecrets(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const triggersQuery = useQuery({
    queryKey: ['agent-triggers', projectID, agentID],
    queryFn: () => client().agentTriggers(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const delegatesQuery = useQuery({
    queryKey: ['agent-delegates', projectID, agentID],
    queryFn: () => client().agentDelegates(agentID),
    enabled,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });

  const invalidate = (key: string) => queryClient.invalidateQueries({ queryKey: [key, projectID, agentID] });
  const onErr = (e: Error) => setError(e.message);

  const setTool = useMutation({
    mutationFn: (vars: { name: string; enabled: boolean; config: Record<string, unknown> }) =>
      client().setAgentTool(vars.name, vars.enabled, vars.config, agentID),
    onSuccess: () => { setMessage('Tool saved'); invalidate('agent-tools'); },
    onError: onErr,
  });

  const clearTool = useMutation({
    mutationFn: (name: string) => client().deleteAgentTool(name, agentID),
    onSuccess: () => { setMessage('Tool removed'); invalidate('agent-tools'); },
    onError: onErr,
  });

  const setSecret = useMutation({
    mutationFn: (vars: { name: string; value: string }) => client().setAgentSecret(vars.name, vars.value, agentID),
    onSuccess: () => { setMessage('Secret saved'); invalidate('agent-secrets'); },
    onError: onErr,
  });

  const clearSecret = useMutation({
    mutationFn: (name: string) => client().deleteAgentSecret(name, agentID),
    onSuccess: () => { setMessage('Secret removed'); invalidate('agent-secrets'); },
    onError: onErr,
  });

  const setDelegate = useMutation({
    mutationFn: (vars: { delegateAgentID: string; enabled: boolean }) =>
      client().setAgentDelegate(vars.delegateAgentID, vars.enabled, agentID),
    onSuccess: () => { setMessage('Teammate saved'); invalidate('agent-delegates'); },
    onError: onErr,
  });

  const clearDelegate = useMutation({
    mutationFn: (delegateAgentID: string) => client().deleteAgentDelegate(delegateAgentID, agentID),
    onSuccess: () => { setMessage('Teammate removed'); invalidate('agent-delegates'); },
    onError: onErr,
  });

  const createTrigger = useMutation({
    mutationFn: (input: AgentTriggerInput) => client().createAgentTrigger(input, agentID),
    onSuccess: () => { setMessage('Trigger created'); invalidate('agent-triggers'); },
    onError: onErr,
  });

  const updateTrigger = useMutation({
    mutationFn: (vars: { id: string; input: Omit<AgentTriggerInput, 'kind'> }) =>
      client().updateAgentTrigger(vars.id, vars.input, agentID),
    onSuccess: () => { setMessage('Trigger updated'); invalidate('agent-triggers'); },
    onError: onErr,
  });

  const deleteTrigger = useMutation({
    mutationFn: (id: string) => client().deleteAgentTrigger(id, agentID),
    onSuccess: () => { setMessage('Trigger removed'); invalidate('agent-triggers'); },
    onError: onErr,
  });

  return {
    catalog: toolsQuery.data?.catalog ?? [],
    selections: toolsQuery.data?.selections ?? [],
    toolsLoading: toolsQuery.isLoading,
    secretNames: secretsQuery.data?.names ?? [],
    secretsLoading: secretsQuery.isLoading,
    triggers: triggersQuery.data?.triggers ?? [],
    triggersLoading: triggersQuery.isLoading,
    delegateAgents: delegatesQuery.data?.agents ?? [],
    delegateSelections: delegatesQuery.data?.selections ?? [],
    delegatesLoading: delegatesQuery.isLoading,
    setTool: (name: string, isEnabled: boolean, config: Record<string, unknown>) =>
      setTool.mutateAsync({ name, enabled: isEnabled, config }),
    clearTool: (name: string) => clearTool.mutateAsync(name),
    setDelegate: (delegateAgentID: string, isEnabled: boolean) => setDelegate.mutateAsync({ delegateAgentID, enabled: isEnabled }),
    clearDelegate: (delegateAgentID: string) => clearDelegate.mutateAsync(delegateAgentID),
    setSecret: (name: string, value: string) => setSecret.mutateAsync({ name, value }),
    clearSecret: (name: string) => clearSecret.mutateAsync(name),
    createTrigger: (input: AgentTriggerInput) => createTrigger.mutateAsync(input),
    updateTrigger: (id: string, input: Omit<AgentTriggerInput, 'kind'>) => updateTrigger.mutateAsync({ id, input }),
    deleteTrigger: (id: string) => deleteTrigger.mutateAsync(id),
  };
}
